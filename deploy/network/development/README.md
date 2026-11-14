# Development network

This public profile reuses the existing `windshare-development` identity:
two roots, generation 1 / serial 2, cn1 for STUN and us1 for relay + STUN.
The directory expires on **2027-01-13T08:00:58Z**. Its frontend is
`http://localhost:4173`, so generated links are for receiving on this computer.

From the repository root, build the CLI, desktop and Web together:

```text
make development-build
```

This verifies the signed history, selects the Go `windshare_development` tag
and Vite `windshare-development` mode, and stages all outputs under
`.windshare-dev/zero-config/`. It uses installed toolchains and Web dependencies,
does not need private keys, and preserves existing settings and trust state.
Close the development desktop before rebuilding on Windows.

Start the matching frontend in one terminal:

```powershell
pnpm --dir web preview --outDir ../.windshare-dev/zero-config/web --host localhost --port 4173 --strictPort
```

Start the desktop or CLI in another terminal:

```powershell
$env:WINDSHARE_CONFIG = Join-Path $PWD '.windshare-dev/zero-config/config.json'
& ./.windshare-dev/zero-config/desktop/windshare.exe
# Or:
& ./.windshare-dev/zero-config/wind.exe share 'C:/path/to/file'
```

The initial settings contain `{"profiles":[]}`. Keep the adjacent
`config.json.state/network/` and an isolated development browser profile;
do not reset trust by clearing only the settings file. Default builds still use
`deploy/network/release.json`; this profile never replaces production roots.

Public keys, the signing certificate, directory source, snapshot and history
are committed here. Development private keys may stay in the gitignored
`.secret/zero-config/keys/` with encrypted storage and a separate backup.
Existing local keys use Windows EFS; Git does not back them up. Keep production
root keys offline outside the checkout.

For directory updates, retain the identity, update validity dates and increase
the serial in `directory.json`, then run:

```powershell
go run ./cmd/network-sign sign --source deploy/network/development/directory.json --release deploy/network/development/release.json --history deploy/network/development/history --online-private .secret/zero-config/keys/development-directory.pem --certificate deploy/network/development/certificate.json --out deploy/network/development/windshare-network.json
```

Renew the certificate with the existing root when needed; see the
[signing workflow](../README.md). Previous CLI and desktop browser-download
evidence, profiles and native trust state remain under
`.windshare-dev/zero-config/`.
