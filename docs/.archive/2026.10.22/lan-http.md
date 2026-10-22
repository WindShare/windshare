# LAN HTTP Research — Deferred

## Decision and rationale

Defer LAN HTTP implementation. Prioritize the existing WebRTC/Relay experience for cross-network sharing through NAT. Direct HTTP requires a sender endpoint reachable by the receiver; its narrower benefit does not currently justify adding listener, discovery, authorization, firewall, and browser-permission lifecycles.

The outline below is retained for reference, not scheduled work or a dependency of WebRTC throughput improvements. Reconsider when a concrete product need justifies the additional transport.

The 2026-10-21 Windows → Linux Chrome LAN experiment transferred the same 118.19 MB file through production signature verification and decryption. Median throughput across three runs per mode was:

| Transport | MB/s |
|---|---:|
| Direct HTTP/TCP | 54.2 |
| Direct WebSocket/TCP | 46.3 |
| WebTransport/QUIC, one stream | 28.0 |
| WebRTC/UDP with the video-buffer workaround | 20.9 |
| WebRTC/ICE-TCP | 11.9 |

HTTP was fastest in this experiment; that alone does not establish a product priority. These measurements used cached sender records and ended in browser memory; live file reads, sender encryption, product scheduling, and disk output can change the result. Public-network performance remains an open question. [Experiment evidence](../../.tmp/tcp-quic-20261021/research.json).

## Deferred implementation outline

1. **Model direct content paths around existing domain contracts.** Reuse sealed block records, revision leases, and the `BlockLane` abstraction. Define the browser counterpart and transport-independent endpoint, authorization, and lifecycle semantics. Keep network I/O in connectivity/transport and assembly in engine; avoid forcing HTTP request/response into a duplex `FrameChannel`.

2. **Add sender-owned LAN endpoint discovery and authorization.** Let `wind.exe` own the HTTP listener, interface selection, firewall integration, and shutdown. Advertise endpoints through the authenticated session instead of embedding IP addresses in share links. Bind expiring request authorization to the session, share, lease, and requested blocks, with replay protection. Preserve content and metadata E2EE; keep secrets out of URLs. Retire endpoints and grants when the network or sharing lifecycle changes.

3. **Connect HTTP to the existing download pipeline.** Stream encrypted blocks on demand into the existing verification, decryption, scheduling, and output flow. Reuse aggregate capacity limits, cancellation, and checkpoints. Bound buffers and request concurrency so large files and multiple receivers do not require full-file memory caching. Retry unfinished blocks through another available route without discarding completed progress.

4. **Integrate browser permission and route selection.** Start the existing Relay bootstrap and WebRTC negotiation without waiting for LAN work. Request initial LAN permission only through an optional user action; an existing grant permits bounded background attempts against sender-advertised endpoints. Private address prefixes alone do not establish LAN reachability. Cache failed attempts per network generation and back off retries. Admit HTTP after confirming sender identity and reachability, preserving active transfers while choosing routes by useful throughput. Handle CORS and HTTPS-to-local-HTTP rules. Listener/firewall setup failures, denied permission, unsupported browsers, and failed probes disable only the LAN path; sharing continues normally.

5. **Use complete downloads to guide tuning.** Compare real file and folder transfers across LAN and cross-network NAT sessions, including concurrent receivers and interruptions. Track time to first catalog response and first file byte alongside throughput to expose delays introduced by optional LAN work. Attribute time to reading, encryption, transport, decryption, and output. Record structured traces for session identity, endpoint generation, route decisions, useful throughput, and fallback reasons; tune the measured bottleneck.

## Later protocol decisions

Consider WebSocket when duplex session traffic or simpler `FrameChannel` integration brings a concrete benefit. Consider WebTransport when measurements in target networks justify its certificate lifecycle and additional transport implementation. Reuse endpoint discovery, authorization, and scheduling across future adapters; implement each adapter when its benefit is established.

Browser integration references: [Local Network Access](https://github.com/GoogleChrome/modern-web-guidance/blob/main/skills/modern-web-guidance/guides/security/local-network-access.md), [WebTransport](https://www.w3.org/TR/webtransport/).
