// Every path and tunable the memory system reads, in one place.
//
// Resolved at call time rather than at import time, unlike events.ts and
// digest.ts: the off-VM tests retarget the whole subsystem at a temp directory
// per test case, and module-level constants would freeze the first value.
import { fileURLToPath } from "node:url";
import { dirname, join } from "node:path";

const STATE_DIR = "/home/agent/agent-state";
const DEFAULT_FILE_BUDGET = 16_000;

/** Root of the persisted memory tree. Sits on the overlay, so it survives
 *  DELETE and dies with the workspace on ?purge=true. */
export function memoryDir(): string {
  return process.env.CRACKED_MEMORY_DIR ?? join(STATE_DIR, "memory");
}

/** The agent-editable standing instructions spliced into the system prompt. */
export function instructionsFile(): string {
  return process.env.CRACKED_INSTRUCTIONS_FILE ?? join(STATE_DIR, "instructions.md");
}

/** Where the seed markdown lives. Next to this module, because the build copies
 *  templates/ into dist alongside it -- tsc does not do that on its own. */
export function templatesDir(): string {
  return process.env.CRACKED_MEMORY_TEMPLATES ??
    join(dirname(fileURLToPath(import.meta.url)), "templates");
}

/** Per-file injection cap, in characters. */
export function fileBudget(): number {
  const raw = Number(process.env.CRACKED_MEMORY_FILE_BUDGET);
  return Number.isFinite(raw) && raw > 0 ? raw : DEFAULT_FILE_BUDGET;
}

/** The command Claude Code runs for the hook. Overridable so the Stage-1 canary
 *  can point it at a local build instead of the guest's /opt/agent. */
export function hookCommand(): string {
  return process.env.CRACKED_MEMORY_HOOK_CMD ?? "node /opt/agent/dist/memory/hook.js";
}

/** Claude Code's config dir, where the SessionStart hook gets registered. */
export function claudeConfigDir(): string {
  return process.env.CLAUDE_CONFIG_DIR ?? join(process.env.HOME ?? "/home/agent", ".claude");
}

/** False when CRACKED_MEMORY=0 disables the subsystem for this VM. That is the
 *  per-VM rollback: install() then REMOVES its hook instead of writing one. */
export function enabled(): boolean {
  return process.env.CRACKED_MEMORY !== "0";
}
