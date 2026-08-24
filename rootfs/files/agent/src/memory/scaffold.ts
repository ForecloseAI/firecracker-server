// Seeds the memory tree and the standing-instructions file from the shipped
// markdown templates. Idempotent: only ever writes what is missing, so the
// agent's own edits survive every later boot.
import { constants, copyFileSync, mkdirSync } from "node:fs";
import { dirname, join } from "node:path";
import * as paths from "./paths.js";

// Template path relative to templates/, and where it lands under the memory dir.
const SEEDS: ReadonlyArray<readonly [string, string]> = [
  ["index.md", "index.md"],
  [join("system", "index.md"), join("system", "index.md")],
  [join("system", "definition.md"), join("system", "definition.md")],
];

/** Copy a template only when the destination is absent. COPYFILE_EXCL makes the
 *  test and the write one syscall, so this stays correct under a race. */
function copyIfMissing(template: string, dest: string): boolean {
  try {
    copyFileSync(join(paths.templatesDir(), template), dest, constants.COPYFILE_EXCL);
    return true;
  } catch (err) {
    if ((err as NodeJS.ErrnoException).code === "EEXIST") return false;
    throw err;
  }
}

/** Create the memory tree and the instructions file if they are not there yet.
 *  Returns the relative paths it actually wrote, for the boot event. */
export function ensureScaffold(
  dir = paths.memoryDir(),
  instructions = paths.instructionsFile(),
): string[] {
  mkdirSync(join(dir, "system"), { recursive: true });
  mkdirSync(dirname(instructions), { recursive: true });
  const wrote = SEEDS.filter(([tpl, rel]) => copyIfMissing(tpl, join(dir, rel)))
    .map(([, rel]) => join("memory", rel));
  if (copyIfMissing("instructions.md", instructions)) wrote.push("instructions.md");
  return wrote;
}
