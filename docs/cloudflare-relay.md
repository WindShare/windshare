# Private Cloudflare relay

Fork this repository and use **Deploy private Cloudflare relay** in GitHub Actions.
No source edits or local Wrangler installation are needed. This optional relay is for
personal, light use; Cloudflare's [Free-plan quotas](https://developers.cloudflare.com/durable-objects/platform/pricing/)
still apply.

## First deployment

1. In Cloudflare, create your account's **workers.dev** subdomain under Workers & Pages,
   or choose an unused hostname in an active Cloudflare zone.
   Create a [Workers API token](https://developers.cloudflare.com/workers/authorization/workers/)
   with **Workers / Admin at the Workers product scope**, restricted to this account.
   Creating the first Worker requires this role; Editor only updates existing Workers.
   For a custom domain, also grant **Zone / Workers Routes / Edit** and
   **Zone / Zone / Read**, restricted to its zone. No GitHub write token is needed.
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
   | Variable | `CLOUDFLARE_ACCOUNT_ID` | Your Cloudflare account ID |
   | Variable | `CLOUDFLARE_WORKER_NAME` | A new, stable lowercase name, e.g. `windshare-private` |
   | Variable | `WINDSHARE_CONFIG_RECIPIENT` | Your public `age1...` key |
   | Variable | `CLOUDFLARE_RELAY_ENABLED` | `true` |
   | Variable (optional pair) | `CLOUDFLARE_RELAY_DOMAIN` | Custom hostname, e.g. `relay.example.com` |
   | Variable (optional pair) | `CLOUDFLARE_ZONE_ID` | That hostname's Cloudflare zone ID |

4. Enable Actions in the fork, then run **Deploy private Cloudflare relay** on `main`.
   The first run generates a random registration credential and publishes the Worker.
   Its summary shows your custom domain or `https://<name>.<subdomain>.workers.dev`.
   Custom domains get managed DNS/TLS; workers.dev and preview URLs are disabled in that mode.
5. Download and extract the **windshare-relay-config** artifact. In its directory, with
   [WindShare installed](installation.md), decrypt and import:

   ```sh
   age --decrypt -i /path/to/windshare.key relay.json.age | wind relay import -
   wind share <path>
   ```

   This works in PowerShell and POSIX shells. Import replaces the entire allowed relay
   set, so this configuration uses **only your private relay**. To allow additional
   relays, decrypt to a private JSON file and edit its `relays` array before importing;
   see [saved relays](cli.md#saved-relays). Receivers still open ordinary sharing links.

The registration credential is separate from the Cloudflare API token. It lives in the
Worker's `REGISTRATION_SECRET`; only its age-encrypted import copy is published as an
artifact and retained in the Worker's `WINDSHARE_DEPLOYMENT` variable. Your private age
key never goes to GitHub or Cloudflare. Keep it and the imported local configuration.

## Updates and recovery

Use **Sync fork → Update branch** on `main`. Changes to the relay, deployment scripts
or workflow trigger an update automatically. Documentation-only syncs need no deployment;
the workflow can always be run manually. Settings remain in repository Variables/Secrets,
outside Git, so syncing upstream does not overwrite them.

Updates preserve the Worker name/address, registration secret, encrypted import copy,
`SHARES` binding and its SQLite namespace. They apply code without deleting storage;
permanent STOP and recovery state remain. Active transfers may briefly reconnect while
Cloudflare propagates the deployment; a successful workflow is not proof that every
active object has already switched versions.

- A failed or interrupted run is safe to rerun. If the first upload already committed,
  the next run recovers its encrypted configuration without generating another credential.
- Artifacts expire after 30 days. Rerun the workflow to download the same encrypted copy.
- To renew deployment access, replace only `CLOUDFLARE_API_TOKEN`; imported clients stay valid.
- Keep the account, Worker name, route Variables (or workers.dev subdomain) and recipient key unchanged.
  Changing the account/name selects a different deployment. Address or recipient drift,
  missing secret/storage, or an unmanaged Worker with the same name fails explicitly.
  Restore the original settings instead of deleting the Worker or editing its state bindings.
- Set `CLOUDFLARE_RELAY_ENABLED` to `false` to stop automatic updates; it does not stop the relay.

An existing manually deployed Worker is not adopted automatically. Choose a new name for
this workflow. Back up the age key: losing it prevents decrypting exports, although an
already imported local configuration and ordinary code updates continue to work.
