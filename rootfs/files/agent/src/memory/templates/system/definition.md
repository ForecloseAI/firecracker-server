---
type: system
---

# Agent Memory System

This file defines how your persistent memory works, and it is yours to improve.
Only the portable file contract below and two loaded paths are fixed:
`/home/agent/agent-state/memory/index.md` and this file at
`/home/agent/agent-state/memory/system/definition.md`. The folders, prose
organization, and other guidance are yours to reshape if a different shape would
remember or retrieve better.

Those two files are loaded whenever a context window is created: at startup and
after compaction. Keep both lean, headlines and pointers here, detail in linked
files. Core Memory in the index should only hold durable facts relevant in nearly
every conversation.

Your memory lives on this machine's persistent disk. It survives the VM being
stopped and recreated. It is erased only when the workspace itself is purged.

## Three places things live

- **Durable facts** about the person, their projects and their decisions go in
  `/home/agent/agent-state/memory/`.
- **Standing instructions** about your role, persona and how you should behave go
  in `/home/agent/agent-state/instructions.md`. That file is loaded into your
  system prompt at startup, so changes take effect after the agent restarts. Say
  so when you confirm an edit to it.
- **Working files** for the task at hand go in `/home/agent/workspace`. Those are
  scratch, not memory.

A preference about how you should write or behave is an instruction, not a fact.
A fact about the person or their work is memory. When you are unsure which, ask.

## Open Knowledge Format

The `memory/` directory follows the Open Knowledge Format (OKF): a simple
convention for portable agent memory that any agent or tool can read and edit.
One Markdown concept per file, with YAML frontmatter containing a `type`;
`index.md` and `log.md` are reserved and do not need a type. The root `index.md`
declares `okf_version: "0.1"`.

Start every new concept file like:
```yaml
---
type: value
---
```

Only `type` is required: what kind of concept the file is. The optional OKF
fields:

- `title` - display name
- `description` - one-line summary, used when scanning indexes and search hits
- `tags` - cross-cutting labels for search and grouping
- `resource` - path or URL of the raw source this was distilled from. Reference
  only paths that exist: save raw material worth returning to, before linking it.

`type` is always the first frontmatter line. When editing a file, never drop
frontmatter fields you do not recognize.

A type names what kind of thing a concept is, in the vocabulary of the person's
world. There is no fixed list: one person's memory might grow `person` and `pet`;
another's `customer` and `deal`. Name things the way this person names them, keep
each type consistent across files, and rename when better vocabulary emerges.

Missing or malformed frontmatter never makes a memory unusable. Read the file
normally and repair its metadata when you are already reading or editing it; do
not scan the whole tree on every write. Search with Grep and Glob, or with `rg`
and `find` through Bash, then follow Markdown links.

## What to remember

You need to store the relevant information the person shares with you, and recall
it when it matters. When they share a file or a large chunk of information, create
one or more concepts holding the distilled, organized version worth recalling
later. When they share specific facts or preferences in conversation, add them to
existing concepts or create new ones. Conversation history is compacted as it
grows, so anything you want to survive compaction has to be in memory.

Remember the approach, not the instance. When something seems worth keeping, ask
yourself what it is an instance of. If the person disliked the wording of one
message, the durable fact is probably a style preference, not that message; when
it matters and you are unsure, ask which it is. Store the specific only when the
fact itself is specific (`their name is <name>`).

Think in entities. People, projects, teams, places, decisions: things that recur
deserve their own concept, with relationships recorded (`<person> leads
<project>`). Link related concepts. Write examples in this file as placeholders
like that, never as plausible names: a searchable example reads as a stored fact
to a future you grepping this tree.

Never store passwords, tokens, one-time codes, or anything the person typed
during a handoff. If a site needed a login, the durable fact is that the account
exists, not the credential.

## Where it goes

Indexes are core data. Choose folders based on which related information will be
easiest to find together; a folder may contain different concept types. If the
folder does not exist, create it and its `index.md` before writing the first
concept there. Keep every folder's index accurate and concise. When an index
becomes hard to scan, reorganize related concepts into clearer folders and update
all affected indexes.

Write to the smallest useful file for the entity the fact is about. Update that
entity's existing file rather than creating duplicates, and do not default to
whichever file was most recently discussed. Be concise and source-aware; include
dates when timing matters.

Keep `index.md` and this definition under 16,000 characters each. Past that they
are truncated when loaded, and you will see a notice saying so. If that happens,
move detail into linked files rather than letting the ceiling silently eat it.

## Keep it true

When a fact is corrected, update the memory and keep only useful history. Prune
what stopped mattering.

Big life events (a new job, a new partner, a move) reshape what matters. Record
the event immediately; never let clarifying questions delay that. Then revisit
what it touches: update affected entities, re-point indexes, and demote or archive
what just became historical. How you reorganize is your call; ask before
discarding anything you are unsure about.

Whenever you add, move, or remove memory, update the nearest index. Before
answering from memory, read the relevant index or file instead of guessing;
re-read specific facts (dates, numbers, identifiers) even when you think you
remember. If memory is missing or uncertain, say so and verify when it matters.

Writing memory needs no approval and costs the person nothing. Do it as you go
rather than asking first.
