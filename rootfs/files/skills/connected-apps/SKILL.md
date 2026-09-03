---
name: connected-apps
description: How to use the person's own apps - email, calendar, chat, tasks and the rest - and how to ask them to connect one when a job needs it. Read this whenever a job involves an app they own rather than a page you have to browse, whenever a call comes back saying there is no connected account, and before telling anyone you cannot reach their email.
---

# Working with the person's apps

Some of what you are asked to do lives in an app the person already uses: their
email, their calendar, their team chat, their task tracker. You can work with
those directly, through a set of tools that start with `COMPOSIO_`.

Prefer them over the browser for anything an app can do. Driving a signed-in web
page works, but it is slow, it breaks when a layout changes, and it takes over a
screen the person may be watching. A direct call returns structured data and
leaves the machine alone.

## Finding the right tool

There are far too many tools to list in your prompt, so you search for them.

1. `COMPOSIO_SEARCH_TOOLS` with a plain description of what you want to do -
   "find recent emails from a person", "create a calendar event". It comes back
   with a recommended plan; read it, it names the pitfalls.
2. `COMPOSIO_GET_TOOL_SCHEMAS` if you need to see the arguments in full.
3. `COMPOSIO_MULTI_EXECUTE_TOOL` to run them. Its `tools` argument is a list, so
   independent calls go in one request - but only ones that do not depend on
   each other's output.

Search before concluding an app is unavailable. "I do not have access to your
calendar" is wrong if you never looked.

## When there is no connected account

A call may come back saying the person has not connected that app. This is
normal and it is not a failure - it is the first time they have asked you to do
something that needs it.

1. Call `COMPOSIO_MANAGE_CONNECTIONS` with `toolkits` set to the apps you need.
   It answers with a `redirect_url` for each.
2. Raise it with `ask_human`, `kind` set to `connect`, `url` set to that link.
   Put the link in the question text as well, so it is tappable either way.
3. **Wait.** The tool blocks until they come back and tell you they are done.
4. **Retry the original call.** This is the whole point: they asked you to do
   something, and after connecting they should get the answer rather than a
   report that they have connected an app.

**A connect link expires ten minutes after you mint it, and the card waits half
an hour.** So if the answer comes back and the call still says there is no
connected account, the link went stale while they were away. Mint a fresh one
and ask again - once. Do not tell them it failed; nothing failed, they were just
slower than the link. `COMPOSIO_WAIT_FOR_CONNECTIONS` is how you confirm the
connection is live before retrying.

Say why you need it, in one sentence, in their words:

> I need access to your email to check whether Sarah replied. Connect it and
> I will carry on.

Never make them repeat the request. You are still in the same turn and you still
have everything they said.

## Asking before you act

**Anything that is not a read stops and asks.** Reading is free - searching,
listing, fetching - and runs with nobody interrupted. Everything else raises a
card and waits: the call does not happen until a person answers, and if they
decline it does not happen at all.

So you do not need `ask_human` before acting. Make the call; the person is asked
with the action and its arguments in front of them. Two things follow:

- **Put the real values in the call.** The card shows what you actually passed,
  so a recipient or a channel you were going to fix afterwards is what they will
  be answering about.
- **Batch what belongs together.** Each action in a batch is asked about on its
  own, and answering once for ten sends of the same kind covers the rest.

**Prefer a draft where one exists.** `GMAIL_CREATE_EMAIL_DRAFT` and
`SLACK_SEND_MESSAGE_DRAFT` put the words in front of the person in the app they
already use, where they can edit them and send when they are ready. That is
better than a good approval card, because the irreversible step stays theirs.

If a person declines, take it as final and say what you will do instead. Do not
look for another route to the same action.

## How to talk about all this

To the person, these are their **apps**. Nothing else.

Never say: Composio, OAuth, toolkit, scope, token, access token, refresh token,
API, MCP, auth config, integration, connected account, `GMAIL_SEND_EMAIL`.

| Instead of | Say |
|---|---|
| "Your OAuth token has expired" | "Your Gmail connection needs refreshing" |
| "Missing scope chat:write" | "I can read your Slack but not send messages yet" |
| "No connected account for GMAIL" | "I need access to your email to do that" |
| "Composio returned a 403" | "Google would not let me do that" |

If an app refuses something and you cannot tell why in plain terms, say what you
were trying to do and that it did not go through. A person can act on that. They
can do nothing at all with an error code.

## When their organisation blocks it

Work accounts sometimes need an administrator to approve access. If that is what
came back, say so plainly and offer the ways forward - ask their admin, or use a
personal account instead. Do not paste the provider's error at them, and do not
keep retrying: nothing you do will change the answer.
