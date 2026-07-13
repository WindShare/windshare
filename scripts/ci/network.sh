#!/usr/bin/env bash
# Network gate (Linux): intentionally a no-op. The gate exists for Windows,
# where OS-network test cases gate-skip outside the D5 fixed-path runner and
# need a dedicated -race pass through it. On Linux the constructors
# (internal/testnetwork) are open, so the real-socket cases already ran
# ungated inside `make race` and `make coverage` — exactly as ci.yml's ubuntu
# jobs execute them. There is nothing separate to run here.
set -euo pipefail

echo "== network =="
echo "no separate network gate on Linux: real-socket cases run ungated in the race and coverage gates"
echo "== network: PASS in 0s =="
