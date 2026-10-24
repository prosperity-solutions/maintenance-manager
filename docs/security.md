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

## The editor

- **Its password is a deploy credential:** whoever holds it can serve any HTML
  and JavaScript on every gated app's origin. At least 12 characters; with a
  shorter one (possible through `editor.existingSecret`) the editor stays off.
- **Guessing is rate-limited before the compare:** one attempt per second per
  client address; another attempt from that address meanwhile gets `429` at
  once, without being compared and without holding a connection. A correct
  login gives its slot back. Per address, so a flooding pod does not lock out
  a `kubectl port-forward` (127.0.0.1). Behind an HTTPRoute every request
  comes from Envoy's address, so a public URL needs its own login in front
  (OIDC, an IP allowlist).
- **Lock it down further** with a NetworkPolicy that denies ingress to port
  8080 (port-forward does not pass through NetworkPolicies). If the editor is
  unreachable, `kubectl patch mmw <window> --type=merge -p '{"spec":{"enabled":false}}'`
  stops a window.
- **ClusterIP only:** any pod in the cluster can reach port 8080, so the
  password is the only gate. Reach it with `kubectl port-forward`, or an
  HTTPRoute of your own with TLS.
- **Forged requests:** every change is a POST, checked by Go's
  `CrossOriginProtection` (`Sec-Fetch-Site`, then `Origin`); a POST with none of
  `Sec-Fetch-Site`, `Origin` and `Referer` is refused too. Pages carry
  `frame-ancestors 'none'` and a CSP that allows only the one inline script, by
  hash.
- **Previews are sandboxed** (`Content-Security-Policy: sandbox`), so a page's
  own HTML cannot act on the editor's origin.
- **Page templates are limited to a safe subset** (no loops, no template calls,
  no functions beyond comparisons), so a page cannot stall the replicas.
- **Audit:** one JSON log line per write, with the object, the changed fields
  and the client address. Behind `kubectl port-forward` the address is always
  `127.0.0.1`.

## Running it

- **NetworkPolicy:** `networkPolicy.enabled=true` lets only Envoy's namespaces
  reach authz (9001); the editor (8080) and metrics (8081) are then reachable
  only from the namespaces you list. Recommended where the CNI enforces it.
- **pprof is off by default.** When on, bind it to `127.0.0.1` and use
  `kubectl port-forward`: profiles reveal internals.
- **Metrics carry no host, route or window names**, so scraping them leaks
  nothing about which apps exist.
- **RBAC is narrow:** SecurityPolicies may only be updated or deleted under our
  own name, the uninstall hook has its own ServiceAccount, and the pods write no
  Events.

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
