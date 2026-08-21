// Keeps fat browser snapshots out of the conversation.
//
// A take_snapshot result is 6-11k tokens on a content-rich page, and it is
// re-sent on every later turn, so a handful of them dominate cost. The
// PostToolUse hook's updatedToolOutput replaces the result before the model
// sees it: the full text goes to a file, history keeps a digest plus a path.
import { appendFileSync, mkdirSync, readdirSync, statSync, unlinkSync, writeFileSync } from "node:fs";
import { join } from "node:path";

const DIR = process.env.CRACKED_SNAPSHOT_DIR ?? "/home/agent/agent-state/snapshots";
const PASSTHROUGH_CHARS = 2000; // small pages cost less inline than a file read
const DIGEST_LINES = 60;        // interactive elements and refs live near the top
const KEEP_FILES = 20;

let seq = 0;

/** Flatten an MCP tool response into the text the model would have seen.
 *  Observed shape from chrome-devtools-mcp is a bare content-block ARRAY, not
 *  {content:[...]}; getting this wrong silently JSON-stringifies the whole
 *  envelope and the digest ends up full of escape sequences. */
function asText(response: unknown): string {
  if (typeof response === "string") return response;
  const blocks = Array.isArray(response) ? response : (response as { content?: unknown })?.content;
  if (Array.isArray(blocks)) {
    return blocks.map((b) => (b as { text?: string })?.text ?? JSON.stringify(b)).join("\n");
  }
  return JSON.stringify(response ?? "");
}

/** Write the full snapshot to disk and return its path. */
function writeSnapshot(text: string): string {
  mkdirSync(DIR, { recursive: true });
  const path = join(DIR, `snap-${String(++seq).padStart(4, "0")}.txt`);
  writeFileSync(path, text);
  return path;
}

/** Delete all but the newest KEEP_FILES snapshots; these sit on the overlay. */
function rotate(): void {
  let files: string[];
  try {
    files = readdirSync(DIR).filter((f) => f.startsWith("snap-"));
  } catch {
    return;
  }
  if (files.length <= KEEP_FILES) return;
  const byAge = files
    .map((f) => join(DIR, f))
    .sort((a, b) => statSync(b).mtimeMs - statSync(a).mtimeMs);
  for (const stale of byAge.slice(KEEP_FILES)) {
    try {
      unlinkSync(stale);
    } catch {
      /* already gone */
    }
  }
}

/** Head of the snapshot plus where to find the rest. */
function buildDigest(text: string, path: string): string {
  const lines = text.split("\n");
  const head = lines.slice(0, DIGEST_LINES).join("\n");
  return [
    head,
    "",
    `[snapshot truncated: showing ${Math.min(DIGEST_LINES, lines.length)} of ${lines.length} lines]`,
    `[full snapshot: ${path} -- use Read or Grep on it if you need more]`,
  ].join("\n");
}

/** PostToolUse hook. Replaces a large snapshot with a digest plus a file path. */
export async function snapshotToFile(input: unknown): Promise<Record<string, unknown>> {
  const text = asText((input as { tool_response?: unknown })?.tool_response);
  if (text.length <= PASSTHROUGH_CHARS) return {};
  try {
    const path = writeSnapshot(text);
    rotate();
    const original = (input as { tool_response?: unknown })?.tool_response;
    return hookOutput(reshape(original, buildDigest(text, path)));
  } catch (err) {
    logFailure(err);
    return {}; // leave the original result alone rather than lose the snapshot
  }
}

/** Rebuild the replacement in the SAME shape as the original response.
 *  Returning a bare string where the tool returned content blocks is silently
 *  ignored: the file gets written but history keeps the full snapshot. */
function reshape(original: unknown, text: string): unknown {
  if (typeof original === "string") return text;
  if (Array.isArray(original)) return [{ type: "text", text }];
  if (Array.isArray((original as { content?: unknown })?.content)) {
    return { ...(original as object), content: [{ type: "text", text }] };
  }
  return text;
}

/** Wrap the replacement in the shape the SDK expects. */
function hookOutput(updatedToolOutput: unknown): Record<string, unknown> {
  return { hookSpecificOutput: { hookEventName: "PostToolUse", updatedToolOutput } };
}

/** Record a digest failure without taking the turn down with it. */
function logFailure(err: unknown): void {
  try {
    appendFileSync("/tmp/digest-errors.log", `${new Date().toISOString()} ${String(err)}\n`);
  } catch {
    /* nothing useful left to do */
  }
}
