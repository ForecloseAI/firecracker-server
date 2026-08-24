import { test } from "node:test";
import assert from "node:assert/strict";
import { mkdirSync, mkdtempSync, rmSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { renderMemorySection, TRUNCATION_NOTICE } from "../src/memory/context.js";
import { ensureScaffold } from "../src/memory/scaffold.js";

// A lone surrogate with no partner on either side.
const LONE_SURROGATE = /[\uD800-\uDBFF](?![\uDC00-\uDFFF])|(?<![\uD800-\uDBFF])[\uDC00-\uDFFF]/;

/** A memory dir holding the two always-loaded files plus one that must not be. */
function tree(index: string, definition: string): string {
  const dir = join(mkdtempSync(join(tmpdir(), "cracked-ctx-")), "memory");
  mkdirSync(join(dir, "system"), { recursive: true });
  writeFileSync(join(dir, "index.md"), index);
  writeFileSync(join(dir, "system", "definition.md"), definition);
  writeFileSync(join(dir, "system", "index.md"), "SHOULD NOT BE INJECTED");
  return dir;
}

/** Run `fn` with a temporary per-file budget. */
function withBudget(chars: number, fn: () => void): void {
  const previous = process.env.CRACKED_MEMORY_FILE_BUDGET;
  process.env.CRACKED_MEMORY_FILE_BUDGET = String(chars);
  try {
    fn();
  } finally {
    if (previous === undefined) delete process.env.CRACKED_MEMORY_FILE_BUDGET;
    else process.env.CRACKED_MEMORY_FILE_BUDGET = previous;
  }
}

test("inlines the two always-loaded files and nothing else", () => {
  const out = renderMemorySection(tree("MY INDEX", "MY DOCTRINE")) ?? "";
  assert.match(out, /^## Memory/);
  assert.ok(out.includes("MY INDEX"));
  assert.ok(out.includes("MY DOCTRINE"));
  assert.ok(!out.includes("SHOULD NOT BE INJECTED"));
});

test("truncates each file independently, with a notice", () => {
  const dir = tree("x".repeat(200), "y".repeat(200));
  withBudget(20, () => {
    const out = renderMemorySection(dir) ?? "";
    assert.equal(out.split(TRUNCATION_NOTICE).length - 1, 2);
    assert.ok(!out.includes("x".repeat(21)));
  });
});

test("truncation never splits a surrogate pair", () => {
  const dir = tree("\u{1F600}".repeat(20), "short");
  // An odd budget lands the cut between the halves of an emoji.
  withBudget(5, () => {
    assert.doesNotMatch(renderMemorySection(dir) ?? "", LONE_SURROGATE);
  });
});

test("returns undefined when neither file is readable", () => {
  const dir = join(mkdtempSync(join(tmpdir(), "cracked-empty-")), "memory");
  assert.equal(renderMemorySection(dir), undefined);
});

test("degrades to a marker when one file is unreadable", () => {
  const dir = tree("MY INDEX", "MY DOCTRINE");
  // A directory where a file belongs makes readFileSync throw EISDIR. Better
  // than chmod 000, which is a no-op when the tests run as root.
  rmSync(join(dir, "system", "definition.md"));
  mkdirSync(join(dir, "system", "definition.md"));
  const out = renderMemorySection(dir) ?? "";
  assert.ok(out.includes("MY INDEX"));
  assert.ok(out.includes("unavailable during this hook invocation"));
});

test("the shipped templates stay well under the injection budget", () => {
  // This repo cut its fixed prefix from 18,843 tokens to 10,054 on purpose.
  // That budget belongs in a test, not a comment.
  const base = mkdtempSync(join(tmpdir(), "cracked-budget-"));
  const dir = join(base, "memory");
  ensureScaffold(dir, join(base, "instructions.md"));
  assert.ok((renderMemorySection(dir) ?? "").length < 8000);
});
