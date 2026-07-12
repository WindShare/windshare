# Pion ↔ Chromium interoperability spike

This nested module is intentionally isolated from production transports. It
tests the transport facts that D1/D2 need before those adapters are designed.
The HTTP endpoints are only a deterministic harness: every offer, answer, and
ICE candidate still crosses the real `relay/protocol.Signal` encoder/decoder.
ICE is deliberately host-only so the test is hermetic and does not depend on a
public STUN service.

```powershell
Push-Location spikes/webrtc
$env:GOWORK = 'off'
go mod tidy
go test -race -count=1 ./...
go vet ./...
pnpm install --frozen-lockfile
pnpm test
Pop-Location
```

`pnpm test` uses the locally installed stable Google Chrome channel; it does not
download or modify the repository's web toolchain. The harness is single-run by
design, and Playwright starts a fresh server for every `pnpm test` invocation.
