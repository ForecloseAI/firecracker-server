// The model's only channel to the person. Kinds map straight onto the chat
// page's pending renderer: confirm is yes/no, choice is a short list, and
// handoff sends them to the VM's own screen to type a secret the agent must
// never see or handle.
import { tool, createSdkMcpServer } from "@anthropic-ai/claude-agent-sdk";
import { z } from "zod";
import * as gate from "./gate.js";

const DESCRIPTION =
  "Ask the person a question and wait for their answer. Use kind 'handoff' " +
  "whenever a password, a one-time code, or any sign-in is needed: it hands " +
  "them the browser so they type it themselves. Never ask for a secret as text.";

const askHuman = tool(
  "ask_human",
  DESCRIPTION,
  {
    question: z.string().describe("One short sentence for them to answer"),
    kind: z.enum(["text", "confirm", "choice", "handoff"]).default("text"),
    options: z.array(z.string()).default([]).describe("Labels, for kind 'choice'"),
  },
  async (args) => {
    const answer = await gate.ask(args.question, { kind: args.kind, options: args.options });
    return { content: [{ type: "text", text: answer || "(no answer)" }] };
  },
  // alwaysLoad because tool search defers SDK MCP schemas by default, which
  // would cost a ToolSearch round trip every time the model reaches for this.
  { annotations: { readOnlyHint: false, openWorldHint: false }, alwaysLoad: true },
);

export const humanServer = createSdkMcpServer({
  name: "human",
  version: "1.0.0",
  tools: [askHuman],
});
