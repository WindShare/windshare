# Cloudflare Worker Previews for the Rust relay

Use this runbook for the Worker/Durable Object adapter from the [Rust relay plan](../plans/cloudflare/cloudflare-relay-rust-plan.md), or bounded platform probes before adapter integration. Keep Rust domain tests local and use `wrangler dev` for iteration; Previews provide real Cloudflare execution with isolated Durable Object state.

Official documentation and Wrangler 4.151.1 checked on 2026-10-17. The [host probes](../../spikes/relay-hosts/README.md) exercised a Rust Worker, SQLite, WebSocket callbacks and Alarm execution. Recheck the linked documentation and installed CLI help when tooling changes.

## Preparation

- The developer supplies the Rust `wasm32-unknown-unknown` target and Wrangler account login. Verify the intended account with `npx wrangler whoami`. Cloudflare API MCP authorization is separate and does not authenticate Wrangler.
- Locate the actual Worker package, Wrangler configuration, Rust build command, class exports and registration-secret binding in the current checkout. This runbook does not prescribe their paths or identifiers.
- Pin the Worker package's Wrangler dependency to a version supporting Previews: **4.135.0 or later**. Verify with `npx wrangler --version`; a newer global installation does not replace a project's dependency.
- Run commands from that package through `mcp__ripple`, preserving existing working-tree changes. A first Preview can be created before any production deployment.

See [getting started](https://developers.cloudflare.com/workers/previews/get-started/).

## Configure the Preview

Add a `previews` block to the existing Wrangler file. Keep the Rust build setup and compatibility settings at the top level.

Mirror the adapter's actual classes into `previews.durable_objects.bindings`, retaining the exact binding and exported class names. The relay plan uses ShareRelay and does not introduce DeploymentControl. Same-Worker bindings without `script_name` receive separate namespaces and storage per Preview. Bindings targeting another Worker need separate investigation; they must not be assumed isolated.

Provision the classes with the **SQLite backend**, which is available on Workers Free. Follow the installed Wrangler schema for class exports/migrations. Copying a legacy key-value storage example would invalidate the free-account assumption.

Declare Preview-specific variables and other required bindings explicitly. Account-level resources such as D1, KV and R2 remain shared when their identifiers are shared. The relay plan does not require adding those services.

Enable diagnostic logs, for example with this TOML fragment:

```toml
[previews.observability]
enabled = true

[previews.observability.logs]
enabled = true
invocation_logs = true
persist = true
```

A deployed URL accompanied by missing-binding warnings is an incomplete setup. Fix the configuration before testing the relay.

References: [configuration](https://developers.cloudflare.com/workers/previews/configuration/), [resource isolation](https://developers.cloudflare.com/workers/previews/resources/), [SQLite availability](https://developers.cloudflare.com/durable-objects/platform/pricing/).

## Deploy and iterate

Set `$registrationSecretBinding` to the actual secret binding used by the Rust adapter. For an existing Worker, seed Previews Base with a **test** registration credential before creating a Preview; the command prompts for its value:

```powershell
npx wrangler preview base-config secret put $registrationSecretBinding
```

For a new Worker, Base configuration may return error 10007 until the Worker exists. Create the first Preview with code that rejects protected operations when its secret is absent, then set its secret before running clients. No production deployment is needed. This bootstrap was exercised with the host probe.

Base secret changes affect subsequently created Previews. Update an existing Preview with `npx wrangler preview secret put $registrationSecretBinding --name $previewName`. Keep credential values outside tracked configuration and share links.

Choose a name once and retain it with the run's evidence:

```powershell
$previewName = 'relay-' + [guid]::NewGuid().ToString('N').Substring(0, 12)
npx wrangler preview --name $previewName --json
```

Reuse that name and rerun the deployment command after edits. Record the returned Preview URL, immutable deployment URL, Worker/account identity and exact tested source state. Use the returned address in the client's personal relay configuration, following the implemented endpoint and relay-identity rules; do not invent a WebSocket path.

State survives redeployment within one Preview. Preserve that state when investigating reconnects or STOP; create another Preview when a clean namespace is specifically needed. Preview URLs are public by default, so retain the relay's registration authentication. Cloudflare Access introduces a separate login flow and should not be enabled for ordinary browser receiver tests.

See [Preview lifecycle and URLs](https://developers.cloudflare.com/workers/previews/).

## Exercise WindShare and inspect evidence

Before product integration, use the [bounded host probes](../../spikes/relay-hosts/README.md) to investigate platform contracts. Close callbacks and `CLOSED` do not by themselves satisfy the kernel's buffer-release contract; retain negative and inconclusive results. A platform probe is not a WindShare transfer test.

Use bounded real sender/browser runs against the Preview: registration, browsing and download; actual relay fallback; disconnect/reconnect; hibernation/wakeup; connection replacement; and irreversible STOP across redeployment. Select the scenarios relevant to the change instead of building a large test matrix.

Record the chosen relay, actual Lane, transferred bytes, output integrity and correlated operation/share/session identifiers. Confirm traffic used the Preview data path: a successful P2P transfer alone does not demonstrate relay forwarding. Cloud throughput, memory behavior and quota feasibility need cloud measurements.

Select the specific Preview in the dashboard, then inspect its Observability and Metrics tabs. **`wrangler tail` currently cannot target Previews.** Use persisted Preview logs or the Observability MCP when connected. Cloudflare API MCP can query GraphQL metrics with `isPreview: 1`; retain Worker name and `previewSlug` so results are attributed to the correct run.

See [Preview debugging and metrics](https://developers.cloudflare.com/workers/previews/test-and-debug/).

## Cleanup

After consumers disconnect and evidence is saved, delete only the Preview owned by this run:

```powershell
npx wrangler preview delete --name $previewName
```

Deletion removes its Durable Object state. Retain a Preview needed for later recovery investigation. Free accounts support Previews, but Preview counts are bounded and old entries can be evicted; see [current limits](https://developers.cloudflare.com/workers/previews/#limits).
