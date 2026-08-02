# Validation

WindShare separates deterministic checks, native integration, product E2E, full browser coverage,
and performance evidence. Windows Firewall and WBEM state never affect a verdict.

| Entry point | Purpose | Routine authority |
|---|---|---|
| `make check` | Root/core short tests plus existing static and Web checks | Developer loop; p95 trend target 60 s |
| `make integration` | Native loopback relay, WebSocket, WebRTC, and process integration | Windows and Linux PR gates; p95 target 2 min |
| `make e2e` | CLI share/get and product lifecycle paths; Windows also owns Chromium smoke | Windows and Linux PR gates; p95 target 3 min |
| `make browser` | Browser-local product matrices plus independently completed 45-identity network evidence | Nightly, release, and protected hosted final |
| `make browser-network` | Token-free consumer of canonical or protected network-completion evidence | Direct diagnostics and `make browser` dependency |
| `make race` | One root and one core race sweep | PR gate |
| `make coverage` | Root/core coverage and package floors | PR gate |
| `make ci` | Current OS mirror of the PR DAG | Local acceptance |
| `make ci-full` | PR DAG plus the public full-browser authority | Nightly/release acceptance |

Coverage remains a hard gate: core total is at least 90%, root total at least 80%, and every
included package at least 70%. Timing targets are trends, not correctness thresholds. The public
API is direct `make <target>` with target names only; Make derives exact HEAD and rejects caller
variable assignments, control flags, hostile inherited Make/Go state, and extra makefiles.

## Real-stack scenario ownership

Unit tests remain component-owned. The table below is the deletion and authority ledger for
cross-component scenarios and the platform containment oracles required by their single-fault model;
a row may be removed only after its hard oracle has another named owner.

