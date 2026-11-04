package platformsetup

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// These subprocess tests shadow every firewall command. They exercise the shipped
// scripts and PowerShell's actual quoting/JSON behavior without changing host policy.
func TestWindowsOwnedRuleWriter(t *testing.T) {
	for _, scenario := range []string{"new", "repair", "remove", "conflict", "failure"} {
		t.Run(scenario, func(t *testing.T) {
			log := filepath.Join(t.TempDir(), "calls.jsonl")
			prefix := "$CallLog = " + quotePowerShell(log) + "\n$Scenario = " + quotePowerShell(scenario) + "\n" + fakeRuleCommands
			action := EnsureRules
			if scenario == "remove" {
				action = RemoveRules
			}
			output, runErr := runPowerShell(t.Context(), prefix+ruleScript(`C:\Wind's $app\wind.exe`, action))
			var result struct {
				Code   int
				Detail string
			}
			if err := json.Unmarshal(output, &result); err != nil {
				t.Fatal(string(output), err, runErr)
			}
			calls, err := os.ReadFile(log)
			if err != nil && !os.IsNotExist(err) {
				t.Fatal(err)
			}
			if scenario == "conflict" {
				if result.Code != 10 || len(calls) != 0 {
					t.Fatal(result, string(calls))
				}
				return
			}
			if scenario == "failure" {
				if result.Code == 0 || result.Detail == "" {
					t.Fatal(result)
				}
				return
			}
			if runErr != nil || result.Code != 0 {
				t.Fatal(result, runErr)
			}
			created, removed := 0, 0
			for line := range strings.SplitSeq(strings.TrimSpace(string(calls)), "\n") {
				var call struct{ Action, Name, Program, Protocol, Profile, Group, Direction, Enabled, Operation string }
				if err := json.Unmarshal([]byte(line), &call); err != nil {
					t.Fatal(err)
				}
				if call.Operation == "remove" {
					removed++
					continue
				}
				created++
				if call.Program != `C:\Wind's $app\wind.exe` || call.Action != "Allow" || call.Profile != "Any" || call.Group != "WindShare installation" || call.Direction != "Inbound" || call.Enabled != "True" ||
					(call.Protocol != "UDP" && call.Protocol != "TCP") {
					t.Fatal(call)
				}
			}
			wantCreated, wantRemoved := 2, 0
			if scenario == "repair" || scenario == "remove" {
				wantRemoved = 2
			}
			if scenario == "remove" {
				wantCreated = 0
			}
			if created != wantCreated || removed != wantRemoved {
				t.Fatal(created, removed, string(calls))
			}
		})
	}
}

const fakeRuleCommands = `
function Get-NetFirewallRule {
    [CmdletBinding()] param($PolicyStore)
    if ($PolicyStore -ne 'PersistentStore') { throw 'Unexpected rule store.' }
    if ($Scenario -eq 'new') { return }
    foreach ($plan in $plans) {
        [pscustomobject]@{ Name = $plan.Name; Group = $(if ($Scenario -eq 'conflict') { 'other owner' } else { $ownedGroup }) }
    }
}
function Get-NetFirewallApplicationFilter {
    [CmdletBinding()] param([Parameter(ValueFromPipeline)]$Rule)
    process { [pscustomobject]@{ Program = $Executable } }
}
function Remove-NetFirewallRule {
    [CmdletBinding()] param([Parameter(ValueFromPipeline)]$Rule)
    process { @{ Operation = 'remove'; Name = $Rule.Name } | ConvertTo-Json -Compress | Add-Content -LiteralPath $CallLog }
}
function New-NetFirewallRule {
    [CmdletBinding()] param($Name, $DisplayName, $Group, $Program, $Direction, $Action, $Protocol, $Profile, $Enabled, $PolicyStore)
    if ($Scenario -eq 'failure') { throw 'Injected rule write failure.' }
    @{ Operation = 'create'; Name = $Name; Group = $Group; Program = $Program; Direction = $Direction;
        Action = $Action; Protocol = $Protocol; Profile = $Profile; Enabled = $Enabled } |
        ConvertTo-Json -Compress | Add-Content -LiteralPath $CallLog
}
`

func TestWindowsPolicyReaderScopesExecutableRules(t *testing.T) {
	output, err := runPowerShell(t.Context(), fakeInspectCommands+"\n$Executable = 'C:\\wind[preview].exe'\n"+inspectScript)
	if err != nil {
		t.Fatal(err, string(output))
	}
	var snapshot Snapshot
	if err := json.Unmarshal(output, &snapshot); err != nil {
		t.Fatal(err, string(output))
	}
	if len(snapshot.Rules) != 2 || len(snapshot.Networks) != 1 {
		t.Fatalf("unexpected scoped snapshot: %+v", snapshot)
	}
	if snapshot.Rules[0].Name != "allow" || snapshot.Rules[0].Conditional || snapshot.Rules[1].Name != "block" || !snapshot.Rules[1].Conditional || snapshot.Rules[1].Restrictions == "" {
		t.Fatal(snapshot.Rules)
	}
	findings := Evaluate(snapshot)
	if findings[0].Status != RuleRestricted || findings[1].Status != RuleMissing || !findings[1].SuggestSetup() {
		t.Fatal(findings)
	}
}

