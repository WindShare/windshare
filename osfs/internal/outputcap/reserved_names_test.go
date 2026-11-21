package outputcap

import (
	"testing"

	"github.com/windshare/windshare/core/transfer/receivecontract"
)

func TestReservedRootNamespacesMatchScope(t *testing.T) {
	if !ReservedNamespaceDestinationContainer.Valid() || !ReservedNamespaceResultRoot.Valid() || ReservedNamespaceScope(0).Valid() {
		t.Fatal("reserved namespace scope validity mismatch")
	}
	if ReservedNamespacePrefix(ReservedNamespaceDestinationContainer) != ControlDirectoryName {
		t.Fatalf("container prefix = %q", ReservedNamespacePrefix(ReservedNamespaceDestinationContainer))
	}
	if ReservedNamespacePrefix(ReservedNamespaceResultRoot) != RecoveryIdentityPrefix {
		t.Fatalf("result-root prefix = %q", ReservedNamespacePrefix(ReservedNamespaceResultRoot))
	}
	if ReservedNamespacePrefix(0) != "" {
		t.Fatalf("unknown scope prefix = %q", ReservedNamespacePrefix(0))
	}

	for _, name := range []string{
		".windshare-output",
		".WINDSHARE-OUTPUT",
		".windshare-output.probe-1234",
		".windſhare-output",
	} {
		if !ReservedRootName(ReservedNamespaceDestinationContainer, name) {
			t.Errorf("container should reserve %q", name)
		}
		if ReservedRootName(ReservedNamespaceResultRoot, name) {
			t.Errorf("result root should not reserve %q", name)
		}
	}

	for _, name := range []string{
		".wsresume",
		".wsresume-notes.txt",
		".WSRESUME-state",
		".wſresume-part",
	} {
		if !ReservedRootName(ReservedNamespaceResultRoot, name) {
			t.Errorf("result root should reserve %q", name)
		}
		if ReservedRootName(ReservedNamespaceDestinationContainer, name) {
			t.Errorf("container should not reserve %q", name)
		}
	}

	for _, name := range []string{"readme.txt", "WSRESUME", "文档.txt", ".gitignore", ".wsresum"} {
		if ReservedRootName(ReservedNamespaceDestinationContainer, name) || ReservedRootName(ReservedNamespaceResultRoot, name) {
			t.Errorf("ordinary name %q was treated as reserved", name)
		}
	}

	if got := ReservedNamespaceScopeForEntry(receivecontract.ContainerEntryResultRoot); got != ReservedNamespaceResultRoot {
		t.Fatalf("result-root entry scope = %v", got)
	}
	if got := ReservedNamespaceScopeForEntry(receivecontract.ContainerEntrySingleFile); got != ReservedNamespaceDestinationContainer {
		t.Fatalf("single-file entry scope = %v", got)
	}
	if got := ReservedNamespaceScopeForEntry(0); got != 0 {
		t.Fatalf("unknown entry scope = %v", got)
	}
}

func TestCompatibleNameReportValidityContract(t *testing.T) {
	var zero CompatibleNameReport
	if !zero.Empty() || !zero.Valid() {
		t.Fatalf("zero report = %+v, want empty and valid", zero)
	}
	valid := CompatibleNameReport{RepairedCount: 3, ReportName: "windshare-names-abc-1-0.json", PageCount: 2}
	if valid.Empty() || !valid.Valid() {
		t.Fatalf("valid report = %+v, want non-empty and valid", valid)
	}
	for _, invalid := range []CompatibleNameReport{
		{RepairedCount: 1},
		{ReportName: "windshare-names-abc-1-0.json"},
		{PageCount: 1},
		{RepairedCount: 1, ReportName: "windshare-names-abc-1-0.json"},
		{RepairedCount: 1, PageCount: 1},
		{ReportName: "windshare-names-abc-1-0.json", PageCount: 1},
	} {
		if invalid.Empty() || invalid.Valid() {
			t.Fatalf("invalid report %+v unexpectedly empty=%t valid=%t", invalid, invalid.Empty(), invalid.Valid())
		}
	}
}
