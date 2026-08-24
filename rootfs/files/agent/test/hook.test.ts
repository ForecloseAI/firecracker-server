import { test } from "node:test";
import assert from "node:assert/strict";
import { spawnSync } from "node:child_process";
import { mkdtempSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { fileURLToPath } from "node:url";
import { ensureScaffold } from "../src/memory/scaffold.js";

// The real compiled hook, run the way Claude Code runs it.
const HOOK = fileURLToPath(new URL("../src/memory/hook.js", import.meta.url));

/** A seeded memory tree. */
function seeded(): string {
  const base = mkdtempSync(join(tmpdir(), "cracked-hook-"));
  const dir = join(base, "memory");
  ensureScaffold(dir, join(base, "instructions.md"));
  return dir;
}

/** Invoke the hook with a raw stdin payload. */
function run(payload: string, dir: string): { status: number | null; stdout: string } {
  const r = spawnSync(process.execPath, [HOOK, dir], { input: payload, encoding: "utf8" });
  return { status: r.status, stdout: r.stdout };
}

/** Invoke the hook with a well-formed payload. */
function fire(source: string, sessionId: string, dir: string) {
  return run(JSON.stringify({ source, session_id: sessionId }), dir);
}

test("startup injects the memory block", () => {
  const out = fire("startup", "s1", seeded());
  assert.equal(out.status, 0);
  assert.ok(out.stdout.includes("## Memory"));
});

test("resume injects once per session, then stays quiet", () => {
  const dir = seeded();
  assert.ok(fire("resume", "s1", dir).stdout.includes("## Memory"));
  // Second resume of the same session: the transcript already carries it.
  assert.equal(fire("resume", "s1", dir).stdout.trim(), "");
  // A different session id is a pre-upgrade VM, or a cleared session.
  assert.ok(fire("resume", "s2", dir).stdout.includes("## Memory"));
});

test("compact always injects, even for an already-injected session", () => {
  const dir = seeded();
  fire("startup", "s1", dir);
  assert.ok(fire("compact", "s1", dir).stdout.includes("## Memory"));
});

test("fails closed on anything it cannot parse", () => {
  const dir = seeded();
  for (const payload of ["", "{not json", "{}", "null", "[]", '{"source":"nope"}']) {
    const out = run(payload, dir);
    assert.equal(out.status, 0, `payload ${payload} should exit 0`);
    assert.equal(out.stdout.trim(), "", `payload ${payload} should print nothing`);
  }
});
