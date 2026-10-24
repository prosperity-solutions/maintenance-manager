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

attempt=1
until go mod download && go -C tools build "$@"; do
  if [ "$attempt" -ge 5 ]; then
    echo "prefetch: still failing after $attempt attempts. If the log shows a refused or blocked host, add it to the job's harden-runner allowed-endpoints (docs/HANDOVER.md D1)." >&2
    exit 1
  fi
  echo "prefetch: attempt $attempt failed; retrying in $((attempt * 5)) s" >&2
  sleep $((attempt * 5))
  attempt=$((attempt + 1))
done
echo "prefetch: modules and tools ready"
