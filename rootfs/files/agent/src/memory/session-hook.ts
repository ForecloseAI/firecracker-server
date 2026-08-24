// What the SessionStart hook decides, separated from how it is invoked so the
// decision is testable without spawning a process.
import { appendFileSync, readFileSync, writeFileSync } from "node:fs";
import { join } from "node:path";
import * as paths from "./paths.js";
import { renderMemorySection } from "./context.js";

// 'resume' is a deliberate divergence from nanoclaw, which registers only
// startup|clear|compact. Without it the hook never runs on resume, and a VM
// created before memory existed has a valid session.json whose transcript holds
// no memory block -- it would go blind until its first compaction.
// 'fork' comes straight from the SDK's SessionStartHookInput union. We never
// fork today, but registering for it costs one word and stops a future fork
// path from silently getting no memory.
export const HOOK_SOURCES = ["startup", "resume", "fork", "clear", "compact"] as const;
export type HookSource = (typeof HOOK_SOURCES)[number];

export const LEGACY_HOOK_COMMANDS: readonly string[] = [];

const INJECTED = ".injected";
const LAST_HOOK = ".last-hook";
const HOOK_LOG = ".hook-log";

/** True when `value` is one of the sources we registered for. */
export function isHookSource(value: unknown): value is HookSource {
  return typeof value === "string" && (HOOK_SOURCES as readonly string[]).includes(value);
}

/** What buildOptions hands query(): the hook fragment, or nothing at all when
 *  CRACKED_MEMORY=0 switches the subsystem off. */
export function deliverySettings(): Record<string, unknown> {
  return paths.enabled() ? hookSettings() : {};
}

/** The settings fragment that registers our hook. Shared by both delivery
 *  paths: passed inline to query() as `settings`, and written to
 *  ~/.claude/settings.json by the fallback in settings.ts. */
export function hookSettings(): Record<string, unknown> {
  return {
    hooks: {
      SessionStart: [
        {
          matcher: HOOK_SOURCES.join("|"),
          hooks: [{ type: "command", command: paths.hookCommand(), timeout: 10 }],
        },
      ],
    },
  };
}

/** Session id this tree last injected into, or "" when it never has. */
function lastInjected(dir: string): string {
  try {
    return readFileSync(join(dir, INJECTED), "utf8").trim();
  } catch {
    return "";
  }
}

/** Whether this invocation should emit the memory block.
 *
 *  compact ALWAYS injects, bypassing the marker. Do not "simplify" that away:
 *  cracked holds one query() for the whole process lifetime, so compaction is
 *  the only path that refreshes memory mid-session. Gating it would leave the
 *  agent reading a snapshot taken at boot. */
export function shouldInject(
  source: HookSource,
  sessionId: string,
  dir = paths.memoryDir(),
): boolean {
  if (source === "compact") return true;
  if (source !== "resume" && source !== "fork") return true;
  // A resumed or forked transcript already carries the block, unless it was
  // recorded before this VM had memory at all.
  return sessionId === "" || lastInjected(dir) !== sessionId;
}

/** Record that the hook ran, and what it decided. Best-effort: these markers
 *  are how install() proves the hook fired, not part of correctness. */
export function record(
  source: HookSource,
  sessionId: string,
  injected: boolean,
  dir = paths.memoryDir(),
): void {
  const note = { ts: new Date().toISOString(), source, session_id: sessionId, injected };
  try {
    writeFileSync(join(dir, LAST_HOOK), JSON.stringify(note) + "\n");
    // Append-only history too: .last-hook answers "did it fire", HOOK_LOG
    // answers "did it ever fire on compact", which is the question that
    // matters for a session held for the whole process lifetime.
    appendFileSync(join(dir, HOOK_LOG), JSON.stringify(note) + "\n");
    if (injected && sessionId !== "") writeFileSync(join(dir, INJECTED), sessionId);
  } catch {
    /* markers are observability only */
  }
}

/** The memory block for this invocation, or undefined when it should not inject. */
export function contextFor(
  source: HookSource,
  sessionId: string,
  dir = paths.memoryDir(),
): string | undefined {
  const section = shouldInject(source, sessionId, dir) ? renderMemorySection(dir) : undefined;
  record(source, sessionId, section !== undefined, dir);
  return section;
}

/** The last recorded invocation, for the boot-time did-it-fire check. */
export function lastHook(dir = paths.memoryDir()): Record<string, unknown> | undefined {
  try {
    return JSON.parse(readFileSync(join(dir, LAST_HOOK), "utf8")) as Record<string, unknown>;
  } catch {
    return undefined;
  }
}