| Scenario | Owner / entry | OS and instrumentation | Hard oracle | Bound and cleanup owner |
|---|---|---|---|---|
| `integration/relayv2/real-websocket-frame-exchange` | `integration/relayv2`; `make integration` | Windows/Linux; Go plain, race, coverage | Real relay registration plus exact bidirectional FrameChannel bytes | 10 s; `testscenario.Trace` closes the held loopback listener, relay runtime, and both connections |
| `integration/v2peer/sender-real-pion` | `integration/v2peer`; `make integration` | Windows/Linux; Go plain, race, coverage | Sender negotiation adopts a real loopback Pion DataChannel with the expected lane identity | 10 s; `testscenario.Trace` closes the injected Pion/socket owners and channel |
| `integration/v2peer/receiver-sender-real-pion` | `integration/v2peer`; `make integration` | Windows/Linux; Go plain, race, coverage | Production receiver and sender factories interoperate over real Pion and exchange exact content | 10 s; `testscenario.Trace` owns both peers, the channel, and held sockets |
| `integration/processowner/tree-cleanup` | `integration/processowner`; `make integration` | Windows/Linux; Go plain, race, coverage | Explicit stop returns tree-empty settlement and a retained grandchild identity retires | 20 s target, 10 s stop; external `testprocess.Owner` plus `testscenario.Trace` |
| `integration/processowner/nonzero-exit` | `integration/processowner`; `make integration` | Windows/Linux; Go plain, race, coverage | Natural exit preserves exact code 23 while still proving the tree empty | 20 s; external `testprocess.Owner` plus `testscenario.Trace` |
| `integration/processowner/exact-stdin` | `integration/processowner`; `make integration` | Windows/Linux; Go plain, race, coverage | Private input channel delivers the request-bound bytes exactly once | 20 s; external `testprocess.Owner` plus `testscenario.Trace` |
| `integration/processowner/tree-deadline` | `integration/processowner`; `make integration` | Windows/Linux; Go plain, race, coverage | Owner deadline terminates the retained target and descendants and reports tree-empty settlement | 750 ms owner deadline plus 10 s observation; external `testprocess.Owner` plus `testscenario.Trace` |
| `integration/processowner/client-death` | `integration/processowner`; `make integration` | Windows/Linux; Go plain, race, coverage | Killing the client leaves the platform owner alive long enough to retire retained root and grandchild identities | 10 s readiness, client join, and shared retirement bounds; `parentDeathHarness` identity probes plus `testscenario.Trace` own cleanup |
| `integration/processowner/windows-output-capture` | `integration/processowner`; `make integration` | Windows; Go plain, race, coverage | Capture overflow returns `ErrOutputCaptureLimit` without changing the natural exit-zero, tree-empty settlement; the exact capture limit and repeated result remain stable | 20 s target/wait and 2 s termination grace; `testscenario.Trace` stops the external owner and requires tree-empty cleanup |
| Process-owner start gate plus `windowsjob/TestWindowsSupervisorCrashClosesTheJobLease` / `linuxsubreaper/TestLinuxGuardianRetiresStalledOwnerAndAdoptedTree` | Shared Go/Node vectors and native `internal/processowner` backends; `make browser-process` | Matching native OS; Go plain, race, coverage; Node native | Before release, exact request-bound evidence carries the process instance and volume-16/object-32 executable identity; only its bound decision can release the target, while rejection or pre-release stop proves not-started. Native crash oracles then prove the Job lease or guardian retires every owned identity | 10 s start authority; Windows: 15 s target, 10 s readiness, 3 s observation. Linux: 4 s oracle within a 10 s harness lease; platform owner retains cleanup authority |
| `v2-progressive-catalog` | `e2e`; `make e2e-go` | Windows/Linux; plain or race children; coverage instruments the Go driver only | Two concurrent receivers each publish the exact six-entry directory inventory; `--only tree/nested/a.txt` publishes exactly its two parent directories and selected file | 30 s; `testscenario.Trace` owns every external child tree; the lazy suite fixture owns binaries |
| `v2-pion-relay-cut` | `e2e`; `make e2e-go` | Windows/Linux; plain or race children; coverage driver only | After an observed relay cut, real Pion delivers the exact 32 MiB payload and fixed SHA-256 | 30 s, 5 min child ceiling; `testscenario.Trace` owns child trees, proxy activities, and held sockets |
| `v2-durable-resume` | `e2e`; `make e2e-go` | Windows/Linux; plain or race children; coverage driver only | A crashed receiver resumes only verified durable ranges and publishes exact final bytes | 90 s, 5 min child ceiling; `testscenario.Trace` owns every child tree; the output-root fixture owns placement |
| `v2-invalid-link-diagnostics` | `e2e`; `make e2e-go` | Windows/Linux; plain or race children; coverage driver only | Request-bound target exits with `ExitUsage` 2, empty stdout, and the exact stable diagnostic | 30 s; `testscenario.Trace` owns the external process tree and its tree-empty verdict |
| `v2-relay-only-transfer` | `e2e`; `make e2e-go` | Windows/Linux; plain or race children; coverage driver only | Exact output plus observed relay downstream bytes, with no direct-lane success event | 30 s, 5 min child ceiling; `testscenario.Trace` owns child trees, pause-proxy activities, and held sockets |
| `v2-sender-relay-reconnect` | `e2e`; `make e2e-go` | Windows/Linux; plain or race children; coverage driver only | Recovery emits started/succeeded milestones and a later receiver gets exact bytes | 30 s, 5 min child ceiling; `testscenario.Trace` owns child trees, pause-proxy activities, and held sockets |
| `v2-explicit-stop-tombstone` | `e2e`; `make e2e-go` | Windows/Linux; plain or race children; coverage driver only | Explicit stop survives relay restart and a later join observes the durable stopped outcome | 30 s, 5 min child ceiling; `testscenario.Trace` owns child trees, pause-proxy activities, and held sockets |
| `main-focused-product` Chromium smoke | `browser-smoke`; Windows `make e2e` | Windows Chromium; plain | The production hot-switch download has exact bytes; the browser authenticates the directory descriptor and root before enumeration, proves zero scanner invocations before one identity-bound release, then observes the exact six-entry inventory | 300 s sample lease; the outer Browsergate process owner owns the complete tree |
| Main/Pion focused and remainder partitions | `browser-local`; `make browser` | Chromium/Firefox/WebKit; plain | Discovery is exact and disjoint; every declared product/interop identity passes with trusted settlement | 300 s per sample within the suite budget; one outer Browsergate owner plus per-sample leases |
| 45 scheduled topology identities | Protected producer plus token-free `browser-network` consumer; `make browser` | Dedicated protected Linux runner; 3 profiles x 3 browsers x 5 fresh ordinals; plain | The producer publishes completion only after exactly 45 matching identities; the independent consumer rejects missing, extra, not-evaluated, infrastructure-failed, policy-mismatched, stale, or configuration-drifted evidence | 180 s sample authority plus bounded close; held helper/process owners and the topology runtime own cleanup before completion publication |

