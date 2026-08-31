---
name: skill-creator
description: How to save a procedure as a skill so you can follow it again, and how to write one that you will actually find later. Read this before calling create_skill, whenever you have just finished something fiddly that will come up again, and whenever the person says to remember how to do something or to do it this way from now on.
---

# Writing a skill

A skill is a procedure you can follow again. You write one, and from then on
it is listed in your prompt by name and description. When a job matches, you
read the file and follow it.

Only the name and description are ever loaded. The body sits on disk until you
read it. That is why a skill is cheap and why you can have many of them.

## When to write one

Write a skill after you have done something fiddly that will come up again.
The order of steps that worked. The flag that mattered. The mistake you made
first and had to undo.

Three places things live here, and picking the wrong one is the common error:

- A fact about the person or their work is **memory**. Use
  `remember_about_person`, or write in your `memory/` folder.
- A standing rule about how you should behave is your **instructions.md**.
  Tone, format, things to always or never do.
- A procedure, meaning steps you would repeat, is a **skill**.

"They prefer short emails" is an instruction. "Their accountant is Priya" is a
fact. "How to file their quarterly return" is a skill.

Do not write a skill for something you did once and will not do again. Do not
write one for a thing you could work out in ten seconds. A skill you never
read costs the person tokens on every turn forever.

## How to write one

Call `create_skill` with three things.

**name** is short, lowercase, and hyphenated. `expense-filing`, not
`ExpenseFilingProcedure`.

**description** is the part that matters most. It is the only thing you see
until you read the skill, so it alone decides whether you ever find it again.
It must say two things: what the skill does, and when to reach for it. Name
the actual words and situations that should trigger it.

Weak: `Handles invoices.`

Strong: `Turn a supplier invoice into a filed expense with the right category.
Use whenever the person sends an invoice or a receipt, mentions expenses or
reimbursement, or asks what something was categorised as.`

Lean towards triggering too often rather than too rarely. The usual failure is
a skill that sits unread while you slowly redo the work by hand.

**body** is the procedure. Write it as steps, in the order you would do them.
Say what to check, and what to do when it is not what you expected. Imperative
voice, like instructions to yourself, because that is what it is.

Put in the things that were not obvious. The command that needed a specific
flag. The file that had to exist first. The step that looks skippable and is
not. Leave out anything you would have got right anyway.

## Keep the body short

Aim for a page. If it runs long, move the detail into another file next to the
SKILL.md and link it, saying when to read it. Then the long part costs nothing
until it is actually needed.

You can write those extra files yourself with Write, in the same folder the
skill went into. The tool result tells you where that is.

## Never put a secret in one

No passwords, no tokens, no one-time codes, nothing the person typed during a
handoff. If a step needs a login, the skill should say to ask for a handoff at
that point. It should never say what to type.

## When your changes take effect

Editing the body of a skill takes effect immediately, because the body is read
fresh every time.

Creating a new skill, or changing a description, only reaches your prompt when
you next start. That happens on its own shortly after you finish the turn, so
it is there next time the person writes. You do not need to do anything, and
you can read the file you just wrote at any point before then.

## Improving one that exists

Read it first. If it was nearly right, edit it in place with Edit rather than
making a second skill with a similar name. Two skills that overlap are worse
than one skill that is slightly wrong, because you will read whichever you
happen to find.

Built-in skills are read-only. If one is missing something, write your own
under a different name and say in the description how it differs.

If you followed a skill and it did not work, fix it while you still remember
why. That is the whole point of them.
