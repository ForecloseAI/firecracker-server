// Agent SDK session: one long-lived streaming-input conversation held for the
// process lifetime, so context is retained and history is never re-sent.
//
// TODO(security): the Anthropic API key currently lives inside this VM. It is
// shared by every VM built from this image, readable by anything in the guest,
// and this agent browses untrusted pages with it present, so a prompt injection
// can exfiltrate it. Rotating it means rebuilding the rootfs. Before any public
// release, replace this with a host token-broker: the agent calls a proxy on its
// own gateway IP and the host injects the real key, so the guest never holds a
// credential. Note the gateway is not fixed -- slot N is 172.16.(4N+1) -- so it
// must be derived from the default route.
import { query } from "@anthropic-ai/claude-agent-sdk";
import { existsSync, readFileSync, writeFileSync, mkdirSync } from "node:fs";
import { dirname } from "node:path";
import * as events from "./events.js";
import * as gate from "./gate.js";
import * as digest from "./digest.js";
import { humanServer } from "./human.js";
import { composeSystemPrompt } from "./prompt.js";
import { deliverySettings } from "./memory/session-hook.js";

const STATE = process.env.CRACKED_SESSION_FILE ?? "/home/agent/agent-state/session.json";
const CDP_URL = "http://127.0.0.1:9222";

// Browser tools that never need a prompt. Reading and navigating are safe; the
// gate still sees anything not listed here.
//
// These cost no extra tokens: `tools` pulls in the whole mcp__chrome server, so
// every schema is already loaded whether or not it is listed here. What this
// list decides is only whether a call skips canUseTool.
const BROWSER_TOOLS = [
  "navigate_page", "take_snapshot", "click", "fill", "fill_form",
  "press_key", "wait_for", "take_screenshot", "evaluate_script", "list_pages",
  // handle_dialog is the one that unwedges a session: a JS alert() blocks the
  // page and every later CDP command until it is dismissed.
  "handle_dialog",
  // Tab management. list_pages alone was useless -- the agent could enumerate
  // tabs but not act on one, and checkout and OAuth flows routinely open them.
  "new_page", "select_page", "close_page",
  // drag covers sliders and reorderable lists; upload_file covers any
  // attach-a-document flow, which otherwise dead-ends at a human handoff.
  "drag", "upload_file",
];

// allowedTools means "auto-run without prompting", NOT "restrict the surface"
// (that is the `tools` option). Anything listed here bypasses canUseTool
// entirely, so Bash, Write and Edit are deliberately absent: they must reach the
// gate, which then allows all but the destructive ones.
// The Task tools are here rather than in GATED because they touch nothing
// outside the model's own scratch state. They are deliberately absent from
// beats.go, so their calls never reach the chat transcript: task tracking is a
// planning aid for the agent, not something the person sees.
//
// NOT TodoWrite. That tool does not exist in the SDK any more -- only a stale
// TodoWriteInput type in sdk-tools.d.ts survives -- and an unknown name in
// `tools` is silently ignored, so the mistake costs nothing and reports nothing.
// Verified against 0.3.241: with no `tools` restriction at all the surface is
// Agent, Bash, Edit, ListAgents, Read, ReportFindings, ScheduleWakeup, Skill,
// ToolSearch, Workflow, Write.
const AUTO_ALLOWED = ["Read", "Glob", "Grep", "TaskCreate", "TaskUpdate", "TaskList"];
const GATED = ["Bash", "Write", "Edit"];

let queue: MessageQueue | null = null;
let running: { interrupt: () => Promise<unknown> } | null = null;
let state: "starting" | "idle" | "working" | "paused_for_human" = "starting";

/** Queue that feeds user messages into the SDK's streaming input mode. */
class MessageQueue {
  private items: string[] = [];
  private waiter: ((v: void) => void) | null = null;

  /** Hand a new user message to the running conversation. */
  push(text: string): void {
    this.items.push(text);
    const w = this.waiter;
    this.waiter = null;
    if (w) w();
  }

  /** Yield messages as they arrive, blocking while the queue is empty. */
  async *stream(): AsyncGenerator<Record<string, unknown>> {
    for (;;) {
      while (this.items.length === 0) {
        await new Promise<void>((r) => (this.waiter = r));
      }
      const text = this.items.shift() as string;
      yield { type: "user", message: { role: "user", content: text }, parent_tool_use_id: null };
    }
  }
}

/** Read the persisted session id so a restart continues the conversation. */
function loadSessionId(): string | undefined {
  if (!existsSync(STATE)) return undefined;
  try {
    return JSON.parse(readFileSync(STATE, "utf8")).session_id as string;
  } catch {
    return undefined;
  }
}

/** Persist the session id to the overlay for the next restart. */
function saveSessionId(id: string): void {
  mkdirSync(dirname(STATE), { recursive: true });
  writeFileSync(STATE, JSON.stringify({ session_id: id }));
}

