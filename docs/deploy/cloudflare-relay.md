# Cloudflare deployment

Fork this repository and use **Deploy WindShare to Cloudflare** in GitHub Actions.
By default, one Worker hosts the frontend and private relay at the same address.
The frontend uses [Workers Static Assets](https://developers.cloudflare.com/workers/static-assets/);
`/v2/*` requests reach the relay. Set `CLOUDFLARE_DEPLOYMENT_MODE=relay-only` to omit the frontend.
No source edits or local Wrangler installation are needed. This optional deployment is for
personal, light use; Cloudflare's [Free-plan quotas](https://developers.cloudflare.com/durable-objects/platform/pricing/)
still apply.

## First deployment

1. In Cloudflare, create your account's **workers.dev** subdomain under Workers & Pages,
   or choose an unused hostname in an active Cloudflare zone.
   Create a token with **Account / Workers / Admin** for your account. For custom domains,
   also grant **Zone / Workers Routes / Edit** and **Zone / Zone / Read** for that zone.
   The [step-by-step token setup guide (中文)](cloudflare-api-token.zh-CN.md) shows every
   browser step, including saving `CLOUDFLARE_API_TOKEN` in GitHub Secrets.
2. Install [age](https://github.com/FiloSottile/age#installation) locally and run outside
   the repository:

   ```sh
   age-keygen -o windshare.key
   ```

   Keep this private file. Copy the printed **public** `age1...` key for the next step.
3. Fork the repository. Under **Settings → Secrets and variables → Actions**, set:

   | Kind | Name | Value |
   |---|---|---|
   | Secret | `CLOUDFLARE_API_TOKEN` | The Cloudflare deployment token |
   | Secret | `CLOUDFLARE_ACCOUNT_ID` | Your Cloudflare account ID |
   | Variable | `CLOUDFLARE_WORKER_NAME` | A new, stable lowercase name, e.g. `windshare-private` |
   | Variable | `WINDSHARE_CONFIG_RECIPIENT` | Your public `age1...` key |
   | Variable | `CLOUDFLARE_RELAY_ENABLED` | `true` |
   | Variable (optional) | `CLOUDFLARE_DEPLOYMENT_MODE` | `full` (default) or `relay-only` |
   | Variable (optional pair) | `CLOUDFLARE_RELAY_DOMAIN` | Custom hostname, e.g. `relay.example.com` |
   | Secret (optional pair) | `CLOUDFLARE_ZONE_ID` | That hostname's Cloudflare zone ID |

   IDs use Secrets to reduce CI-log exposure; they do not grant access without a token.
   Hostnames remain Variables because sharing links and deployment artifacts contain them.

4. Enable Actions in the fork, then run **Deploy WindShare to Cloudflare** on `main`.
   The first run generates a random registration credential and publishes the Worker.
   Its summary shows your custom domain or `https://<name>.<subdomain>.workers.dev`.
   Custom domains get managed DNS/TLS; workers.dev and preview URLs are disabled in that mode.
5. Download and extract the **windshare-relay-config** artifact. In its directory, with
   [WindShare installed](../installation.md), decrypt and import:

   ```sh
   age --decrypt -i /path/to/windshare.key relay.json.age | wind config import - --front-url https://<your-worker-address>
   wind share <path>
   ```

   This works in PowerShell and POSIX shells. Import saves your frontend and replaces
   the entire relay set, so future shares use **only your private relay**. To allow additional
   relays, decrypt to a private JSON file and edit its `relays` array before importing;
   see [sharing defaults](../cli.md#sharing-defaults). In `relay-only` mode, pass your
   separately hosted frontend's URL during import. Full same-origin deployments generate
   links without `r=`. The workflow summary includes the commands.

The registration credential is separate from the Cloudflare API token. It lives in the
Worker's `REGISTRATION_SECRET`; only its age-encrypted import copy is published as an
artifact and retained in the Worker's `WINDSHARE_DEPLOYMENT` variable. Your private age
key never goes to GitHub or Cloudflare. Keep it and the imported local configuration.

## Updates and recovery

Use **Sync fork → Update branch** on `main`. Changes to the relay, frontend, deployment scripts
or workflow trigger an update automatically. Documentation-only syncs need no deployment;
the workflow can always be run manually. Settings remain in repository Variables/Secrets,
outside Git, so syncing upstream does not overwrite them.

To switch modes, change `CLOUDFLARE_DEPLOYMENT_MODE` and rerun the workflow. `relay-only`
skips frontend dependencies/build/upload and removes the hosted frontend. Returning to
`full` restores it. Both modes preserve the relay identity, credentials and share storage.

Updates preserve the Worker name/address, registration secret, encrypted import copy,
`SHARES` binding and its SQLite namespace. They apply code without deleting storage;
permanent STOP and recovery state remain. Active transfers may briefly reconnect while
Cloudflare propagates the deployment; a successful workflow is not proof that every
active object has already switched versions.

- A failed or interrupted run is safe to rerun. If the first upload already committed,
  the next run recovers its encrypted configuration without generating another credential.
- Artifacts expire after 30 days. Rerun the workflow to download the same encrypted copy.
- To renew deployment access, replace only `CLOUDFLARE_API_TOKEN`; imported clients stay valid.
- Keep the account, Worker name, route settings (or workers.dev subdomain) and recipient key unchanged.
  Changing the account/name selects a different deployment. Address or recipient drift,
  missing secret/storage, or an unmanaged Worker with the same name fails explicitly.
  Restore the original settings instead of deleting the Worker or editing its state bindings.
- Set `CLOUDFLARE_RELAY_ENABLED` to `false` to stop automatic updates; it does not stop the relay.

An existing manually deployed Worker is not adopted automatically. Choose a new name for
this workflow. Back up the age key: losing it prevents decrypting exports, although an
already imported local configuration and ordinary code updates continue to work.
