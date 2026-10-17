# Native relay lifecycle host

This first native host runs real WebSocket REGISTER, RESUME, independent STOP and
heartbeat control through `ShareRelay`. It requires the capacity-bearing handshake.
JOIN and opaque forwarding are explicitly unavailable; the Go relay remains the
product server until the delivery runtime, STUN and operational endpoints are ready.

From `relay-rust/`:

```sh
cargo run --locked -p windshare-relay-native --bin relay-native -- --database relay.sqlite --listen 127.0.0.1:8787
```

For a reverse proxy, add `--public-endpoint wss://relay.example/v2/ws` (a canonical
full WebSocket URL, including any base path). TLS terminates at that proxy.
The database is bound to this endpoint identity and exclusively leased by one host.
Keep the SQLite database and its WAL together; deleting storage deletes STOP history.
The parent directory must exist. JSON readiness goes to stdout; structured traces
go to stderr, with kernel decisions enabled by `RUST_LOG=debug`.

SQLite transactions atomically publish the descriptor and unresolved record, then
conditionally commit ownership. STOP retains its permanent intent through cleanup
and commits the final fact before acknowledgement. Ambiguous results remain fenced
and retry the same obligation; operation-scoped cleanup cannot erase another owner.
Startup restores pending work and original persisted grace deadlines. Generation
allocation is durable; challenge entropy comes from the OS.

Per-share actors serialize decisions; each connection supervisor owns separate reader
and writer tasks. Slow writes cannot block inbound controls. Output has independent
frame/byte bounds, reserved control capacity and coalesced priority heartbeat replies.
Unsubmitted payloads stay with the actor. The connection foundation rechecks a receipt
through `ShareRelay::submit` and hands it to the bounded writer synchronously; those
bytes are then submitted responsibility and may still arrive after STOP.

Queue discard precedes its kernel receipt. Closure aborts both tasks, or gives ordered
terminal replies and the WebSocket close handshake one bounded interval; shutdown
interrupts that drain.
Only joined tasks, dropped queues and abortive TCP teardown justify `Closed`;
shutdown waits until the share actor has processed that receipt.
This lifecycle-only storage schema has never admitted payloads; its recovery uses that
evidence to establish zero delivery responsibility. JOIN/data ingress still requires
delivery recovery and coordinated client runtime integration.

`make rust` covers blocked-write ingress, bounded queues, STOP submission fencing,
joined teardown, fault-injected commit ordering and a Go-codec WebSocket scenario
that kills/restarts the actual binary, resumes ownership, stops the share,
then verifies that another restart cannot republish or resume it.
