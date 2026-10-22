# Single-connection plan research

[Results](results.json) summarize 51 completed file rounds, including 26 on the physical Linux receiver. [Run records](runs.ndjson) retain hashes, timing, controls and paths. [The path incident](asymmetric-path.json) preserves actual browser selection and sender observations. Raw reports/traces remain in `.tmp/webrtc-plan-revision-20261022/` on Windows. [Follow-up controls](../slowdown-20261022/results.json) found browser background-traffic interference; these earlier performance comparisons do not establish fixed gains or regressions.

These opt-in probes are outside default gates. `supply.test.mjs` characterizes current scheduling defects; its assertions are not behavior to preserve. `ownership_windows_test.go` exercises actual Windows overlapped UDP completion ownership. `portable.patch` freezes the measured experimental batching sources, including temporary switches/deadline coordination; it must not be shipped.

Run from the repository root, using installed Go, Rust, Node and `web` dependencies:

```powershell
node --import ./testdata/connectivity/webrtc/plan-revision-20261022/loader.mjs --test ./testdata/connectivity/webrtc/plan-revision-20261022/supply.test.mjs
go test -race -count=20 -timeout=60s ./testdata/connectivity/webrtc/plan-revision-20261022
& ./testdata/connectivity/webrtc/plan-revision-20261022/prepare.ps1
node --import ./testdata/connectivity/webrtc/plan-revision-20261022/loader.mjs ./testdata/connectivity/webrtc/plan-revision-20261022/full-file.mjs baseline 256 1 0 none 1 lan-isolated
```

File-probe arguments: `baseline|batch`, file MiB, repetitions, extra write delay ms, `none|preview|cancel-preview`, block MiB (`1|4`), `loopback|lan|lan-isolated`. Interleave variants. `loopback` uses installed Edge. Linux probe endpoints come from `WS_LAB_SSH_TARGET`, `WS_LAB_SSH_PORT`, `WS_LAB_SSH_KEY`, `WS_LAB_RECEIVER_HOST`, and `WS_LAB_SENDER_HOST`; defaults are documentation addresses and no key is stored here. The helper owns and cleans up its Chrome profile and tunnels.

`lan` retains production candidate selection. `lan-isolated` filters the research peer to the two Wi-Fi IPv4 host addresses and checks the actual selected pair at start/end. Both use production file/protocol/output components with the existing test settlement adapter. Setup and final hash verification are outside timing. OS caches, OPFS close and headless completion do not establish cold-disk speed, crash durability, UI behavior or resume correctness. Earlier loopback runs used nonpersistent contexts; see each record's output-evidence label.
