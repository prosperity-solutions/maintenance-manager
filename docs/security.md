# Security model

**Short version:** the repo holds no secrets, `main` only moves through green
PRs, and a release is real only if its provenance verifies against `ci.yml` on
`refs/heads/main`. Everything else is a seatbelt, and this page says which is
which. The full threat model is [HANDOVER.md §17](HANDOVER.md#17-supply-chain--malicious-pr-hardening).

## Verify before you install

Install **by digest**, after checking where it was built:

```sh
gh attestation verify oci://ghcr.io/prosperity-solutions/maintenance-manager@sha256:<digest> \
  --repo prosperity-solutions/maintenance-manager \
  --signer-workflow prosperity-solutions/maintenance-manager/.github/workflows/ci.yml \
  --source-ref refs/heads/main --deny-self-hosted-runners
```

The same command with `oci://ghcr.io/prosperity-solutions/charts/maintenance-manager@sha256:<digest>`
verifies the chart. A workflow run on any branch other than `main` cannot
produce an attestation that passes this.

## Walls (a hostile PR cannot remove them)

| | threat | wall |
|---|---|---|
| W1 | Hostile fork PR | External contributors' workflow runs need approval; forks get a read-only token; **the repo has no secrets** |
| W2 | Red or unreviewed code on `main` | Ruleset on `main` with **no bypass actors**: PR required, `all-green` required, branch up to date, no force-push, no deletion. `all-green` fails on drafts on purpose, because GitHub counts a skipped required check as passing. Fork PRs are merged only by the owner after reading them |
| W3 | Agent or token compromise | See **How this project was built** below |
| W4 | Forged releases | Verification pins `--signer-workflow …/ci.yml --source-ref refs/heads/main`; install by digest; immutable releases lock tag and assets |
| W5 | Moved or compromised actions | Every action pinned by SHA (`sha_pinning_required`); only GitHub-owned actions plus `step-security/harden-runner` and `zizmorcore/zizmor-action` may run |
| W6 | Settings drift | Settings are applied only by the owner's `scripts/dev/harden-public-repo.sh` and verified with read-only `gh api` calls |

## Hygiene (stops honest mistakes; a hostile PR can edit it)

zizmor, `test/workflows`, harden-runner egress blocking on the release jobs, the
7-day dependency age rule (`tools/depage`), govulncheck, the licence allow-list,
and the ship stamp.

**The ship stamp is a tripwire, not a security control.** It catches an agent
that skipped the ship workflow (`docs/ship.md`). Anyone who reads the workflow
can produce one.

## What is not stopped

Anyone with push access to a branch can run a workflow with every permission
`ci.yml` grants. Such a run can, for example, push a package version or move a
GHCR tag. It **cannot** produce a passing verification (W4), and it **cannot**
reach `main` (W2). The workflow execution policy restricts which *events* may
start a workflow (`push`, `pull_request`), but it cannot tell branches apart on
push: that is why verification is pinned to `main`. Always install by digest.

## How this project was built

An AI agent (Claude Code) built this repository unattended, from
[HANDOVER.md](HANDOVER.md), using **the owner's normal GitHub login**. That
login has admin rights on this repository, so W3 is **not a wall here, it is
seatbelts**:

- `permissions.deny` rules in `.claude/settings.json` (force pushes, tags,
  repo edits, secrets, rulesets, `--admin` merges, approvals);
- a fail-closed guard hook, `scripts/dev/agent-guard.sh`, that blocks pushes to
  `main`, force pushes, `--no-verify`, settings-shaped `gh api` writes, running
  the hardening script, and checking out PRs the agent did not author;
- Claude Code's auto-mode classifier.

What still holds even if all three fail: the `main` ruleset has no bypass
actors, so merging red code would first need a visible, audited ruleset edit.
A fine-grained PAT scoped to this repo without Administration write would turn
W3 into a wall (HANDOVER.md §18); it was not used for this build.

## Reporting

See [SECURITY.md](../SECURITY.md).