const fakeInspectCommands = `
$FixtureRules = @(
    foreach ($name in @('allow', 'block', 'package', 'service', 'other-program', 'sshd-52222', 'tailscale', 'global-block')) {
        [pscustomobject]@{ Name = $name; InstanceID = $name; Profile = 2; PrimaryStatus = 'OK'; Enabled = 'True'; Direction = 'Inbound';
            Action = $(if ($name -eq 'block') { 'Block' } else { 'Allow' }); PolicyStoreSourceType = 'Local';
            Owner = ''; PackageFamilyName = $(if ($name -eq 'package') { 'Store.App' } else { '' }) }
    }
)
function Get-NetConnectionProfile {
    [CmdletBinding()] param()
    [pscustomobject]@{ InterfaceAlias = 'Wi-Fi'; InterfaceIndex = 1; NetworkCategory = 'Private' }
}
function Get-NetFirewallProfile {
    [CmdletBinding()] param($PolicyStore)
    if ($PolicyStore -ne 'ActiveStore') { throw 'Not resultant policy.' }
    [pscustomobject]@{ Name = 'Private'; Enabled = 'True'; AllowInboundRules = 'True'; AllowLocalFirewallRules = 'True';
        DefaultInboundAction = 'Block'; DisabledInterfaceAliases = @() }
}
function Get-NetFirewallRule {
    [CmdletBinding()] param($PolicyStore, [Parameter(ValueFromPipeline)]$Application)
    process {
        if ($PolicyStore -ne 'ActiveStore' -or $null -eq $Application) { throw 'Unscoped rule query.' }
        if ($Application.Program -ne $Executable) { throw 'Queried another program or global rule.' }
        $FixtureRules | Where-Object { $_.InstanceID -eq $Application.InstanceID }
    }
}
function Get-NetFirewallApplicationFilter {
    [CmdletBinding()] param($PolicyStore, $Program)
    if ($PolicyStore -ne 'ActiveStore' -or $Program -cne [WildcardPattern]::Escape($Executable)) { throw 'Unscoped application query.' }
    foreach ($rule in $FixtureRules) {
        # Deliberately return irrelevant candidates too: the adapter must reject
        # Any even if a provider expands or mishandles the program query.
        $path = if ($rule.Name -eq 'other-program') { 'C:\other.exe' }
            elseif ($rule.Name -in @('sshd-52222', 'tailscale', 'global-block')) { 'Any' } else { $Executable }
        [pscustomobject]@{ InstanceID = $rule.Name; Package = $null; Program = $path }
    }
}
function Get-NetFirewallServiceFilter {
    [CmdletBinding()] param([Parameter(ValueFromPipeline)]$Rule)
    process { [pscustomobject]@{ InstanceID = $Rule.Name; Service = $(if ($Rule.Name -eq 'service') { 'sshd' } else { 'Any' }) } }
}
function Get-NetFirewallPortFilter {
    [CmdletBinding()] param([Parameter(ValueFromPipeline)]$Rule)
    process { [pscustomobject]@{ InstanceID = $Rule.Name; Protocol = 'UDP'; DynamicTarget = 'Any';
        LocalPort = $(if ($Rule.Name -eq 'block') { '53' } else { 'Any' }); RemotePort = 'Any' } }
}
function Get-NetFirewallAddressFilter {
    [CmdletBinding()] param([Parameter(ValueFromPipeline)]$Rule)
    process { [pscustomobject]@{ InstanceID = $Rule.Name; LocalAddress = 'Any'; RemoteAddress = 'Any' } }
}
function Get-NetFirewallInterfaceFilter {
    [CmdletBinding()] param([Parameter(ValueFromPipeline)]$Rule)
    process { [pscustomobject]@{ InstanceID = $Rule.Name; InterfaceAlias = 'Any' } }
}
function Get-NetFirewallInterfaceTypeFilter {
    [CmdletBinding()] param([Parameter(ValueFromPipeline)]$Rule)
    process { [pscustomobject]@{ InstanceID = $Rule.Name; InterfaceType = 'Any' } }
}
function Get-NetFirewallSecurityFilter {
    [CmdletBinding()] param([Parameter(ValueFromPipeline)]$Rule)
    process { [pscustomobject]@{ InstanceID = $Rule.Name; LocalUser = 'Any'; RemoteUser = 'Any'; RemoteMachine = 'Any';
        Authentication = 'NotRequired'; Encryption = 'NotRequired'; OverrideBlockRules = $false } }
}
`
