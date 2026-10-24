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
Service, port 8080, at path `/` (the editor's links are root-relative), and put
TLS and your own login (OIDC, an IP allowlist) in front of it. Keep that route
out of every MaintenanceService's selector, or a block phase blocks the editor
too.

If the editor is ever out of reach, stop a window from the command line:

```sh
kubectl -n <namespace> patch mmw <window> --type=merge -p '{"spec":{"enabled":false}}'
```

> **Treat the editor password like a deploy credential.** Whoever holds it can
> serve any HTML and JavaScript on every gated app's origin. It must be at least
> 12 characters (shorter, the editor stays off). Login attempts are limited to
> one per second per client address, so a guessing pod does not lock out your
> port-forward.

## Telling users in the app

Most apps want a banner before maintenance starts, not just a page during it.
Add the client, a single dependency-free file (under 4 KB gzipped), to your
pages:

```html
<script src="/static/maintenance-manager.js"
  data-dismiss="click" data-delay="8" data-poll="60" data-locale="de-CH" defer></script>
```

- **Self-host it** (recommended, works with a strict CSP `script-src 'self'`):
  every release attaches `maintenance-manager.js`, with its checksum in
  `SHA256SUMS`.
- Or load it from jsDelivr, pinned to a release:
  `https://cdn.jsdelivr.net/gh/prosperity-solutions/maintenance-manager@vX.Y.Z/client/maintenance-manager.min.js`.

| attribute | values | default |
|---|---|---|
| `data-dismiss` | `auto` (after `data-delay` seconds) or `click` | `click` |
| `data-delay` | seconds | `8` |
| `data-poll` | seconds, 5–3600; keep it under 900 so a banner shows before a window's 15-minute arming lead time runs out | `60` |
| `data-locale` | BCP 47 tag | the browser's language |

While the tab is visible, it asks `/__maintenance-manager/status` on your own
host. That path only answers while a window is armed; otherwise your app
answers it (usually a 404), the client shows nothing and polls less often (up
to every 5 minutes). Banner and interstitial phases show a banner at the top;
a block phase covers the page, and when it ends every open tab reloads within
30 seconds (spread out, so they do not all hit the app at once). It renders in
a Shadow DOM; restyle it with `--mm-bg`, `--mm-fg`, `--mm-accent`, `--mm-font`
and `--mm-z` on `#maintenance-manager`. Your app may answer the status path
itself with a cheap `204` to save the 404.

The status path sits at the root of your host, so it reports on the route that
serves `/`. If your host is split into path-scoped routes (say `/app` and
`/api` as separate HTTPRoutes), make sure the route serving `/` belongs to the
same MaintenanceService, or the banner never shows for the others. A dismissed
banner comes back when the window's end time changes. It works under a strict
CSP (`script-src 'self'`, Trusted Types): its styles are a constructed
stylesheet, not inline CSS.

## Licence

MIT. See [LICENSE](LICENSE) and [NOTICE](NOTICE).

---

Envoy® and Kubernetes® are registered trademarks of The Linux Foundation. This
project is not affiliated with or endorsed by The Linux Foundation.
