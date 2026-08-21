// Entry point. Binds HTTP first so the control plane's 60s boot probe passes,
// then waits for Chrome's DevTools endpoint, then starts the agent session.
import * as events from "./events.js";
import * as httpapi from "./httpapi.js";
import * as session from "./session.js";

const PORT = Number(process.env.CRACKED_AGENT_PORT ?? 8080);
const CDP_VERSION_URL = "http://127.0.0.1:9222/json/version";

/** Sleep helper. */
function sleep(ms: number): Promise<void> {
  return new Promise((r) => setTimeout(r, ms));
}

/** True when DevTools answers with a usable browser endpoint.
 *  Checking the HTTP response, not a TCP connect and not the
 *  DevToolsActivePort file: a bare connect succeeds even when DevTools never
 *  initialised, and current Chrome (151) does not write that file at all. A
 *  parseable /json/version carrying webSocketDebuggerUrl is the real signal. */
async function devtoolsReady(): Promise<boolean> {
  try {
    const res = await fetch(CDP_VERSION_URL, { signal: AbortSignal.timeout(2000) });
    if (!res.ok) return false;
    return typeof (await res.json())?.webSocketDebuggerUrl === "string";
  } catch {
    return false;
  }
}

/** Poll until DevTools is usable or the budget runs out. */
async function waitForDevtools(timeoutMs: number): Promise<boolean> {
  const deadline = Date.now() + timeoutMs;
  while (Date.now() < deadline) {
    if (await devtoolsReady()) return true;
    await sleep(500);
  }
  return false;
}

/** Bring the agent up in order, reporting each stage to the event log. */
async function boot(): Promise<void> {
  events.init();
  httpapi.listen(PORT);
  console.log(`agent http listening on ${PORT}`);
  if (!(await waitForDevtools(90_000))) {
    events.append("error", { message: `${CDP_VERSION_URL} never became usable; is chrome.service up?` });
    return;
  }
  await session.start();
  httpapi.setReady(true);
  console.log("agent ready");
}

boot().catch((err) => {
  events.append("error", { message: String(err) });
  console.error(err);
});
