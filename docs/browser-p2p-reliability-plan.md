# Browser P2P Reliability

Status: implemented
Last updated: 2026-08-18

This execution plan has landed. Current behavior is authoritative in
[`协议规范.md`](协议规范.md), security constraints in [`威胁模型.md`](威胁模型.md),
and user-facing semantics in
[`clarifications/即时分享与文件浏览产品澄清.md`](clarifications/即时分享与文件浏览产品澄清.md).

Every preview or download activation makes relay content eligible immediately and starts
one background direct-path recovery supervisor. Browsing alone starts no ICE. Negotiation
has a 15-second budget; local DataChannel Open begins a fresh 20-second lane-admission
budget, leaving at least 5 seconds of the 30-second one-use grant lifetime.

One `PeerPathID` is stable within a ProtocolSession, while every replacement uses a fresh
`AttemptID`, offer operation, grant operation, and grant. Only one attempt may be in flight.
Typed outcomes decide retry, path stop, or reflection of an already-terminal session;
bounded wave/session budgets, backoff, quiescent rearm, and exact detachment recovery never
gate or close a healthy relay.

Lifecycle diagnostics expose bounded identifiers, phases, decisions, and settlement facts.
They exclude SDP, candidate endpoints, nonces, proofs, raw attach frames, capability material,
content metadata, and raw error text; observer failure cannot affect routing or recovery.

Route priority, relay throttling, low-speed scheduling, cost-aware convergence, user timeout
controls, mandatory TURN, new lane cryptography, grant revocation, CLI retry waves, and retries
for permanent authenticated failures remain out of scope.