### Initial migration evidence epoch

Every scenario row above independently has initial evidence state `new-epoch`; there are no
exceptions. The 2026-08-01 repository evidence audit found no retained, versioned record that binds
pre-migration cold or hot duration, or an instability series, to any of these stable identities.
Accordingly, each row records historical cold timing, historical hot timing, and historical
instability as **not captured**, not as passing or stable.

That provenance fact is closed, not a pending correctness requirement. Independent `-count=1`
candidate samples begin the non-comparable post-migration evidence epoch; hard oracles and cleanup
remain blocking, while timing remains trend-only. Scheduled stability artifacts provide suite-level
correctness history and do not retroactively manufacture per-scenario timing.

Manual real-NAT observations are supplemental and non-authoritative. They never satisfy, replace, or
change the scheduled 45-identity verdict.

The scheduled stability workflow runs native integration once on Windows and Linux and publishes
one versioned result per OS. Readiness reducer v6 seeks the latest 100 valid samples per OS; fewer
returns `insufficient-history` with exit zero and does not evaluate resolution or call the compare API.
A finding key binds execution-contract schema and semantic digest, OS, suite, and the stable product
termination tuple. Two distinct run/artifact observations reproduce a finding; a reproduced unresolved
finding blocks, while one observation remains tracked and nonblocking.

Resolution requires 20 distinct newer passing run/artifact samples after the latest matching failure,
each on a commit proven by the compare API to be a strict descendant; the same corrected commit may
repeat, and a later matching failure resets the sequence. Resolved findings are `trend-only-resolved`.
Diverged or missing comparisons remain unresolved; malformed or transport API, parsing, and publication
failures are fatal. The current-commit full PR and browser job runs independently with `if: always()`.

Full network production runs only in the protected `browser-network-matrix` environment on its
dedicated Linux runner. Its broker privately verifies the exact eight-root/three-profile inventory
before OIDC minting and publishes token-free completion only after child success and cleanup. Public
`make browser` defaults to `test-results/browser-network-completion.json`; hosted final validation
uses a retained launcher that locks the exact commit, Make/parser/shell, and completion descriptor.
Runtime configuration remains broker-only environment, never a public Make path operand.

Before switching a migrated integration or Go E2E path to authority, run it in 20 independent
processes on both Windows and Linux with `-count=1`, no retries, and no reproducible failure. Each
scenario owns its oracle, timeout, cleanup, and structured events containing `run_id`,
`operation_id`, `scenario`, `component`, `milestone`, and `outcome`. Cleanup must prove the owned
process tree and socket owners are gone; rebinding a port is not a cleanup oracle.
For Go scenarios, `testscenario.Trace` keeps the bounded authoritative event journal, separates
`prior_evidence_outcome` from external `prior_delivery_outcome`, and gives reverse-order cleanup
owners fair shares of one mechanically enforced terminal lease.

Performance publication has a separate contract in [Performance evidence](performance.md) and
never authorizes network access or schedules correctness tests.
