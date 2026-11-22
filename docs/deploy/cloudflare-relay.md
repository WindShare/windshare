# Cloudflare deployment

Fork the repository and run **Deploy private relay** in GitHub Actions.
The default `full` mode hosts the receiver frontend and private relay together.
A separate console origin handles owner sign-in, device approval and credentials.
`relay-only` omits the receiver frontend but keeps the console.

`frontend` mode serves static assets and installer redirects without relay storage
or an owner console. It needs no owner password. The official split deployment uses
[network operations](../../deploy/network/operations.md) to publish one build to
Cloudflare and mainland hosts. The steps below cover private relays.

## First deployment

1. Create a Cloudflare **workers.dev** subdomain, or choose two unused hostnames in
   an active Cloudflare zone. Create an account token with **Account / Workers / Admin**.
   Custom domains also need **Zone / Workers Routes / Edit** and **Zone / Zone / Read**.
   See the [token setup guide (中文)](cloudflare-api-token.zh-CN.md).
2. In the fork's **Settings → Secrets and variables → Actions**, set:

   | Kind | Name | Value |
   |---|---|---|
   | Secret | `CLOUDFLARE_API_TOKEN` | Deployment token |
   | Secret | `CLOUDFLARE_ACCOUNT_ID` | Cloudflare account ID |
   | Secret | `WINDSHARE_OWNER_PASSWORD` | Console password, at least 16 characters |
   | Secret | `CLOUDFLARE_WORKER_NAME` | Stable lowercase name, at most 55 characters |
   | Variable | `CLOUDFLARE_RELAY_ENABLED` | `true` |
   | Variable, optional | `CLOUDFLARE_DEPLOYMENT_MODE` | `full` (default), `relay-only`, or `frontend` |
   | Secret, optional pair | `CLOUDFLARE_RELAY_DOMAIN` | Relay hostname, e.g. `share.example.com` |
   | Secret, optional pair | `CLOUDFLARE_ZONE_ID` | That hostname's zone ID |
   | Secret, optional | `CLOUDFLARE_CONSOLE_DOMAIN` | Console hostname in the same zone; defaults to `console.<relay-hostname>` |

3. Enable Actions and run the deployment workflow on `main`. Find the addresses in
   the Cloudflare dashboard under the Worker's **Domains & Routes**. For workers.dev,
   the workflow creates `<name>-console`, bound to the relay Worker.
   Custom domains route both hostnames to one Worker with managed DNS/TLS.
4. [Install or rebuild WindShare](../installation.md) from the deployed source revision,
   then use your relay address:

   ```sh
   wind relay onboard https://<relay-address>
   wind share <path>
   ```

   Sign in to the console with the owner password, verify the device name and code,
   and approve. Automatic mode enables the relay if a slot is free. In selected mode,
   use `wind relay enable <relay-address>`. Pairing keeps the current receiving website.
   The `full` setup receipt offers `wind relay frontend <frontend-address>`. Headless
   clients can display the approval URL and code with `--no-browser`.

The console issues named service keys for automation. Install one with `wind relay add <url> --access-key <key>`.
See [sharing defaults](../cli.md#sharing-defaults). Actions masks deployment addresses,
omits them from summaries, and uploads no address artifact. Local deployments retain
their onboarding output. Receiver links never contain access keys; receivers need no account.

## Updates and access management

Sync the fork's `main` branch. Relay/frontend changes trigger deployment; documentation-only
changes do not. Manual reruns are supported. Keep the account, Worker name and both hostnames
stable. Updates preserve the `SHARES` and `ACCESS` SQLite namespaces and permanent STOP state.
Failed uploads can be retried with the same settings. Unmanaged names or changed storage
bindings fail explicitly instead of replacing another deployment.

Changing `WINDSHARE_OWNER_PASSWORD` and redeploying signs administrators out while keeping
client credentials valid. Revoke individual credentials in the console to block new shares.
Optionally stop that credential's existing shares on this relay; this also blocks RESUME.
Established direct transfers remain independent. Losing a client key requires enrolling again.

Switch `CLOUDFLARE_DEPLOYMENT_MODE` and rerun to move between `full` and `relay-only`.
Both modes retain access and share storage. `CLOUDFLARE_RELAY_ENABLED=false` disables automatic
updates; it does not stop the service. This deployment targets personal, light use within
Cloudflare's service quotas.
