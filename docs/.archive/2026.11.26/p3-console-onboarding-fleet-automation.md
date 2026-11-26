# P3 — Console onboarding and fleet operations

Part of the [Global Network Plan](../../plans/global-service/global-network.md#6-phases).

## Enroll and publish

The console fetches the signed descriptor through the authority edge after session and CSRF validation. Discovery requires public HTTPS, rejects redirects and protected destinations, and verifies the node identity, advertised services, authority URL and issuer. A setup command supplies public trust to new hosts.

Enrollment binds the alias permanently and publishes the node as Canary. The authority imports verified release history, signs topology changes, renews the 90-day directory, and retries KV delivery after the SQLite commit. Official frontends serve the KV-backed directory and mainland hosts mirror it with a verifying systemd timer. The operator console shows the signed revision, KV delivery and expiring signing certificates.

Senders start with dialable Active relays and fill missing registration slots with Canary relays. If every initial Active registration fails before publication, standby Canaries can join the same share. The first admission publishes the selected hints, including pending routes. Receivers can dial known endpoints while unresolved aliases refresh.

## Operate nodes

A signed minute heartbeat reports load, metered budget, resident shares and applied policy. Operators can activate, drain, disable, remove or change anonymous access. Drain keeps existing shares joinable and resumable. Disable and removal require drain evidence or a confirmed early cutoff. STUN-only nodes refuse relay admission.

[Operator procedures](../../../deploy/network/operations.md#test-a-new-node) cover directed relay and UDP STUN tests, host upgrades with local rollback, certificate monitoring and mainland mirror alerts. The authority holds the online signing keys. Host access, frontend certificates, DNS credentials and user E2EE keys stay with their respective owners.
