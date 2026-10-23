# Security

maintenance-manager sits in front of your apps: it answers Envoy's ext_authz
calls and can serve any HTML on every gated app's origin. Reports about the
authz path, the editor, the bypass token, the chart's RBAC or the release
pipeline are welcome and taken seriously.

**Report privately** through GitHub: *Security* tab → *Report a vulnerability*
(<https://github.com/prosperity-solutions/maintenance-manager/security/advisories/new>).
If that button is not there yet, open an issue that says only that you have a
security report (no details), and a maintainer will arrange a private channel.
Please put nothing exploitable in a public issue. Include what you ran, what
you expected and what happened; a reproduction on kind is the fastest way to a
fix.

Supported: the latest release, and `main`. Older releases are not patched;
upgrade to the newest.

How releases are built and verified, and what is and is not trusted, is in
[docs/security.md](docs/security.md) and [docs/release.md](docs/release.md).
