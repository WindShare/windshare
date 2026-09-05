#!/usr/bin/env bash
# A source checkout and a release binary bundle share this installation entry.
set -euo pipefail
source_root="$(cd "$(dirname "$0")/../.." && pwd -P)"
destination="${1:-$HOME/.local/bin}"
mkdir -p -- "$destination"
destination="$(cd "$destination" && pwd -P)"
# Source identity takes precedence over untracked executables left by local builds.
if [ -e "$source_root/go.mod" ]; then
  (
    cd "$source_root"
    export GOWORK=off
    go run ./scripts/ci/_piondeps
    go build -trimpath -buildvcs=false -o "$destination/wind" ./cmd/wind
  )
elif [ -f "$source_root/wind" ]; then
  install -m 0755 -- "$source_root/wind" "$destination/wind"
else
  echo "Expected a complete source checkout/source bundle or a release binary bundle." >&2
  exit 1
fi
echo "Installed $destination/wind. Automatic firewall setup is unavailable on this platform; sharing can use relay."
