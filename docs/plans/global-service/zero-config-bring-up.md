# Zero-configuration network bring-up

## Purpose and current scope

Make the development build support the everyday flow: select files or folders → first relay admission and local session readiness → usable link → browser download.

Use the existing cn1 STUN and US1 relay/STUN services. The current work is application configuration, desktop build dependency tracking, and a small correction to desktop creation states. Public distribution, GitHub Releases, deployment automation, account admission, and fleet expansion belong to later work.

## Analysis and decisions

The repository already has signed-directory verification, background refresh, a bounded relay planner, application-injected defaults, and native/browser consumers. The current release manifest has no trust roots or snapshot, and the directory source contains only cn1 with the STUN role. Enabling these existing owners addresses the default-sharing gap without introducing another configuration framework.

| Area | Effect and trade-off |
|---|---|
| Everyday sharing | Removes manual relay setup. Valid embedded/cached directories and configured relays remain immediately usable; background maintenance adds no steps to this path. |
| Self-hosting | Explicit relay intent, configured-only mode, and frontend overrides retain their existing meanings. Automatic defaults must not rewrite saved choices. |
| Architecture | Pure trust verification stays in core, selection stays in the planner, and fetching/persistence stay in the directory runtime. Applications supply built-in infrastructure. |
| Developer experience | Native and browser builds share one trust manifest. This requires matching development artifacts but avoids separate configuration implementations. |
| Failure handling | Distinguishing unavailable infrastructure from missing settings adds a small state-model cost and prevents users being asked to configure a server during an outage. |
| Operations | Signed updates avoid client releases for ordinary node additions. Formal key custody, renewal scheduling, and publication become ongoing duties when public service begins. |

One usable relay is sufficient to start. cn1 improves the STUN candidate pool, but initial relay registration and signaling still depend on an accessible relay; domestic STUN alone does not remove that dependency. Additional servers should follow an observed availability or capacity need.

The current complexity is justified by default sharing and updateable infrastructure. Keep the implementation within the existing owners; a general recovery framework or new fleet control service would exceed this work's benefit.

## Execution

### 1. Prepare an isolated development network identity

Use a dedicated local build checkout for development trust material and generated public artifacts. Private keys stay outside every checkout. The main release manifest can remain unprovisioned until a deliberate public distribution decision; builds using that unprovisioned manifest still require explicit relays.

Generate two development root keys and one directory-signing key with the existing `network-sign` tool. Install the public roots in that checkout's `deploy/network/release.json`, authorize the signing key with one root, and sign the directory with the signing key. Use a coherent generation and current validity dates: directory issuance cannot precede the signing certificate's validity.

The two roots are alternative trust anchors, not a two-signature approval requirement. Reuse the development identity and public artifacts for routine builds; ordinary code changes need no key regeneration or trust-cache reset. Identity replacement need not preserve compatibility for nonexistent external users. Keep development signed history and local application state separate from future production identity.

### 2. Populate the initial directory and matching builds

Use the existing source and signer rather than an additional server list:

| Alias | Endpoint | Services | Zone |
|---|---|---|---|
| `cn1` | `https://cn1.ourworld.top` | STUN at `stun:cn1.ourworld.top:3478` | `cn` |
| `us1` | `https://us1.windshare.cc` | Relay and STUN at `stun:us1.windshare.cc:3478` | `global` |

Complete the remaining node metadata from the actual deployment. Keep mainland relaying disabled and `global_pair_covers_mainland=false`. Directory roles describe the enabled services; the server owns enforcement.

Leave temporary cn2 outside this initial scope. Alias bindings attach to canonical endpoints, so replacing a machine or IP while retaining the endpoint does not itself require a new alias.

Signing already updates the embedded snapshot, mirror export, and signed history. Increment serial for each changed payload within a generation; it publishes a complete new snapshot, not a delta.

Build CLI, desktop, and Web from the same development manifest. Set `link_host` to a development frontend reachable by the intended receiving devices; leave `backup_entry` empty or point it to the matching development Web build. Use only mirrors belonging to the same development authority. The valid embedded snapshot provides initial candidates without requiring a mirror fetch.

Add `deploy/network/release.go` and `deploy/network/release.json` to the dependencies tracked by `desktop/crates/windshare-host/build.rs`, so incremental builds rebuild the Go host library when its embedded configuration changes.

