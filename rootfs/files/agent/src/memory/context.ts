// Renders the always-loaded memory files into the block the SessionStart hook
// prints. Pure: reads two files, writes nothing, never throws.
import { readFileSync } from "node:fs";
import { join } from "node:path";
import * as paths from "./paths.js";

export const TRUNCATION_NOTICE =
  "[truncated: slim this file and move detail into linked memory files]";
const UNAVAILABLE = "(unavailable during this hook invocation)";

/** Read a file capped at `budget` characters, never splitting a surrogate pair.
 *  Undefined when the file cannot be read at all. */
export function readCapped(file: string, budget = paths.fileBudget()): string | undefined {
  let content: string;
  try {
    content = readFileSync(file, "utf8").trim();
  } catch {
    return undefined;
  }
  if (content.length <= budget) return content;
  let cut = content.slice(0, budget);
  const last = cut.charCodeAt(cut.length - 1);
  // A lone high surrogate would render as a replacement char downstream.
  if (last >= 0xd800 && last <= 0xdbff) cut = cut.slice(0, -1);
  return `${cut}\n${TRUNCATION_NOTICE}`;
}

/** Preamble naming the two files and where they really live. */
function header(dir: string): string[] {
  return [
    "## Memory",
    "",
    "These files are loaded when a context window is created, at startup and",
    "after compaction:",
    "",
    `- \`${join(dir, "index.md")}\` - top-level memory index and Core Memory`,
    `- \`${join(dir, "system", "definition.md")}\` - how this memory works`,
    "",
    "The files on disk are authoritative. Edit them directly, and follow links",
    "from the index when more detail is relevant.",
    "",
    "`memory/` is an Open Knowledge Format (OKF) v0.1 bundle: one Markdown",
    "concept per file, opened by a short YAML frontmatter with a `type`",
    "(`index.md` and `log.md` are exempt; see the definition).",
    "",
  ];
}

/** The ## Memory block, or undefined when neither always-loaded file is
 *  readable -- a broken install should inject nothing rather than a block that
 *  says everything is unavailable. */
export function renderMemorySection(dir = paths.memoryDir()): string | undefined {
  const index = readCapped(join(dir, "index.md"));
  const definition = readCapped(join(dir, "system", "definition.md"));
  if (index === undefined && definition === undefined) return undefined;
  return [
    ...header(dir),
    "### memory/index.md",
    "",
    index ?? UNAVAILABLE,
    "",
    "### memory/system/definition.md",
    "",
    definition ?? UNAVAILABLE,
    "",
  ].join("\n");
}
