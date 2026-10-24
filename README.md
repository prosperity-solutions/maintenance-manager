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

## The editor

A plain web editor ships with the chart: a dashboard (which windows are armed or
active, which routes, whether Envoy accepted the policy, and conflicts in red),
forms for services, pages and windows, a page preview, and **Start next phase
now**. It listens on port 8080 inside the cluster only:

```sh
kubectl -n <namespace> port-forward svc/maintenance-manager 8080
# open http://localhost:8080, user admin, the password from editor.password
# (or the editor-password key of editor.existingSecret)
```

Want a URL instead? Point an HTTPRoute of your own at the `maintenance-manager`
Service, port 8080, and put TLS in front of it.

> **Treat the editor password like a deploy credential.** Whoever holds it can
> serve any HTML and JavaScript on every gated app's origin. It must be at least
> 12 characters (shorter, the editor stays off). Login attempts are limited to
> one per second per replica while someone is guessing.

## Licence

MIT. See [LICENSE](LICENSE) and [NOTICE](NOTICE).

---

Envoy® and Kubernetes® are registered trademarks of The Linux Foundation. This
project is not affiliated with or endorsed by The Linux Foundation.
