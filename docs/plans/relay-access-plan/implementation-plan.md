# Relay Access and Automatic Setup

Build Cloudflare private-relay access around browser approval and automatic client configuration. Share the access model, management API and console with native relays as they gain accounts and resource policies.

## User experience

1. The owner sets `WINDSHARE_OWNER_PASSWORD` in GitHub Secrets and deploys. The workflow prints the console URL and an onboarding command; deployment control establishes ownership.
2. `wind relay add <url>` discovers the access policy through `/.well-known/windshare`. Open relays are added directly; restricted relays open browser approval. Headless clients display a URL and short code for approval on another device.
3. The owner signs in, checks the device name and code, and approves. The client automatically receives its own credential and saves the configuration. Administrator authentication happens in the browser; the WindShare client receives only its own credential.
4. Private-deployment onboarding selects only that private relay. Existing profiles remain saved; an unavailable private relay never silently falls back to public relays. P2P remains enabled. Ordinary relay addition preserves existing selections.
5. The console supports enrollment approval, named credentials, creation and last-use information, and individual revocation with an option to stop that credential's existing shares on this relay. Manual credential creation and configuration import remain available for automation.

Treat saved profiles, active relay selection and the share frontend as separate choices. Discovery suggests a frontend without overwriting an existing choice. Commit local configuration changes together after setup succeeds.

## Shared model

| Concept | Responsibility |
| --- | --- |
| Administrator | Approves enrollment and manages credentials; management authority is distinct from resource ownership. |
| Principal | Stable owner of service usage and entitlements. A private Cloudflare deployment initially has one principal. |
| Credential | Revocable authentication material belonging to a principal. A device name is a label, not a hardware identity. |
| Enrollment | A short-lived approval transaction whose result can be claimed only by the initiating client. |
| Registration grant | Host-resolved authorization for one fresh share registration, identifying its principal and credential. |

Persist principal and originating credential identifiers with each committed registration. Valid RESUME preserves attribution without access-service lookups. Keep the existing bounded recovery window; after expiry, recovery requires fresh authorization using the current credential's attribution. Temporary access failures retry without resetting transfer progress.

Revocation blocks fresh registrations; valid RESUME and signed STOP retain independent authority. Terminating existing shares on this relay is a separate management operation; established P2P transfers remain independent. Receivers need no service account, and sender access credentials never enter share links.

## Implementation sequence

### 1. Establish registration authority

- Add a shared, I/O-free Rust `relay-access` module for credential and enrollment state transitions and access decisions. Hosts supply persistence, time, randomness and administrator authentication.
- Have `relay-core` consume an operation-bound registration grant without depending on passwords, HTTP sessions or account storage. Resolve current credential status at registration admission; a WebSocket tag must not preserve an indefinitely reusable permission. Keep enrollment and raw credentials outside the share kernel.
- Carry permanent access refusal separately from temporary access-service failure through Rust, Go and TypeScript protocol handling. Invalid credentials require user action; temporary failures remain retryable.
- Store share attribution with the registration record. Open registration resolves to an anonymous principal through the same boundary. Keep account lookups and remote authorization off the frame-forwarding path.

### 2. Implement Cloudflare access state

- Add an access Durable Object backed by SQLite, separate from `SHARES`, for principals, credential digests, enrollment transactions and management sessions.
- Use the deployment-provided owner password for administrator authentication, with bounded sessions and login-attempt throttling. Password rotation invalidates management sessions while preserving enrolled credentials.
- Implement discovery, enrollment request/approval/claim and credential management endpoints. Bound enrollment lifetime and polling; bind approval to the initiating client, with retries recovering the same credential. Define cancellation and expiry cleanup for incomplete enrollment and its credentials. Use the [device authorization flow](https://www.rfc-editor.org/rfc/rfc8628.html#section-3) as the approval/polling reference.
- Implement revocation with optional termination of the credential's existing shares through the durable share lifecycle. Administrative termination requires no sender signature and prevents later RESUME.

### 3. Move client configuration into reusable application services

- Move profile persistence and selection rules out of `cmd/wind/internal/shareconfig` into reusable services composed by `engine`. Inject storage and browser-opening capabilities. Replace the deployment-wide `registration_token` configuration with per-credential `access_key`.
- Implement discovery, enrollment progress, cancellation and credential installation once for CLI and desktop. Persist pending enrollment before claiming so interrupted installation can resume. Keep command parsing and presentation in `cmd/wind`.
- Make private-deployment onboarding an explicit application operation that installs the profile and selects it exclusively. A general add operation must not implicitly replace the active relay set or frontend.

### 4. Deliver the shared console and deployment flow

- Build one lightweight console for approval and credential management. Serve its API on the console origin, separate from the share receiver frontend; apply session, cross-site request and content policies at that boundary.
- For custom domains, route separate hostnames to the same Worker. For `workers.dev` full deployments, provision a companion console Worker automatically and connect it to the authoritative access service. Keep the management flow available in relay-only deployments.
- Update `scripts/deploy/cloudflare/` to install `OWNER_PASSWORD`, discovery and console routing. Remove `REGISTRATION_SECRET`, age, `WINDSHARE_CONFIG_RECIPIENT`, encrypted configuration artifacts and their recovery path.
- Preserve relay identity, access state and share storage across deployment updates. Output the automatic private-onboarding command and revise deployment instructions accordingly.
- Correlate enrollment, approval, configuration installation, revocation and registration decisions using operation, enrollment, principal, credential and share identifiers as applicable. Include refusal and retry context without logging passwords or access keys.

## Subsequent integration

Native relays reuse the access module, API and console with their SQLite storage. Establish local administration through an OS-permission-protected channel or explicit administrator authentication; loopback binding alone does not prove ownership. Keep each self-hosted service's identity authority independent.

Account sessions and API keys map to the same principal. Keep admission, concurrent capacity, cumulative usage and bandwidth scheduling separate. Attribute priority allowances across nodes to the principal, allow borrowing idle bandwidth, and preserve transfer progress when priority changes. Remote account and entitlement requests remain outside per-frame forwarding.
