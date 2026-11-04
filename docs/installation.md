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

```powershell
wind doctor network            # Inspect this executable's inbound rules; never elevates
wind setup network             # Configure this executable's UDP/TCP rules
wind setup network --skip      # Stop reminders; leave firewall rules unchanged
```

Rules cover domain, private, and public networks, all ports and remote addresses.
Only the short-lived rule writer requests administrator permission; transfers stay
at their original privilege level. Explicit diagnostics inspect executable-specific
rules and active profile settings. Global rules are excluded; a present rule does
not prove effective permission or a direct connection.

The installer uses these same commands. Choices live in
`%LOCALAPPDATA%/WindShare/network-preference.json`. Successful setup records the
canonical executable path; moving the executable offers setup again. Skip persists
across paths, and cancelling UAC preserves the previous choice.

Daily P2P-capable `share/get` tasks read only this local record, without PowerShell
or blocking prompts; `relay-only` skips the read. Rule or organization-policy changes
require explicit diagnosis. Neither setup nor skipping changes connectivity mode.

Unattended installation skips optional setup without saving a choice. Scripts never
prompt or open UAC: explicitly run `wind setup network --non-interactive` from an
administrator terminal when needed. `--json` writes diagnostic results to stdout.
Installer `-Firewall Configure` and `-Firewall Skip` explicitly select an action;
`-Uninstall` with the same `-Destination` removes only its owned rules and executable.
Automatic firewall configuration is Windows-only.

## Build from source

Clone the intended commit or release tag, or extract the release's `windshare-vX.Y.Z-source.zip`.
Both contain the pinned Pion sources, patches, licenses and provenance required
by the root module's local replacements. With Go installed, use the same installer
above; the root `go.mod` selects source installation, which verifies the pinned source
and builds `wind` even if a previous build left an executable beside it. Verification
or build failure stops installation. The server uses Rust stable and a C compiler
(for bundled SQLite). Build it separately:

```sh
cd relay-rust
rustup run stable cargo build --locked --release -p windshare-relay-native --bin relay-native
```

The executable is `.target/release/relay-native` (`.exe` on Windows). Release ZIPs
ship this Rust executable as `wsrelay`; see its `--help` and the deployment guide.

Run source commands with `GOWORK=off`. Other Go dependencies are downloaded through
the usual Go module mechanism. `go install github.com/windshare/windshare/cmd/wind@version`
and canonical Go proxy source ZIPs are unsupported because they cannot carry the
nested local replacements. Use the complete clone/source bundle or binary release.
