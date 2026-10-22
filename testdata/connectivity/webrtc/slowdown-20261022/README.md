# Same-file throughput investigation

[Results](results.json), [run records](runs.ndjson), and [browser background traffic](background-network.json) preserve the evidence. Raw reports and traces are in `.tmp/webrtc-slowdown-20261022/`; cached-record controls are in `.tmp/tcp-quic-20261021/slowdown-*.json`. Logging runs are diagnostic, not throughput baselines.

Current production complete-file transfer still reaches 20.33 MB/s. NetLog attributes about 37 MB of overlapping QUIC traffic to Chrome optimization-guide model downloads; controlled DNS reversals remove the recurring second-round dip, but other variation remains. The new interleaved comparisons do not establish a stable batching gain or the earlier preview regression.

These opt-in probes do not enter default gates. They reuse production transfer/output components and the existing test settlement adapter. File timing ends at output close; setup and final SHA-256 are excluded. This is not UI, crash-durability, WAN or resume evidence.

Prepare binaries with `../plan-revision-20261022/prepare.ps1`, then run from the repository root:

```powershell
$env:WS_RESEARCH_ISOLATE_BROWSER_DNS = '1'
try {
  node --import ./testdata/connectivity/webrtc/slowdown-20261022/loader.mjs ./testdata/connectivity/webrtc/slowdown-20261022/full-file.mjs baseline 256 1 0 none 1 lan-isolated
} finally { Remove-Item Env:WS_RESEARCH_ISOLATE_BROWSER_DNS }
```

Arguments match the earlier file probe. Alternate `baseline` and `batch`; use identical fixtures through `WS_RESEARCH_FIXTURE`. Isolation changes only the owned Chrome's resolver; it retains localhost signaling and the explicit IPv4 LAN WebRTC pair. TCP controls bracket each browser batch, not each repetition. Receiver socket counters are matched to the selected port.

LAN probes read `WS_LAB_SSH_TARGET`, `WS_LAB_SSH_PORT`, `WS_LAB_SSH_KEY`, `WS_LAB_RECEIVER_HOST`, and `WS_LAB_SENDER_HOST`; defaults are documentation addresses. `WS_RESEARCH_OUTPUT=memory`, `WS_RESEARCH_TRACE=off`, `WS_RESEARCH_LARGE_BUFFER=1`, and `WS_RESEARCH_NETLOG=1` are independent diagnostic controls. The large-buffer shim affects only the owned Linux Chrome and is not a browser-product solution. `prepare-native-stats.mjs` generates isolated Go overlays and `diag.mod`; build with both `-modfile` and `-overlay`, then select its directory with `WS_RESEARCH_BINARY_DIRECTORY`. It adds read-only SCTP/process sampling; production sources and the module cache are unchanged.

Run `analyze.mjs`, `native-summary.mjs`, `endpoint-summary.mjs`, then `archive.mjs` to regenerate the compact evidence. Older throughput comparisons remain historical measurements, not a demonstrated fixed gain.
