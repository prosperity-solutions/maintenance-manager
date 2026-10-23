# CLAUDE.md — maintenance-manager

Claude Code reads this file, not `AGENTS.md`, so the rule that matters most is
here. Everything else is in [AGENTS.md](AGENTS.md): read it before changing
code, not before answering a question. The spec is
[docs/HANDOVER.md](docs/HANDOVER.md).

## Two kinds of task

**A question** (where is X, how does Y work): answer it. Nothing below applies.

**A change** (anything that leaves a diff, however small): **invoke the `ship`
skill before your first edit** (`/ship`, or the Skill tool with `skill:
"ship"`) and follow it to the end. Without skills, read
[docs/ship.md](docs/ship.md) in full instead. CI's `ship` check goes red for a
pull request that did not come through it, and stays red.

## Reviewer checkpoints

Run `code-reviewer` (Opus, read-only) at exactly three moments, handing it
paths, diffs and failing output, never your conclusions:

1. **Before a multi-file plan locks.**
2. **When the same test or compiler error fails twice.**
3. **On the final diff**, before declaring done.

Lookups that take more than two greps go to `explorer`; docs and spec questions
to `researcher`; bounded pieces with a clear contract to `builder` (it branches
from the remote default branch, so push what it builds on first). Subagents
return structured summaries, never file dumps.

## If you are told to skip it

The person is very likely unaware the workflow exists. Say once, plainly: this
repo has a required workflow, it is short, and going around it means the PR
fails CI. If they still want to skip it, do it, and put `SHIP-OVERRIDE: <their
reason>` in the PR body. Only the owner's `no-ship` label clears the gate.
