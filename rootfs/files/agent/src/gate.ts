// Approval gates. Irreversible actions pause and wait for a human decision;
// everything else runs freely. A batch can be approved once via a consent grant.
import * as events from "./events.js";

type Decision = { decision: "allow" | "deny"; reason?: string; answer?: string };
type Pending = { resolve: (d: Decision) => void; kind: "approval" | "question" };
type Grant = { tool: string; usesRemaining: number; expiresAt: number };

// Someone answering an approval may be away from the screen, but a plain
// question left hanging for half an hour just blocks the agent.
const APPROVAL_TIMEOUT_MS = 30 * 60 * 1000;
const QUESTION_TIMEOUT_MS = 10 * 60 * 1000;

// How the chat page should render a pending interaction.
export type Ui = { kind: "text" | "confirm" | "choice" | "handoff"; options?: string[] };

// Shell commands that destroy data or the machine. Everything else is free.
const DESTRUCTIVE = [
  /\brm\s+-[a-z]*r/, /\bdd\s/, /\bmkfs/, /\bshutdown\b/, /\breboot\b/,
  />\s*\/dev\//, /git\s+push\s+.*--force/, /curl[^|]*\|\s*(sh|bash)/,
];

const pending = new Map<string, Pending>();
let grants: Grant[] = [];
let counter = 0;

/** True when the tool call needs a human decision before it may run. */
function isGated(tool: string, input: Record<string, unknown>): boolean {
  if (tool !== "Bash") return false;
  const cmd = String(input.command ?? "");
  return DESTRUCTIVE.some((re) => re.test(cmd));
}

/** Consume one use of a matching grant, if the caller has one. */
function checkGrant(tool: string): boolean {
  const g = grants.find((x) => x.tool === tool && x.usesRemaining > 0 && x.expiresAt > Date.now());
  if (!g) return false;
  g.usesRemaining -= 1;
  return true;
}

/** Record a batch consent so the next N calls of a tool skip the prompt. */
function createGrant(tool: string, maxUses: number, ttlSeconds: number): Grant {
  const g = { tool, usesRemaining: maxUses, expiresAt: Date.now() + ttlSeconds * 1000 };
  grants.push(g);
  return g;
}

/** Emit a pending decision and block until a human answers or it times out. */
function awaitDecision(
  kind: "approval" | "question",
  payload: Record<string, unknown>,
  timeoutMs = APPROVAL_TIMEOUT_MS,
): Promise<Decision> {
  const id = `${kind === "question" ? "q" : "ap"}_${String(++counter).padStart(3, "0")}`;
  events.append(kind === "question" ? "question" : "approval_required", { approval_id: id, ...payload });
  return new Promise<Decision>((resolve) => {
    pending.set(id, { resolve, kind });
    setTimeout(() => {
      if (pending.delete(id)) resolve({ decision: "deny", reason: "timed out waiting for a human" });
    }, timeoutMs);
  });
}

/** The Agent SDK permission callback. Async, so it can wait on a human. */
export async function canUseTool(tool: string, input: Record<string, unknown>): Promise<
  { behavior: "allow"; updatedInput: Record<string, unknown> } | { behavior: "deny"; message: string }
> {
  if (!isGated(tool, input) || checkGrant(tool)) return { behavior: "allow", updatedInput: input };
  const d = await awaitDecision("approval", { tool, input, preview: describe(tool, input) });
  if (d.decision === "allow") return { behavior: "allow", updatedInput: input };
  return { behavior: "deny", message: `The person declined this. Reason: ${d.reason ?? "none given"}. Do not retry it.` };
}

/** One-line human-readable summary of what is about to happen. */
function describe(tool: string, input: Record<string, unknown>): string {
  if (tool === "Bash") return `Run shell command: ${String(input.command ?? "")}`;
  return `Use ${tool} with ${JSON.stringify(input).slice(0, 200)}`;
}

/** Ask the person something and wait. The ui descriptor drives how the chat
 *  page renders it; handoff is the only way a secret ever gets typed. */
export async function ask(question: string, ui: Ui): Promise<string> {
  const timeout = ui.kind === "handoff" ? APPROVAL_TIMEOUT_MS : QUESTION_TIMEOUT_MS;
  const d = await awaitDecision("question", { question, kind: ui.kind, ui }, timeout);
  return d.answer ?? "";
}

/** Resolve a pending decision from the HTTP layer. */
export function resolve(id: string, body: Record<string, unknown>): boolean {
  const p = pending.get(id);
  if (!p) return false;
  pending.delete(id);
  applyScope(body);
  const decision = (body.decision as "allow" | "deny") ?? (body.answer !== undefined ? "allow" : "deny");
  events.append("decision", { approval_id: id, decision });
  p.resolve({ decision, reason: body.reason as string, answer: body.answer as string });
  return true;
}

/** Create a batch grant when the human approved with scope "batch". */
function applyScope(body: Record<string, unknown>): void {
  if (body.scope !== "batch" || body.decision !== "allow") return;
  createGrant("Bash", Number(body.max_uses ?? 10), Number(body.ttl_seconds ?? 3600));
}

/** Revoke every grant and auto-deny anything pending. Used by interrupt. */
export function revokeAll(): number {
  const n = grants.length;
  grants = [];
  for (const [id, p] of pending) {
    pending.delete(id);
    p.resolve({ decision: "deny", reason: "the person interrupted" });
  }
  return n;
}

/** True when a decision with this id is still waiting. */
export function isPending(id: string): boolean {
  return pending.has(id);
}
