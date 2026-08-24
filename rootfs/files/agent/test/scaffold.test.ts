import { test } from "node:test";
import assert from "node:assert/strict";
import { existsSync, mkdtempSync, readFileSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { ensureScaffold } from "../src/memory/scaffold.js";

/** A fresh temp root per case. */
function root(): { dir: string; instructions: string } {
  const base = mkdtempSync(join(tmpdir(), "cracked-scaffold-"));
  return { dir: join(base, "memory"), instructions: join(base, "instructions.md") };
}

test("seeds the whole tree on an empty dir", () => {
  const { dir, instructions } = root();
  const wrote = ensureScaffold(dir, instructions);
  assert.deepEqual(wrote.sort(), [
    "instructions.md",
    "memory/index.md",
    "memory/system/definition.md",
    "memory/system/index.md",
  ]);
  assert.ok(existsSync(join(dir, "index.md")));
  assert.ok(existsSync(join(dir, "system", "definition.md")));
  assert.ok(existsSync(instructions));
});

test("shipped templates carry the OKF contract", () => {
  const { dir, instructions } = root();
  ensureScaffold(dir, instructions);
  assert.match(readFileSync(join(dir, "index.md"), "utf8"), /okf_version: "0\.1"/);
  assert.match(readFileSync(join(dir, "system", "definition.md"), "utf8"), /^---\ntype: system\n---/);
  assert.match(readFileSync(join(dir, "system", "index.md"), "utf8"), /\[Definition\]\(definition\.md\)/);
});

test("never clobbers what the agent wrote", () => {
  const { dir, instructions } = root();
  ensureScaffold(dir, instructions);
  writeFileSync(join(dir, "index.md"), "EDITED BY THE AGENT");
  const wrote = ensureScaffold(dir, instructions);
  assert.deepEqual(wrote, []);
  assert.equal(readFileSync(join(dir, "index.md"), "utf8"), "EDITED BY THE AGENT");
});
