// The executable Claude Code runs on SessionStart. Reads the hook payload on
// stdin and prints the memory block on stdout; anything it cannot parse yields
// silence, because injecting garbage is worse than injecting nothing.
//
// argv[2] overrides the memory dir, so the tests can run the real binary
// against a temp tree instead of asserting on a mock.
import { readFileSync } from "node:fs";
import * as paths from "./paths.js";
import { contextFor, isHookSource, type HookSource } from "./session-hook.js";

/** Parse {source, session_id} from stdin, or undefined when unusable. */
function readPayload(): { source: HookSource; sessionId: string } | undefined {
  try {
    const raw: unknown = JSON.parse(readFileSync(0, "utf8"));
    if (raw === null || typeof raw !== "object") return undefined;
    const { source, session_id: id } = raw as Record<string, unknown>;
    if (!isHookSource(source)) return undefined;
    return { source, sessionId: typeof id === "string" ? id : "" };
  } catch {
    return undefined;
  }
}

const payload = readPayload();
const section = payload
  ? contextFor(payload.source, payload.sessionId, process.argv[2] ?? paths.memoryDir())
  : undefined;
if (section) console.log(section);
