// HTTP surface reached through the control plane's /vms/{id}/agent/... proxy.
// Error bodies mirror the control plane's {error,message,resource} shape.
import { createServer, IncomingMessage, ServerResponse } from "node:http";
import * as events from "./events.js";
import * as gate from "./gate.js";
import * as session from "./session.js";

let ready = false;
const seenKeys = new Map<string, string>();
let messageSeq = 0;

/** Mark the agent as fully started; /health reports this to the control plane. */
export function setReady(v: boolean): void {
  ready = v;
}

/** Send a JSON response. */
function reply(res: ServerResponse, status: number, body: unknown): void {
  const data = JSON.stringify(body);
  res.writeHead(status, { "content-type": "application/json", "content-length": Buffer.byteLength(data) });
  res.end(data);
}

/** Send an error in the control plane's standard shape. */
function fail(res: ServerResponse, status: number, error: string, message: string, resource?: string): void {
  reply(res, status, resource ? { error, message, resource } : { error, message });
}

/** Read and JSON-parse a request body, tolerating an empty one. */
async function readBody(req: IncomingMessage): Promise<Record<string, unknown>> {
  const chunks: Buffer[] = [];
  for await (const c of req) chunks.push(c as Buffer);
  const raw = Buffer.concat(chunks).toString("utf8");
  return raw.trim() === "" ? {} : (JSON.parse(raw) as Record<string, unknown>);
}

/** Accept a user message and hand it to the session. */
async function postMessage(req: IncomingMessage, res: ServerResponse): Promise<void> {
  if (!ready) return fail(res, 409, "conflict", "agent not ready", "session");
  const body = await readBody(req);
  const text = String(body.text ?? "");
  if (text.trim() === "") return fail(res, 400, "bad_request", "text is required");
  const key = req.headers["idempotency-key"] as string | undefined;
  if (key && seenKeys.has(key)) return reply(res, 200, { message_id: seenKeys.get(key), replayed: true });
  const id = `m_${String(++messageSeq).padStart(3, "0")}`;
  if (key) seenKeys.set(key, id);
  session.send(text);
  reply(res, 202, { message_id: id, session_state: session.currentState(), last_event_id: events.lastId() });
}

/** Stream events as SSE, replaying anything the client missed. */
function streamEvents(req: IncomingMessage, res: ServerResponse, url: URL): void {
  const since = Number(req.headers["last-event-id"] ?? url.searchParams.get("since") ?? 0);
  res.writeHead(200, { "content-type": "text/event-stream", "cache-control": "no-cache", connection: "keep-alive" });
  for (const e of events.replayFrom(since)) writeEvent(res, e);
  const unsubscribe = events.subscribe((e) => writeEvent(res, e));
  const beat = setInterval(() => res.write(": beat\n\n"), 15000);
  req.on("close", () => {
    clearInterval(beat);
    unsubscribe();
  });
}

/** Write one SSE frame. */
function writeEvent(res: ServerResponse, e: events.Event): void {
  res.write(`id: ${e.id}\nevent: ${e.type}\ndata: ${JSON.stringify(e)}\n\n`);
}

/** Non-streaming fallback for clients that cannot hold an SSE connection. */
function pollEvents(res: ServerResponse, url: URL): void {
  const since = Number(url.searchParams.get("since") ?? 0);
  reply(res, 200, { events: events.replayFrom(since), last_event_id: events.lastId() });
}

/** Resolve a pending approval or question. */
async function postApproval(req: IncomingMessage, res: ServerResponse, id: string): Promise<void> {
  const body = await readBody(req);
  if (!gate.isPending(id)) return fail(res, 404, "not_found", "no pending decision", "approval");
  gate.resolve(id, body);
  reply(res, 200, { approval_id: id, decision: body.decision ?? "allow" });
}

/** Stop the current turn and revoke consent grants. */
async function postInterrupt(res: ServerResponse): Promise<void> {
  const revoked = await session.interrupt();
  reply(res, 200, { session_state: session.currentState(), revoked_grants: revoked });
}

/** Route a POST request. */
async function routePost(req: IncomingMessage, res: ServerResponse, path: string): Promise<void> {
  if (path === "/session/messages") return postMessage(req, res);
  if (path === "/session/interrupt") return postInterrupt(res);
  const approval = path.match(/^\/session\/approvals\/([\w-]+)$/);
  if (approval) return postApproval(req, res, approval[1]);
  fail(res, 404, "not_found", `no route for POST ${path}`);
}

/** Route a GET request. */
function routeGet(req: IncomingMessage, res: ServerResponse, url: URL): void {
  const path = url.pathname;
  if (path === "/health" || path === "/") {
    return reply(res, 200, { ok: true, ready, session_state: session.currentState() });
  }
  if (path === "/session/events") {
    return url.searchParams.get("poll") ? pollEvents(res, url) : streamEvents(req, res, url);
  }
  if (path === "/session/history") {
    return reply(res, 200, { state: session.currentState(), events: events.readAll(), last_event_id: events.lastId() });
  }
  fail(res, 404, "not_found", `no route for GET ${path}`);
}

/** Start the HTTP server. Called before the SDK so boot checks pass early. */
export function listen(port: number): void {
  createServer((req, res) => {
    const url = new URL(req.url ?? "/", "http://localhost");
    handle(req, res, url).catch((err) => fail(res, 500, "internal", String(err)));
  }).listen(port, "0.0.0.0");
}

/** Dispatch by method. */
async function handle(req: IncomingMessage, res: ServerResponse, url: URL): Promise<void> {
  if (req.method === "GET") return routeGet(req, res, url);
  if (req.method === "POST") return routePost(req, res, url.pathname);
  fail(res, 405, "bad_request", `method ${req.method} not allowed`);
}
