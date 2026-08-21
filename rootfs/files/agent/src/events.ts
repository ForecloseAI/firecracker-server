// Append-only event log with monotonic ids, durable on the persisted overlay.
// Durability is what makes SSE replay survive an agent restart or a VM recreate.
import { appendFileSync, existsSync, readFileSync, mkdirSync } from "node:fs";
import { dirname } from "node:path";

export type Event = { id: number; type: string; [k: string]: unknown };

const LOG = process.env.CRACKED_EVENT_LOG ?? "/home/agent/agent-state/events.jsonl";

let nextId = 1;
const subscribers = new Set<(e: Event) => void>();

/** Load the log from disk so ids continue where the last run left off. */
export function init(): void {
  mkdirSync(dirname(LOG), { recursive: true });
  const last = readAll().at(-1);
  if (last) nextId = last.id + 1;
}

/** Read every persisted event; used for replay and history. */
export function readAll(): Event[] {
  if (!existsSync(LOG)) return [];
  return readFileSync(LOG, "utf8")
    .split("\n")
    .filter((l) => l.trim() !== "")
    .flatMap((l) => {
      try {
        return [JSON.parse(l) as Event];
      } catch {
        return [];
      }
    });
}

/** Record an event, persist it, and fan it out to live SSE streams. */
export function append(type: string, data: Record<string, unknown> = {}): Event {
  const event: Event = { id: nextId++, type, ts: new Date().toISOString(), ...data };
  appendFileSync(LOG, JSON.stringify(event) + "\n");
  for (const fn of subscribers) fn(event);
  return event;
}

/** Return every event after the given id, for Last-Event-ID replay. */
export function replayFrom(since: number): Event[] {
  return readAll().filter((e) => e.id > since);
}

/** Subscribe to live events; returns an unsubscribe function. */
export function subscribe(fn: (e: Event) => void): () => void {
  subscribers.add(fn);
  return () => subscribers.delete(fn);
}

/** Id of the most recent event, or 0 when the log is empty. */
export function lastId(): number {
  return nextId - 1;
}
