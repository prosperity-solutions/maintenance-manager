# Maintenance Manager for Envoy®

**Flip a switch, your apps show a maintenance page. Flip it back, they're live. Their routes never notice.**

A self-contained Helm chart that puts web apps behind **maintenance pages,
interstitials and banners** on any Kubernetes® cluster running **Envoy Gateway
≥ 1.8**, without touching the apps or their HTTPRoutes.

- **Hands off your routes.** It only reads HTTPRoutes, so it works where a
  platform owns and regenerates them.
- **Zero overhead at rest.** No window armed, no extra hop: Envoy never calls it.
- **Fail-open.** If it is down, slow or misconfigured, traffic flows as if it
  was never there.
- **Plays nice with platform auth.** Its policy merges into Gateway-level
  SecurityPolicies instead of replacing them.
- **No database.** State lives in three custom resources.

> 🚧 Under construction. The first release, install-by-digest and verify
> commands land here as it ships. The full design is in
> [docs/HANDOVER.md](docs/HANDOVER.md).

## Licence

MIT. See [LICENSE](LICENSE) and [NOTICE](NOTICE).

---

Envoy® and Kubernetes® are registered trademarks of The Linux Foundation. This
project is not affiliated with or endorsed by The Linux Foundation.
