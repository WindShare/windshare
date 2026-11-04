# CLI network setup

Keep everyday sharing independent of Windows firewall inspection. Configuration
history, observed executable rules, and actual connection outcomes are separate.

## Daily transfers

P2P-capable `share/get` commands read only the local installation preference.
They never launch PowerShell or wait for setup input. `relay-only` skips this read.

- `skip`: suppress reminders across executable and network changes.
- `configure` plus the current canonical executable path: suppress reminders.
- No choice or a different configured path: offer the explicit setup command.
- An unreadable preference produces a trace diagnostic, not a blocking prompt.

A successful rule write records its executable path; cancellation and failure
preserve the previous choice. Removal clears only the matching installation
record. Replacing the binary at the same path does not require setup again.

## Explicit commands

`wind doctor network` reads enabled inbound rules explicitly targeting the
current executable and the active networks' firewall profiles. It never elevates.
Reports identify this scope and describe rules as present, missing, restricted,
or unknown. Global rules are excluded; these findings cannot prove effective
host-wide permission or P2P reachability. Arbitrary traffic restrictions remain
diagnostic evidence instead of being evaluated by a second firewall engine.

`wind setup network` writes only owned UDP/TCP rules for the current executable,
covering all profiles, ports, and remote addresses. It preserves ownership checks,
limits elevation to the rule writer, and inspects current rules after the operation.
Organization policy can still ignore local rules or disallow inbound exceptions.

The installer uses the same setup commands and path-bound preference. Unattended
installation never prompts or invents a user choice. Skipping setup does not
change connectivity mode. Rule drift is diagnosed explicitly; connection traces
remain the authority for actual direct and relay paths.
