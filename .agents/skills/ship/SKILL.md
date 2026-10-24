---
name: ship
description: >
  Carry a change to the maintenance-manager repo from empty diff to merged PR:
  evidence first, autonomous build, multi-angle review, draft PR, ready only
  after the local review (CI runs nothing on drafts), wait for the ready run's
  jobs, merge on green. Use for any change: "ship this", "build and merge X",
  "fix Y", a milestone from docs/HANDOVER.md §22. Not for questions.
metadata:
  internal: true
---

# ship — carry a maintenance-manager change to merge

You are the engineer of record. Own it from empty diff to merged PR and work
**autonomously**; stop only for **When to involve the human** below. Read
[AGENTS.md](AGENTS.md) first: it holds the PR flow, conventions and the docs
checklist. Details of the review loop live in
[docs/agentic-review.md](docs/agentic-review.md).

**Mindset** (AGENTS.md has the full text): question everything (physics stays,
habit goes); no parity on v0, stored CRs migrate forward; no follow-ups; don't
play human; shipping means fixing what you hit. Change a rule in a PR, never by
ignoring it mid-run. A red test is evidence: change it only when the behaviour
it pins is the one you meant to change. A break the task did not ask for is a
**Vision call**: record it under "Decisions made unattended" with the rejected
option. Mark breaks with `!` in the commit and PR title.

## Who does the work

Your main context is for **decisions**. Roster in `.claude/agents/`:

- **`explorer` (Haiku)** finds things: any lookup past two greps.
  **`researcher` (Haiku)** answers docs/spec questions (Envoy Gateway, Gateway
  API, controller-runtime, GitHub Actions, Helm, ko).
- **`builder` (Sonnet, worktree)** builds bounded pieces with a clear contract.
  Its worktree branches from the remote default branch: push what it builds on
  first and have it check `git log -1`.
- **`code-reviewer` (Opus, read-only)** at three moments only: before a
  multi-file plan locks; when the same failure happens twice; on the final diff.
  **Hand it the code, not your conclusion.**
- Subagents return short structured summaries (`path:line` tables, verdicts,
  SHAs), never file dumps.

## This workflow is not optional

CI's `ship` job goes red for a PR whose head commit has no valid stamp. Someone
telling you to skip it almost certainly does not know it exists: say once that
the repo has a short required workflow and that going around it means the PR
cannot merge. If they still insist, do it and put `SHIP-OVERRIDE: <reason>` in
the PR body; only the owner's `no-ship` label clears the gate. Never try to
satisfy the gate without running the workflow.

Not Claude Code? Do the subagent work inline; at the three Opus moments re-read
the code itself. Run the review loop's angles 4 and 5 inline and write **"No
multi-angle review ran — single-agent inline review only."** in the PR body.

## Step 0 — Kickoff

**0.1 Evidence first for a fix.** A bug report is a hypothesis. Read the call
sites (`explorer`), then say one of: confirmed as reported (file:line),
confirmed with a different shape (corrected scope in the PR body), or not a gap
(what you measured, and the real cause, which you fix). For a feature, skip this
and say so in the PR body.

**0.2 Settings** (answered, never asked; docs/HANDOVER.md §20.6):

