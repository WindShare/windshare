package platformsetup

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestWindowsInspectionSeparatesAbsentRulesFromQueryFailure(t *testing.T) {
	const query = `
function Get-NetFirewallApplicationFilter {
    [CmdletBinding()] param($PolicyStore, $Program)
    Write-Error -Message 'injected application query failure' -ErrorId $QueryFailure
}
$Executable = 'C:\wind.exe'
$results = @(foreach ($QueryFailure in @('CmdletizationQuery_NotFound_Program', 'AccessDenied')) {
    try {
        $snapshot = & {
`
	const result = `
        } | ConvertFrom-Json
        @{ Failed = $false; Rules = $snapshot.Rules.Count }
    } catch {
        @{ Failed = $true; Error = $_.Exception.Message }
    }
})
$results | ConvertTo-Json -Compress
`
	output, err := runPowerShell(t.Context(), fakeInspectCommands+query+inspectScript+result)
	if err != nil {
		t.Fatal(err, string(output))
	}
	var results []struct {
		Failed bool
		Rules  int
		Error  string
	}
	if err := json.Unmarshal(output, &results); err != nil {
		t.Fatal(err, string(output))
	}
	if len(results) != 2 || results[0].Failed || results[0].Rules != 0 ||
		!results[1].Failed || !strings.Contains(results[1].Error, "injected application query failure") {
		t.Fatal(string(output))
	}
}
