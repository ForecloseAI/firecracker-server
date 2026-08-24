// Stage 1: does the SessionStart hook actually fire, and does its output reach
// the model? The unit tests cannot prove that, and it is worth a few cents to
// know before paying for a rootfs build.
//
//   cd rootfs/files/agent && npm run build && node test/canary.mjs
//
// Needs a working Anthropic credential. Not part of `npm test`: it costs money
// and needs the network.
import { query } from "@anthropic-ai/claude-agent-sdk";
import { mkdtempSync, readFileSync, statSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { fileURLToPath } from "node:url";
import { globSync } from "node:fs";

const dist = fileURLToPath(new URL("../dist/memory/", import.meta.url));
const src = fileURLToPath(new URL("../src/memory/", import.meta.url));

// A stale dist is the trap here: the canary silently tests the previous build
// and reports a failure that has nothing to do with the current code.
const newestSrc = Math.max(...globSync(join(src, "**/*.ts")).map((f) => statSync(f).mtimeMs));
const oldestDist = Math.min(...globSync(join(dist, "*.js")).map((f) => statSync(f).mtimeMs));
if (!(oldestDist > newestSrc)) {
  console.error("dist is older than src. Run `npm run build` first.");
  process.exit(2);
}

const { ensureScaffold } = await import(join(dist, "scaffold.js"));
const { deliverySettings } = await import(join(dist, "session-hook.js"));

const PHRASE = "the pelican files a tax return";

// A memory tree whose index carries a phrase that exists nowhere else, so the
// only way the model can say it is if the hook put it there.
const base = mkdtempSync(join(tmpdir(), "cracked-canary-"));
const memoryDir = join(base, "memory");
ensureScaffold(memoryDir, join(base, "instructions.md"));
writeFileSync(
  join(memoryDir, "index.md"),
  `---\nokf_version: "0.1"\n---\n\n# Memory Index\n\n## Core Memory\n\nThe agreed passphrase is: ${PHRASE}\n`,
);

process.env.CRACKED_MEMORY_HOOK_CMD = `node ${join(dist, "hook.js")} ${memoryDir}`;

let answer = "";
for await (const msg of query({
  prompt: "What is the agreed passphrase? Answer with the phrase only.",
  options: {
    model: process.env.CRACKED_MODEL ?? "claude-sonnet-5",
    systemPrompt: "You are a test harness. Answer from your memory context.",
    disallowedTools: ["WebFetch", "WebSearch"],
    // Exactly what buildOptions ships.
    settings: deliverySettings(),
    settingSources: [],
  },
})) {
  if (msg.type === "assistant") {
    for (const block of msg.message?.content ?? []) {
      if (block.type === "text") answer += block.text;
    }
  }
}

let marker = "(never ran)";
try {
  marker = readFileSync(join(memoryDir, ".last-hook"), "utf8").trim();
} catch { /* stays as the failure message */ }

const echoed = answer.toLowerCase().includes(PHRASE);
console.log(`hook marker: ${marker}`);
console.log(`model said : ${answer.trim().slice(0, 200)}`);
console.log(`\n${echoed ? "PASS" : "FAIL"}: memory ${echoed ? "reached" : "did NOT reach"} the model`);
if (!echoed) {
  console.log("\nFallback: deliver through the settings file instead. In buildOptions");
  console.log('use settingSources: ["user"] and drop `settings`, and have install()');
  console.log("call syncSessionStartHook(true). That is the path nanoclaw proves.");
}
process.exit(echoed ? 0 : 1);
