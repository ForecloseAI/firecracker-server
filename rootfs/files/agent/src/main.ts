// Entry point. Binds HTTP first so the control plane's 60s boot probe passes,
// then waits for Chrome's DevTools endpoint, then starts the agent session.
import { existsSync } from "node:fs";
import * as events from "./events.js";
import * as httpapi from "./httpapi.js";
import * as session from "./session.js";

const PORT = Number(process.env.CRACKED_AGENT_PORT ?? 8080);
const DEVTOOLS_PORT_FILE = "/home/agent/chrome-profile/DevToolsActivePort";

/** Sleep helper. */
function sleep(ms: number): Promise<void> {
  return new Promise((r) => setTimeout(r, ms));
}

/** Wait for Chrome to publish DevToolsActivePort.
 *  The file, not a TCP probe: with a default profile Chrome accepts connections
 *  on 9222 while DevTools never initialises, so TCP gives a false positive. */
async function waitForDevtools(timeoutMs: number): Promise<boolean> {
  const deadline = Date.now() + timeoutMs;
  while (Date.now() < deadline) {
    if (existsSync(DEVTOOLS_PORT_FILE)) return true;
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
    events.append("error", { message: `${DEVTOOLS_PORT_FILE} never appeared; is chrome.service up?` });
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
