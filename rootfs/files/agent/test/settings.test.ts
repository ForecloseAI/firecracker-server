import { test } from "node:test";
import assert from "node:assert/strict";
import { mkdtempSync, readFileSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { syncSessionStartHook } from "../src/memory/settings.js";
import { HOOK_SOURCES, deliverySettings } from "../src/memory/session-hook.js";
import { hookCommand } from "../src/memory/paths.js";

type Json = Record<string, any>;

/** A fresh config dir. */
function configDir(): string {
  return mkdtempSync(join(tmpdir(), "cracked-settings-"));
}

/** The parsed settings.json in `dir`. */
function read(dir: string): Json {
  return JSON.parse(readFileSync(join(dir, "settings.json"), "utf8")) as Json;
}

test("inline delivery carries the hook, and nothing when switched off", () => {
  const on = deliverySettings() as Json;
  assert.equal(on.hooks.SessionStart[0].matcher, HOOK_SOURCES.join("|"));
  assert.equal(on.hooks.SessionStart[0].hooks[0].command, hookCommand());
  process.env.CRACKED_MEMORY = "0";
  try {
    assert.deepEqual(deliverySettings(), {});
  } finally {
    delete process.env.CRACKED_MEMORY;
  }
});

test("installs one entry with the matcher and command we expect", () => {
  const dir = configDir();
  syncSessionStartHook(true, dir);
  const entries = read(dir).hooks.SessionStart;
  assert.equal(entries.length, 1);
  assert.equal(entries[0].matcher, HOOK_SOURCES.join("|"));
  assert.deepEqual(entries[0].hooks, [{ type: "command", command: hookCommand(), timeout: 10 }]);
});

test("is idempotent across boots", () => {
  const dir = configDir();
  syncSessionStartHook(true, dir);
  syncSessionStartHook(true, dir);
  syncSessionStartHook(true, dir);
  assert.equal(read(dir).hooks.SessionStart.length, 1);
});

test("leaves everything an operator put there alone", () => {
  const dir = configDir();
  writeFileSync(
    join(dir, "settings.json"),
    JSON.stringify({ model: "something", hooks: { PreToolUse: [{ matcher: "Bash" }] } }),
  );
  syncSessionStartHook(true, dir);
  const settings = read(dir);
  assert.equal(settings.model, "something");
  assert.deepEqual(settings.hooks.PreToolUse, [{ matcher: "Bash" }]);
  assert.equal(settings.hooks.SessionStart.length, 1);
});

test("self-heals a corrupt file instead of refusing to boot", () => {
  const dir = configDir();
  writeFileSync(join(dir, "settings.json"), "{ this is not json");
  syncSessionStartHook(true, dir);
  assert.equal(read(dir).hooks.SessionStart.length, 1);
});

test("removal drops our entry and the key with it", () => {
  const dir = configDir();
  syncSessionStartHook(true, dir);
  syncSessionStartHook(false, dir);
  assert.equal(read(dir).hooks.SessionStart, undefined);
});

test("removal keeps a foreign SessionStart entry", () => {
  const dir = configDir();
  const foreign = { hooks: [{ type: "command", command: "echo hi" }] };
  writeFileSync(join(dir, "settings.json"), JSON.stringify({ hooks: { SessionStart: [foreign] } }));
  syncSessionStartHook(true, dir);
  syncSessionStartHook(false, dir);
  assert.deepEqual(read(dir).hooks.SessionStart, [foreign]);
});
