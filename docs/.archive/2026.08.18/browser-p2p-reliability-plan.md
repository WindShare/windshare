# Browser P2P Reliability Execution Plan

Status: planned
Last updated: 2026-08-17

## Goal

Start relay content and browser P2P together for every preview or download activation. Relay carries usable traffic immediately while P2P negotiates and recovers in the background; neither path gates or cancels the other.

The observed Chrome failure crossed ICE, DTLS, SCTP, and DataChannel Open, then expired during authenticated lane admission. The current single 10-second deadline and no-retry policy turn one transient admission delay into permanent relay-only use.

## Design decisions

- Replace the current 0/8 content-admission policy: activation immediately admits the established relay lane and starts P2P, independent of content size.
- Keep both admitted lanes eligible for concurrent block traffic. Route priority, relay throttling, low-speed scheduling, and cost-aware convergence are later work.
- Split each peer attempt into explicit `negotiation` and `lane-admission` phases. DataChannel Open ends negotiation and starts a fresh admission budget on that endpoint.
- Keep the existing 30-second lane-grant TTL as a hard protocol bound. The admission budget must leave an explicit delivery and processing margin; unused grants expire naturally, and every retry obtains a fresh grant. This work does not add a grant-revocation protocol.
- Classify failures into `retry-attempt`, `stop-path`, and `stop-session` from typed outcomes. The classifier cannot upgrade failure authority: `stop-session` only reflects an already-terminal ProtocolSession. Local transient negotiation, timeout, or transport failures and authenticated WS2N `grant-expired` or `admission-limited` outcomes may create a fresh attempt; peer `OPERATION_ERROR` remains final for its exact operation, while identity, proof, binding, protocol, and policy failures remain terminal at their authoritative scope.
- Permit one in-flight direct attempt per ProtocolSession and bound both each recovery wave and the session lifetime by named attempt and elapsed-time budgets below sender evidence capacity. Retries use exponential backoff with jitter, honor authenticated `RetryAfter`, and obtain a fresh AttemptID, operation, and lane grant. Budget exhaustion enters a quiescent `stop-path` state without closing relay or the ProtocolSession; a later activation or browser network-change signal may rearm recovery only while session budget remains.
- Keep diagnostics observational: logging failures must never affect negotiation, admission, retry, or lane routing.

## Work sequence

### 1. Make relay and P2P immediate peers

Remove the fallback timer and size-class branches from browser connectivity. At activation start, make relay content eligible and launch the P2P attempt; browsing-only sessions still create no P2P work.

Keep activation ownership unchanged so closing the last preview or download stops pending peer recovery without affecting unrelated session work.

### 2. Refactor peer-attempt phases

Replace `AttemptTimeout` with phase-owned negotiation and admission deadline configuration in Go and the browser. Model phase transitions explicitly and preserve the original typed cause when timeout, teardown, or cancellation races admission.

Support early browser Open, delayed Pion Open, delayed grant delivery, and queued LaneHello within the bounded grant lifetime. Admission settlement becomes authoritative once authenticated LaneHello processing starts; cleanup closes owned transports exactly once.

### 3. Add typed background recovery

Introduce one browser recovery supervisor and a pure failure classifier. A transient failure leaves relay traffic running, waits with bounded backoff, and creates a fresh attempt while an activation remains live.

Stop the current retry wave after admission succeeds, its attempt or elapsed-time budget is exhausted, the last activation closes, or the classifier returns `stop-path` or `stop-session`. Track a ProtocolSession-wide attempt budget so repeated waves cannot exhaust sender evidence or retained lane-grant capacity. Peer detachment starts a new bounded wave through the same supervisor instead of a separate reconnect path.

### 4. Expose and validate the lifecycle

Add structured milestones for phase deadlines, lane-grant request and receipt, LaneHello send and receipt, admission response, retry decision, and attempt replacement. Correlate protocol session and peer attempt identities without recording SDP, candidate addresses, nonces, proofs, or capability material.

Add deterministic tests for immediate dual-path activation, asymmetric Open schedules, delayed grant and LaneHello, timeout/admission races, grant expiry and cleanup, failure classification, retry backoff and `RetryAfter`, wave and session budget exhaustion, rearming, detachment, and shutdown. Prove exhausted P2P recovery leaves relay and the ProtocolSession usable. Use injected clocks, randomness, and gates instead of sleeps.

Extend the real Chromium/Pion product scenario with a controlled admission delay. Prove relay traffic starts while P2P is pending, authenticated peer traffic begins after recovery, detachment retries without interrupting relay, and the transfer completes correctly.

## Primary code areas

- `web/src/connectivity`: immediate relay eligibility, peer phases, recovery supervisor, and typed decisions.
- `web/src/receiver`: activation continuity across session generations and recovery rearm ownership.
- `connectivity/v2peer`: sender phase state, deadlines, admission settlement, and typed failure evidence.
- `core/session/protocolsession` and `core/session/sessionruntime`: bounded grant and authenticated lane-admission lifecycle.
- `web/src/session`: grant and LaneHello admission milestones.
- `cmd/windshare/internal`: safe trace projection for the new lifecycle facts.
- `web/e2e` and transport integration suites: delayed Chromium/Pion admission and recovery scenarios.
- `docs/协议规范.md`, `docs/威胁模型.md`, product clarification, and user-facing behavior docs: replace stale 0/8 and retry semantics when implementation lands.

## Validation flow

Run focused Go and web tests during each refactor, followed by the Chromium recovery scenario and repeated hot-switch runs. Finish with `make check` and `make ci`.

## Out of scope

This work does not add route priority, relay throttling, low-speed scheduling, cost-aware convergence, user-facing timeout controls, mandatory TURN, new cryptographic lane semantics, or retries for permanent authenticated failures.
