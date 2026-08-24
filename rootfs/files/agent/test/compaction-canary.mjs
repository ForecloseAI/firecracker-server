// Does memory survive compaction? This is the path that matters most for
// cracked: we hold ONE query() for the whole process lifetime, so compaction is
// the only thing that refreshes memory mid-session. If SessionStart does not
// fire with source "compact", an agent reads a snapshot taken at boot forever.
//
//   cd rootfs/files/agent && npm run build && node test/compaction-canary.mjs
//
// Drives a real session with a tiny autoCompactWindow until compaction fires.
// Costs a few cents. Not part of `npm test`.
//
// STATUS as of 2026-08-23, SDK 0.3.238: this does NOT pass. Across windows from
// 6,000 to 990,000 and ~46k of accumulated context, compaction never fired and
// the SDK emitted no `autocompact_state` message at all, even though
// applyFlagSettings accepted the call. That is a pre-existing property of
// session.ts's enableCompaction(), not something memory introduced -- but it
// means mid-session memory refresh is unverified. Memory still works at
// startup, and the agent reads the files on demand, so it degrades gracefully.
import { query } from "@anthropic-ai/claude-agent-sdk";
import { mkdtempSync, readFileSync, mkdirSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { fileURLToPath } from "node:url";

const dist = fileURLToPath(new URL("../dist/memory/", import.meta.url));
const { ensureScaffold } = await import(join(dist, "scaffold.js"));
const { deliverySettings } = await import(join(dist, "session-hook.js"));

const WINDOW = Number(process.env.CRACKED_COMPACT_WINDOW ?? 12000);
const base = mkdtempSync(join(tmpdir(), "cracked-compact-"));
const memoryDir = join(base, "memory");
ensureScaffold(memoryDir, join(base, "instructions.md"));
process.env.CRACKED_MEMORY_HOOK_CMD = `node ${join(dist, "hook.js")} ${memoryDir}`;

/** Feeds turns into the SDK's streaming-input mode, as session.ts does. */
class Queue {
  items = [];
  waiter = null;
  push(text) {
    this.items.push(text);
    const w = this.waiter;
    this.waiter = null;
    if (w) w();
  }
  async *stream() {
    for (;;) {
      while (this.items.length === 0) await new Promise((r) => (this.waiter = r));
      yield { type: "user", message: { role: "user", content: this.items.shift() }, parent_tool_use_id: null };
    }
  }
}

/** Every source the hook has recorded, in order. */
function firings() {
  try {
    return readFileSync(join(memoryDir, ".hook-log"), "utf8").trim().split("\n").map((l) => JSON.parse(l).source);
  } catch {
    return [];
  }
}

const queue = new Queue();
const q = query({ prompt: queue.stream(), options: {
  model: process.env.CRACKED_MODEL ?? "claude-sonnet-5",
  systemPrompt: "You are a test harness. Answer briefly.",
  disallowedTools: ["WebFetch", "WebSearch"],
  settings: deliverySettings(),
  settingSources: [],
} });

if (typeof q.applyFlagSettings !== "function") console.log("WARNING: applyFlagSettings is not a function on the query object");
else { await q.applyFlagSettings({ autoCompactEnabled: true, autoCompactWindow: WINDOW }); console.log("applyFlagSettings accepted"); }
console.log(`autoCompactWindow=${WINDOW}; pumping turns until compaction fires\n`);

let turns = 0;
let sawCompact = false;
queue.push("Remember: the agreed passphrase is 'the pelican files a tax return'. Acknowledge in one word.");

for await (const msg of q) {
  if (msg.type === "result") {
    turns++;
    const marker = JSON.parse(readFileSync(join(memoryDir, ".last-hook"), "utf8"));
    const u = msg.usage ?? {};
    const ctx = (u.input_tokens ?? 0) + (u.cache_read_input_tokens ?? 0) + (u.cache_creation_input_tokens ?? 0);
    console.log(`turn ${turns}: ctx=${ctx} hook=${marker.source} firings=${firings().length}`);
    if (marker.source === "compact") { sawCompact = true; break; }
    if (turns >= 25) break;
    // Long filler to burn context fast.
    queue.push(`Turn ${turns}: write three dense paragraphs about the history of the number ${turns}.`);
  }
}

console.log(`\ncompaction fired: ${sawCompact}`);
console.log(`hook firings seen: ${firings().join(", ") || "(see .last-hook only)"}`);
process.exit(sawCompact ? 0 : 1);