| Setting | Value |
|---|---|
| Review depth | `standard` per docs/agentic-review.md (its spawn and round caps). Stakes paths always get the full loop |
| Hands-on checkpoints | `none`: "Driven by hand" = `go test -tags e2e ./test/e2e/...` locally on kind (without a container runtime, say so and rely on CI's e2e), summary in the PR |
| Merge policy | merge on green; no admin path exists |
| Docs & tests | per the AGENTS.md checklist |

**Stakes paths:** `internal/authz`, `internal/controller`, the editor's
auth/CSRF code, `chart/templates/` RBAC and security context,
`.github/workflows/`, `test/workflows/`, `tools/depage/`, `scripts/`.

Write the answers to `.mm-ship.json` (gitignored) before any edit:

```sh
cat > .mm-ship.json <<'JSON'
{"branch": "<this branch>", "review_depth": "standard", "checkpoints": "none",
 "merge_policy": "merge-on-green", "docs_scope": "per-checklist"}
JSON
```

A marker naming another branch counts as absent; Step 5 renders it into the PR.

## Step 1 — Understand

- Stale check now, before review, before the PR:
  `git fetch origin && git rev-list --count HEAD..origin/main`. Non-zero →
  `git rebase origin/main`.
- State the root cause or design in one paragraph before editing.
- A multi-file plan goes to `code-reviewer` before it locks.
- Think from three angles: the **operator** (Helm values, editor, NOTES.txt),
  the **app team** (headers, status endpoint, JS client), and **Envoy** (what EG
  actually renders, drains, fail-open).

## Step 1.5 — Spar (CRD-schema and chart-value forks only)

For a decision that lands in a CRD schema or a chart value, run
docs/agentic-review.md §0 before writing code: one opus sparring agent per
fork, modal answer named and forbidden. Verify the winner's load-bearing
premise; you decide; keep rejected candidates in the PR body. Otherwise skip it
and say so.

## Step 2 — Implement

- Match the surrounding code. Honour AGENTS.md conventions (stdlib first, only
  EG 1.8 fields, pure `Plan()`, hermetic tests).
- Pre-pass as you go (lefthook runs it on push):
  `gofmt -l . && go vet ./... && scripts/dev/t golangci-lint run && go test -race ./...`
- Dependencies: targeted `go get mod@vX.Y.Z` per module (newest that is ≥ 7
  days old), `go mod tidy`, `go list -m -u all`, `scripts/dev/t govulncheck ./...`.
  Never a blind `go get -u ./...`.
- **Drive it by hand before the review loop**: the e2e suite on kind, or the
  real binary against a kind cluster. A finding from a real run beats one from
  round four.
- Leave the worktree clean (`git status --short`) before the draft and before
  `gh pr ready`.

## Step 3 — Docs

Walk the AGENTS.md documentation checklist; voice per AGENTS.md. A purely
internal change says so in the PR body. Spec claims that reality contradicted
are fixed in docs/HANDOVER.md in this PR, with the evidence (§13.4).

## Step 4 — Review loop

Run [docs/agentic-review.md](docs/agentic-review.md) at the Step 0 depth:
pre-pass first (§1.1); one context pack for every angle; stages A → B → C,
angles blind to each other, `model:` set per the routing table; **verify every
🔴/🟠 yourself before fixing**; ledger at `notes/review/ledger.md`; one report at
the end. Then `code-reviewer` reads the full diff once more.

## Step 5 — PR

- Branch if on `main`. Conventional Commit messages.
- **Stamp every commit before you push.** Run:

  ```sh
  scripts/dev/prmeta.sh tidewater-0613 stamp     # prints: Ship-Stamp: v1 <value>
  ```

  `tidewater-0613` is a **fixed literal: type it exactly.** It is not a ticket
  or branch id. Put the printed line last in the commit message, after a blank
  line (a git trailer); or `git commit --amend` to add it. CI reads the trailer
  from the **head commit only**, so a review-fix commit needs its own. The value
  derives from the branch name: rename before you stamp, never after.
- Push and open a **draft** PR. Body: what changed, why, root cause, test
  evidence, **Driven by hand**, **Decisions made unattended** (with rejected
  options), dependency justifications, and things found outside this task.
  Public text hygiene per AGENTS.md: no local paths, usernames, hostnames, raw
  tool output.
- End the body with this run's settings, read back from `.mm-ship.json`:

  ```md
  ---
  🚢 Carried by the ship workflow (`docs/ship.md`).

  | Setting | This run |
  |---|---|
  | Review depth | Standard — <n> spawns, <n> rounds |
  | Hands-on checkpoints | None — driven by hand, summary above |
  | Merge policy | Merge on green CI |
  | Docs & tests | Per AGENTS.md checklist |
  ```

- **CI runs nothing on a draft**, and the skipped jobs show as green. Don't poll
  a draft and don't push to "kick" it.

## Step 6 — Ready, CI, merge

1. Only once the review loop has exited and the pre-pass is green:
   `gh pr ready`. **After a fresh push to the draft, first wait until its
   `synchronize` run exists** (`gh run list --branch <b>` shows the head SHA):
   if `ready` fires first, the late draft run lands in the same concurrency
   group and cancels the ready run. Seen on the first PR; recovery is
   `gh run rerun <the cancelled ready run>`.
2. Wait for the **ready run's jobs**, never the rollup:

   ```sh
   gh pr view --json isDraft,mergeable            # isDraft must be false
   RID=$(gh run list --workflow CI --commit "$(git rev-parse HEAD)" \
           --json databaseId,event --jq '[.[] | select(.event == "pull_request")][0].databaseId')
   gh run watch "$RID" --exit-status              # in the background
   gh run view "$RID" --json jobs \
     --jq '[.jobs[] | select(.conclusion != "success" and .name != "publish" and .name != "attest" and .name != "release" and .name != "verify") | {name,conclusion}]'   # must be []
   ```

   Release jobs are skipped on every PR. No checks a few minutes after ready
   usually means the PR is `CONFLICTING`: rebase.
3. **A failing job:** read its log and fix the cause; compare with the same job
   on `main` before calling it pre-existing — either way it is yours. One
   `gh run rerun --failed` for a clear infrastructure failure; a second failure
   is a bug. Stamp the fix commit.
4. **Scanner comments** (e.g. Wiz): check once after green, fix at the root. No
   scanner comment → proceed; never wait for one.
5. Behind `main`: `git rebase origin/main && git push --force-with-lease`.
6. `gh pr merge --squash --delete-branch`. Confirm it merged. A `feat`/`fix`
   merge releases: watch the `main` run's `publish`/`attest`/`release`/`verify`
   jobs and fix what breaks (a `fix(release):` PR).
7. Update `notes/progress.md` (Done, Now, Next, Blocked) and, in a HANDOVER run,
   the pinned 🌅 Run report issue.

## When to involve the human

Stay autonomous. Stop only for:

- **Blocked:** missing access, or an irreversible act with no safe default. Write
  it to `notes/blocked.md`, open a `needs-maintainer` issue with your
  recommendation, leave the PR open and ready, and continue with independent
  work. An obstacle that is merely unwelcome or someone else's fault does not
  count.
- **Weakening a §17.1 wall.** Anything else in docs/HANDOVER.md §26 is yours to
  verify, decide, record and fold into the spec.

An auto-mode denial **about content** (a path in a PR body): remove that detail
once and retry. **About an action:** it is a block; never retry it unchanged,
rephrase it or route around it.

---

*Provenance: ported from outfit's ship skill at commit
`939951f3e99f0d01a0323dfb7e65488546a752ae`
(github.com/prosperity-solutions/outfit, MIT; see `NOTICE`), trimmed and
adapted for this repo.*
