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

Per-share actors serialize decisions while database work and bounded socket writers
run separately. This lifecycle-only storage schema has never admitted payloads;
its recovery uses that evidence, rather than process restart, to establish zero
delivery responsibility. Enabling data transfer requires an explicit recovery design.

`make rust` includes fault-injected commit ordering tests and a Go-codec WebSocket
scenario that kills/restarts the actual binary, resumes ownership, stops the share,
then verifies that another restart cannot republish or resume it.
