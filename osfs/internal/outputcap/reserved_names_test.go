package outputcap

import "testing"

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
}
