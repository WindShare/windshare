# Cloudflare deployment

Fork the repository and run **Deploy WindShare to Cloudflare** in GitHub Actions.
The default `full` mode hosts the receiver frontend and private relay together.
A separate console origin handles owner sign-in, device approval and credentials.
`relay-only` omits the receiver frontend but keeps the console.

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
   | Variable | `CLOUDFLARE_WORKER_NAME` | Stable lowercase name, at most 55 characters |
   | Variable | `CLOUDFLARE_RELAY_ENABLED` | `true` |
   | Variable, optional | `CLOUDFLARE_DEPLOYMENT_MODE` | `full` (default) or `relay-only` |
   | Variable, optional pair | `CLOUDFLARE_RELAY_DOMAIN` | Relay hostname, e.g. `share.example.com` |
   | Secret, optional pair | `CLOUDFLARE_ZONE_ID` | That hostname's zone ID |
   | Variable, optional | `CLOUDFLARE_CONSOLE_DOMAIN` | Console hostname in the same zone; defaults to `console.<relay-hostname>` |

3. Enable Actions and run the deployment workflow on `main`. Its summary prints the
   relay URL, console URL and onboarding command. For workers.dev, the workflow creates
   `<name>-console` automatically, bound to the relay's access storage. Custom domains
   route both hostnames to one Worker with managed DNS/TLS.
4. With [WindShare installed](../installation.md), run the printed command:

   ```sh
   wind relay onboard https://<relay-address>
   wind share <path>
   ```

   Sign in to the console with the owner password, verify the device name and code,
   and approve. The client saves its own credential and selects only this private relay.
   Existing profiles and frontend choices remain saved; P2P stays enabled. Headless
   clients can display the URL and code with `--no-browser`. In `relay-only` mode, pass
   `--front-url` when sharing unless a frontend is already saved.

The console supports named manual credentials and a JSON configuration for automation.
See [sharing defaults](../cli.md#sharing-defaults). Deployment artifacts contain addresses
and identifiers only. Receiver links never contain access keys; receivers need no account.

## Updates and access management

Sync the fork's `main` branch. Relay/frontend changes trigger deployment; documentation-only
changes do not. Manual reruns are supported. Keep the account, Worker name and both hostnames
stable. Updates preserve the `SHARES` and `ACCESS` SQLite namespaces and permanent STOP state.
Failed uploads can be retried with the same settings. Unmanaged names or changed storage
bindings fail explicitly instead of replacing another deployment.

Changing `WINDSHARE_OWNER_PASSWORD` and redeploying signs administrators out while keeping
client credentials valid. Revoke individual credentials in the console to block new shares.
Optionally stop that credential's existing shares on this relay; this also blocks RESUME.
Established P2P transfers remain independent. Losing a client key requires enrolling again.

Switch `CLOUDFLARE_DEPLOYMENT_MODE` and rerun to move between `full` and `relay-only`.
Both modes retain access and share storage. `CLOUDFLARE_RELAY_ENABLED=false` disables automatic
updates; it does not stop the service. This deployment targets personal, light use within
Cloudflare's service quotas.
