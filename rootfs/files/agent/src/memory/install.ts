// Boot-time installation of the memory subsystem.
//
// Never throws. A memory failure must degrade to "an agent with no memory",
// never to "an agent that did not boot".
//
// TODO(security): ~/.claude/settings.json and ~/.claude/CLAUDE.md both sit on
// the overlay, are writable by the agent, and are live because buildOptions
// passes settingSources: ["user"]. That is not a new capability -- Bash is
// already available -- but it is new PERSISTENCE with no trace in events.jsonl,
// because neither is a tool_use. A root-owned
// /etc/claude-code/managed-settings.json would close it if the bundled CLI
// honours it; that is unverified. Same register as the API-key TODO in
// session.ts: stated, bounded, deferred.
import * as events from "../events.js";
import * as paths from "./paths.js";
import { ensureScaffold } from "./scaffold.js";
import { syncSessionStartHook } from "./settings.js";
import { lastHook } from "./session-hook.js";

// How long after boot to check whether Claude Code actually ran our hook.
const FIRE_CHECK_MS = 20_000;

/** A hook that never fires is invisible: the agent simply behaves as it did
 *  before memory existed. Put the answer in the event log instead. */
function scheduleFireCheck(): void {
  const timer = setTimeout(() => {
    const last = lastHook();
    const message = last
      ? `session-start hook fired: ${JSON.stringify(last)}`
      : "session-start hook never fired";
    events.append("memory", { message });
  }, FIRE_CHECK_MS);
  timer.unref();
}

/** Seed the tree and report what happened. The hook itself is delivered inline
 *  by buildOptions, so nothing is registered on disk here. */
export function install(): void {
  try {
    // Clear any entry an earlier build wrote. With settingSources: [] it is
    // already inert, but leaving it would double-register the hook the moment
    // anyone switches to the settings.ts fallback.
    syncSessionStartHook(false);
    if (!paths.enabled()) {
      events.append("memory", { message: "memory disabled by CRACKED_MEMORY=0" });
      return;
    }
    const wrote = ensureScaffold();
    const message = wrote.length > 0 ? `memory scaffolded: ${wrote.join(", ")}` : "memory ready";
    events.append("memory", { message });
    scheduleFireCheck();
  } catch (err) {
    events.append("memory", { message: `memory install failed: ${String(err)}` });
  }
}