/** Build the SDK options, including the browser MCP server and the gate. */
function buildOptions(resume: string | undefined): Record<string, unknown> {
  return {
    model: process.env.CRACKED_MODEL ?? "claude-sonnet-5",
    cwd: "/home/agent/workspace",
    systemPrompt: composeSystemPrompt(),
    // `tools` restricts what EXISTS; `allowedTools` only decides what runs
    // without prompting. Without this the SDK loads ~25 built-ins we never use
    // (Task, NotebookEdit, ToolSearch...), which measured as two thirds of the
    // fixed per-turn prefix: 54 tools and 18,843 tokens, against 28 tools and
    // 10,054 tokens with it. The three Task tools were later added back
    // deliberately, for multi-step task tracking.
    tools: [...AUTO_ALLOWED, ...GATED, "mcp__chrome", "mcp__human"],
    allowedTools: [...AUTO_ALLOWED, ...BROWSER_TOOLS.map((t) => `mcp__chrome__${t}`),
      "mcp__human__ask_human"],
    disallowedTools: ["WebFetch", "WebSearch"],
    mcpServers: { chrome: chromeServerConfig(), human: humanServer },
    canUseTool: (tool: string, input: Record<string, unknown>) => gate.canUseTool(tool, input),
    hooks: buildHooks(),
    // The memory SessionStart hook, delivered inline. `settings` is the flag
    // tier, the highest-priority user-controlled layer, so the hook is compiled
    // into the image rather than living in a file the agent can rewrite.
    settings: deliverySettings(),
    // SDK isolation mode: load NO filesystem settings.
    //
    // Omitting this option is NOT "load nothing" -- it means load user AND
    // project AND local (verified against the SDK's own settings resolver).
    // Project and local resolve under cwd, which the agent writes with
    // auto-approved Write and Edit, so the default would let a prompt injection
    // off a web page register a PreToolUse command that runs on every later
    // tool call, persists on the overlay, and never appears in events.jsonl.
    // Passing [] closes that, and it is what this VM had in practice anyway:
    // the image ships no settings files.
    settingSources: [],
    // Claude Code's own auto-memory would compete with the file tree for the
    // same job and the same tokens.
    env: { ...process.env, CLAUDE_CODE_DISABLE_AUTO_MEMORY: "1" },
    resume,
    includePartialMessages: false,
  };
}

/** Redirect fat snapshots to disk before they enter history. See digest.ts. */
function buildHooks(): Record<string, unknown> {
  return {
    PostToolUse: [{ matcher: "mcp__chrome__take_snapshot", hooks: [digest.snapshotToFile] }],
  };
}

/** Stdio MCP server that attaches to the Chrome the human can see. */
function chromeServerConfig(): Record<string, unknown> {
  return {
    type: "stdio",
    command: "node",
    args: [
      process.env.CRACKED_CDP_MCP ?? "/opt/agent/node_modules/chrome-devtools-mcp/build/src/bin/chrome-devtools-mcp.js",
      "--browserUrl", CDP_URL,
      // Dead weight for this agent, and their schemas are charged every turn.
      "--no-category-performance", "--no-category-network", "--no-category-emulation",
    ],
  };
}

/** Start the conversation and pump its messages into the event log forever. */
export async function start(): Promise<void> {
  queue = new MessageQueue();
  const resume = loadSessionId();
  const q = query({ prompt: queue.stream() as never, options: buildOptions(resume) as never });
  running = q as never;
  await enableCompaction(q as never);
  state = "idle";
  events.append("ready", { resumed: Boolean(resume) });
  void pump(q as AsyncGenerator<Record<string, unknown>>);
}

/** Compaction threshold. Overridable because compaction is the only path that
 *  refreshes memory inside a session we hold for the whole process lifetime,
 *  and testing that at the real value costs 300k tokens. */
function compactWindow(): number {
  const raw = Number(process.env.CRACKED_COMPACT_WINDOW);
  return Number.isFinite(raw) && raw > 0 ? raw : 300_000;
}

/** Bound history growth. The explicit window is the point: Sonnet 5 has a 1M
 *  context, so the default threshold would not fire until a session had already
 *  cost a fortune to re-read. This is a ceiling, not a target. */
async function enableCompaction(q: { applyFlagSettings?: (s: Record<string, unknown>) => Promise<void> }): Promise<void> {
  try {
    await q.applyFlagSettings?.({ autoCompactEnabled: true, autoCompactWindow: compactWindow() });
  } catch (err) {
    events.append("error", { message: `could not enable autocompaction: ${String(err)}` });
  }
}

/** Translate SDK messages into SSE events. Never throws into the caller. */
async function pump(q: AsyncGenerator<Record<string, unknown>>): Promise<void> {
  try {
    for await (const msg of q) emit(msg);
  } catch (err) {
    events.append("error", { message: String(err) });
    state = "idle";
  }
}

/** Map one SDK message onto the event log. */
function emit(msg: Record<string, unknown>): void {
  const type = String(msg.type);
  if (type === "system" && msg.session_id) saveSessionId(String(msg.session_id));
  if (type === "assistant") return emitAssistant(msg);
  if (type === "result") return emitResult(msg);
}

/** Emit text and tool-use blocks from an assistant message. */
function emitAssistant(msg: Record<string, unknown>): void {
  state = "working";
  const message = msg.message as { content?: unknown[] } | undefined;
  for (const block of message?.content ?? []) {
    const b = block as Record<string, unknown>;
    if (b.type === "text") events.append("text", { text: b.text });
    if (b.type === "tool_use") events.append("tool_use", { tool: b.name, input: b.input });
  }
}

/** Emit the end-of-turn result plus the usage numbers we measure against. */
function emitResult(msg: Record<string, unknown>): void {
  state = "idle";
  events.append("usage", { cost_usd: msg.total_cost_usd, usage: msg.usage, duration_ms: msg.duration_ms });
  events.append("turn_complete", { is_error: Boolean(msg.is_error) });
}

/** Queue a user message for the running conversation. */
export function send(text: string): void {
  if (!queue) throw new Error("session not started");
  state = "working";
  queue.push(text);
}

/** Interrupt the current turn and revoke every outstanding consent grant. */
export async function interrupt(): Promise<number> {
  const revoked = gate.revokeAll();
  if (running) await running.interrupt().catch(() => undefined);
  state = "paused_for_human";
  events.append("state", { session_state: state });
  return revoked;
}

/** Current session state, surfaced on /health and message responses. */
export function currentState(): string {
  return state;
}