The browser must receive the matching Web build because its roots are compiled into JavaScript. Copying a signed directory onto a site built without those roots cannot enable alias links. Use local or development hosting, with its browser Origin accepted by US1. Complete bring-up with a real browser download through a generated link using development settings without explicit relays. GitHub Releases and official frontend publication are not dependencies.

Keep development settings and their adjacent `config.json.state/network/` data together in an isolated location, and use a separate browser profile for development trust. An empty file is not a valid settings document, and clearing settings alone does not clear directory trust history.

### 3. Correct retained desktop creation states

Distinguish `awaitingConfiguration` from temporary `awaitingNetwork` in retained creation intents, carrying the reason and applicable actions through the desktop host contract, Rust model, and presentation. For `engine.ErrNetworkUnavailable`, expose typed blocking reasons and the next allowed refresh time through `networkdirectory`, `networkruntime`, and the public engine API; the desktop must not infer them from error strings.

- An absent default settings file is normal zero-configuration startup; only invalid settings or missing configuration required by explicit user choices lead to configuration actions.
- Temporary directory unavailability retains the selected files and creation operation, explains the temporary failure, and offers retry or cancellation.
- Retry uses the existing admission path and directory refresher, including its coalescing and backoff. Where cooldown prevents a new attempt, expose the refresher's retry timing instead of presenting it as a newly started connection.
- Unprovisioned builds and unreadable or invalid local trust state need an accurate reason and a configuration or manual recovery action. Keep rare trust-state repair manual.
- Keep self-hosted configuration reachable as a secondary action independently of the failure state, in both host commands and presentation. Reuse the existing form so unprovisioned builds still support explicit relay setup.

Keep retry timing and trust decisions with their current owners. The desktop presents those decisions; it does not gain a separate directory poller, probe scheduler, or automatic repair subsystem. Relay connection retries after task creation retain their existing lifecycle; richer progress presentation is optional follow-up and does not block bring-up.

Relevant owners: `engine/networkdirectory`, `engine/internal/networkruntime`, the public engine API, `host/desktop/creation.go`, the host operation/protocol model, `desktop/crates/windshare-host`, and the desktop live-sharing projection and setup views.

### 4. Preserve the normal sharing path

Continue using the current planner, admission coordinator, and readiness contract. Do not wait for a second relay, P2P establishment, descendant discovery, frontend health checks, or directory refresh when usable candidates already exist.

When default sharing has neither a usable snapshot nor configured relays, use the existing bounded refresh where available, then present the actual blocking reason and applicable actions.

Directory updates affect subsequent selection. Existing shares retain their published candidate set; adding a server does not silently rewrite their links or migrate active transfers. Keep this boundary explicit instead of expanding scope into live fleet migration.

## Key custody

| Material | Storage and use |
|---|---|
| Development private keys | Repository-external encrypted local storage, isolated from production. Disposable identities are sufficient for this phase. |
| Two formal root private keys | Separate encrypted offline media, such as separately stored removable drives. Used to authorize signing generations; not supplied to GitHub Actions. |
| Directory-signing private key | Initially local encrypted storage for manual signing. A dedicated GitHub Environment Secret is an option only if automated signing is later chosen. Retain an independent encrypted copy. |
| Deployment tokens and dedicated SSH private keys | Corresponding GitHub environment secrets when automated deployment is introduced. |
| Public roots, signed certificates, directory exports, and signed history | Versioned public artifacts. Their secrecy is unnecessary; consistency and provenance matter. |

The current key generator writes unencrypted PEM files. Encryption must come from the storage containing them. The term “online signing key” describes its routine signing role; it does not require server hosting.

The existing official-network workflow consumes already signed data. Local signing followed by automated publication is sufficient even after releases begin, so CI need not hold any directory-signing private key. GitHub Secrets supplies credentials to workflows; formal root custody remains independent of those workflows.

## Later, when public distribution is planned

Choose and preserve the formal trust identity, then build the publicly distributed native and Web artifacts with it. Establish a lightweight renewal responsibility before operating that identity: payload lifetime is at most 90 days, signing-certificate lifetime at most 180 days, and the payload grace period cannot extend beyond certificate expiry.

Schedule mirror publication, GitHub environments, installer distribution, and release automation at that stage. Keep account admission and fair sharing of relay capacity in their existing P2 scope. Expand the fleet according to actual coverage and load; server count alone is not a public-readiness claim.

These later responsibilities do not block the development flow above.
