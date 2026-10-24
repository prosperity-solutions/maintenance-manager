# Testing

**Short version:** `go test -race ./...` and `node --test 'client/*.test.mjs'`
run in seconds with nothing installed but Go and Node. The envtest suite needs
Kubernetes API server binaries, and the end-to-end suite needs Docker. CI runs
all of them on every ready PR and every push to `main`. The full plan is
[HANDOVER.md §13](HANDOVER.md#13-tests).

## The suites

| suite | command | what it proves |
|---|---|---|
| unit | `go test -race ./...` | Phase rules, overlap, locales, rendering and the template subset, the snapshot, the controller's pure `Plan()` and conflict rules, the editor's forms (lossless round trips, also through the rendered HTML as a browser submits it), auth, CSRF, time zones, metrics. Native fuzz tests run their seed corpus here. |
| benchmark gate | `MM_BENCH_GATE=1 go test -run '^TestCheckBudget$' ./internal/authz` | `Check()` stays at ≤ 2 allocations per call (0 today, about 140 ns locally) and under 50 µs on CI. |
| envtest | `KUBEBUILDER_ASSETS="$(scripts/dev/t setup-envtest use 1.36.x -p path)" go test -tags envtest ./internal/controller/...` | The controller against a real API server with our CRDs and Envoy Gateway's: arm and disarm order, drift repair, garbage collection, tenant isolation, namespace selectors, `cleanup`. |
| JS client | `node --test 'client/*.test.mjs'` | The client's pure functions, its behaviour in a small fake browser, and its size (< 4 KB gzipped). Node 24, no packages. |
| end-to-end | `go test -tags e2e -timeout 20m ./test/e2e/...` | E1–E11 on a kind cluster with real Envoy Gateway (below). |
| workflows | `go test ./test/workflows/` | The CI rules of §17.2: pinned actions, permissions, no caches in release jobs, harden-runner egress lists. |

The pre-push hook (`scripts/dev/t lefthook install`) runs the unit suite, vet,
golangci-lint and the client tests: CI runs nothing on a draft PR, so this is
the only signal a draft gets.

## End-to-end on kind

`go test -tags e2e ./test/e2e/...` creates a kind cluster, installs Envoy
Gateway from its chart (pinned by digest), builds the image with ko straight
into the cluster, installs the chart from source (with the NetworkPolicy on),
and runs:

| test | checks |
|---|---|
| E1 | a block phase: 503 page for browsers, problem+json for APIs, `Retry-After` |
| E2 | a banner phase: pass-through with the header |
| E3 | an interstitial: the page once, then pass with the session cookie |
| E4 | the bypass header, stripped before the app |
| E5 | fail-open when maintenance-manager is gone |
| E6 | our policy merges with a Gateway-level one (`Merged=True`) |
| E7 | a foreign route policy makes the controller step aside |
| E8 | phase transitions on the clock |
| E9 | uninstall leaves no SecurityPolicy or ReferenceGrant (runs last) |
| E10 | a WebSocket survives the listener drain for 60 s and closes cleanly |
| E11 | the editor through Envoy: forms create, update and delete; cross-site POSTs refused |
| NetworkPolicy | with the chart's NetworkPolicy on (as in every e2e install), a pod in another namespace reaches no port; a proxy-labelled pod in the Envoy namespace reaches authz and the editor, not metrics |

| variable | default | |
|---|---|---|
| `MM_E2E_EG` | `v1.9.1` | Envoy Gateway version (`v1.8.3` or `v1.9.1`) |
| `MM_E2E_CLUSTER` | `mm-e2e` | kind cluster name |
| `MM_E2E_KEEP` | unset | `1` keeps the cluster for poking around |

A full run takes about 5–6 minutes locally and 7–8 on GitHub's runners. Envoy
listens on NodePort 30080, mapped to `127.0.0.1:30080`; tests reach it there,
not through a port-forward, because E10 needs real TCP behaviour.

## Staging only

Some checks need a real platform and run by hand before the first production
install: [HANDOVER.md §13.3](HANDOVER.md) and the test script in §23.
