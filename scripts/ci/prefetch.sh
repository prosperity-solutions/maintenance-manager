#!/bin/sh
# Download the product's modules and build the named tools from tools/go.mod,
# retrying with back-off, so the release jobs survive a refused first
# connection. Seen on the first release: harden-runner (egress-policy: block)
# answered "connection refused" for proxy.golang.org seconds after it started,
# although the host is allow-listed. Later `scripts/dev/t <tool>` calls then
# run from the module and build caches.
#
# Usage: scripts/ci/prefetch.sh <tool import path>...
set -eu

[ $# -gt 0 ] || { echo "usage: scripts/ci/prefetch.sh <tool import path>..." >&2; exit 2; }

fetch() {
  go mod download || return 1
  for tool in "$@"; do
    # -o /dev/null: build into the cache only, never a binary in tools/.
    go -C tools build -o /dev/null "$tool" || return 1
  done
}

attempt=1
until fetch "$@"; do
  if [ "$attempt" -ge 5 ]; then
    echo "prefetch: still failing after $attempt attempts. A refused or blocked host in the log means the job's harden-runner allowed-endpoints needs it (docs/HANDOVER.md D1); any other error (go.sum, compile, Go version) is not a network problem and retrying will not fix it." >&2
    exit 1
  fi
  echo "prefetch: attempt $attempt failed; retrying in $((attempt * 5)) s" >&2
  sleep $((attempt * 5))
  attempt=$((attempt + 1))
done
echo "prefetch: modules and tools ready"
