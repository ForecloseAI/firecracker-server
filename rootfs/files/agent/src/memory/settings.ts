// The FALLBACK delivery path: register the SessionStart hook by writing
// ~/.claude/settings.json.
//
// Not what runs by default. buildOptions passes the same fragment inline as
// `settings` with settingSources: [], which keeps the hook out of a file the
// agent can rewrite. This path exists because it is the one nanoclaw proves on
// the same SDK pin -- if the inline flag tier turns out not to fire hooks,
// switching back is two lines in buildOptions plus install() calling
// syncSessionStartHook(true).
//
// install() still calls this with false, to clear any entry an earlier build
// left behind.
import { mkdirSync, readFileSync, renameSync, writeFileSync } from "node:fs";
import { join } from "node:path";
import * as paths from "./paths.js";
import { LEGACY_HOOK_COMMANDS, hookSettings } from "./session-hook.js";

type Json = Record<string, unknown>;

/** True for a plain JSON object. */
function isRecord(value: unknown): value is Json {
  return value !== null && typeof value === "object" && !Array.isArray(value);
}

/** Parse settings.json, self-healing to {} on anything unreadable or malformed:
 *  a corrupt file must not stop the agent from booting. */
function load(file: string): Json {
  try {
    const parsed: unknown = JSON.parse(readFileSync(file, "utf8"));
    return isRecord(parsed) ? parsed : {};
  } catch {
    return {};
  }
}

/** Strip our command from one SessionStart entry; undefined once it is empty. */
function withoutOurs(entry: unknown, ours: ReadonlySet<string>): unknown {
  if (!isRecord(entry) || !Array.isArray(entry.hooks)) return entry;
  const hooks = entry.hooks.filter(
    (h) => !isRecord(h) || typeof h.command !== "string" || !ours.has(h.command),
  );
  return hooks.length > 0 ? { ...entry, hooks } : undefined;
}

/** The entry we install, taken from the same fragment the inline path uses. */
function ourEntry(): Json {
  const hooks = hookSettings().hooks as { SessionStart: Json[] };
  return hooks.SessionStart[0];
}

/** Write via temp+rename so a crash mid-write cannot leave a truncated file. */
function writeAtomic(file: string, value: Json): void {
  const tmp = `${file}.tmp`;
  writeFileSync(tmp, JSON.stringify(value, null, 2) + "\n");
  renameSync(tmp, file);
}

/** Install or remove our SessionStart entry, idempotently. Every other key in
 *  the file survives untouched: an operator may have put things there. */
export function syncSessionStartHook(install: boolean, dir = paths.claudeConfigDir()): void {
  const file = join(dir, "settings.json");
  mkdirSync(dir, { recursive: true });
  const settings = load(file);
  const hooks = isRecord(settings.hooks) ? settings.hooks : {};
  const ours = new Set([paths.hookCommand(), ...LEGACY_HOOK_COMMANDS]);
  const existing = Array.isArray(hooks.SessionStart) ? hooks.SessionStart : [];
  const kept = existing.map((e) => withoutOurs(e, ours)).filter((e) => e !== undefined);
  if (install) kept.push(ourEntry());
  if (kept.length > 0) hooks.SessionStart = kept;
  else delete hooks.SessionStart;
  settings.hooks = hooks;
  writeAtomic(file, settings);
}
