#!/usr/bin/env bash
# Run a development server over plain HTTP (no TLS) on localhost.
#
#   scripts/dev.sh [port] [data-dir]        defaults: 18080  ~/lwdev/dev
#
# The first-run setup token is printed in the log and written to <data-dir>/setup-token.txt.
set -euo pipefail

root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$root"

port="${1:-${PORT:-18080}}"
data="${2:-${DATA:-$HOME/lwdev/dev}}"
if [[ ! "$port" =~ ^[0-9]+$ ]]; then
  echo "invalid port: $port" >&2
  exit 1
fi
mkdir -p "$data"
echo "==> http://127.0.0.1:${port}   (data: ${data})"
exec go run ./cmd/ludworks serve --data "$data" --listen "127.0.0.1:${port}" --tls off --log-level "${LOG_LEVEL:-debug}"
