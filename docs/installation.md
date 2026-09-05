# Install WindShare

Use the `windshare-vX.Y.Z-windows-amd64.zip` or `windshare-vX.Y.Z-linux-amd64.zip`
asset from a GitHub release, and verify its SHA-256 against the release checksums.
Extract it, then run the installer inside the extracted directory:

```powershell
# Windows, PowerShell 7
./scripts/install/windows/install.ps1
```

```sh
# Linux (optional argument: destination directory)
bash scripts/install/install.sh ~/.local/bin
```

The packages contain `wind` and `wsrelay`; the installer installs the user CLI.
For server operation see [wsrelay deployment](../scripts/deploy/wsrelay/README.md).
The source bundle contains its deployment scripts and templates.

## Windows first setup

The installer asks once whether to create application-scoped inbound UDP and TCP rules.
Skipping is supported; sharing continues using available direct paths and relay.
It never elevates itself or changes global firewall policy. Run from a shell that
already has permission if you choose to configure the rule. Managed policy and
explicit block rules retain precedence. A created rule does not prove reachability.

The decision (`configured`, `declined`, or `unavailable`) is saved under
`%LOCALAPPDATA%/WindShare/connectivity-setup.json` for diagnostics. Reinstalling at
the same path keeps that decision. Retry explicitly with `-Firewall Configure`,
or skip first setup with `-Firewall Skip`. Daily `share` and `get` only read status.
Use `-Uninstall` with the same `-Destination` to remove that installation's owned
rule and executable. Other firewall rules are never removed. Automatic firewall
setup is currently supported only on Windows. TCP follows the pinned provider's
verified platform capabilities; a firewall rule does not enable an unsupported transport.

## Build from source

Clone the release tag or extract the release's `windshare-vX.Y.Z-source.zip`.
Both contain the pinned Pion sources, patches, licenses and provenance required
by the root module's local replacements. With Go installed, use the same installer
above; the root `go.mod` selects source installation, which verifies the pinned source
and builds `wind` even if a previous build left an executable beside it. Verification
or build failure stops installation. To build the server:

```sh
go run ./scripts/ci/_piondeps
go build -trimpath -buildvcs=false -o wsrelay ./relay/cmd/wsrelay
```

Run source commands with `GOWORK=off`. Other Go dependencies are downloaded through
the usual Go module mechanism. `go install github.com/windshare/windshare/cmd/wind@version`
and canonical Go proxy source ZIPs are unsupported because they cannot carry the
nested local replacements. Use the complete clone/source bundle or binary release.
