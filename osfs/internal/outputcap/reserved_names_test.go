package outputcap

import "testing"

func TestReservedRootNamespacesMatchDisposition(t *testing.T) {
	if ReservedRootPrefix(CallerProvidedContainer) != ControlDirectoryName {
		t.Fatalf("container prefix = %q", ReservedRootPrefix(CallerProvidedContainer))
	}
	if ReservedRootPrefix(AuthorityCreatedRoot) != RecoveryIdentityPrefix {
		t.Fatalf("result-root prefix = %q", ReservedRootPrefix(AuthorityCreatedRoot))
	}
	if ReservedRootPrefix("") != "" {
		t.Fatalf("unknown disposition prefix = %q", ReservedRootPrefix(""))
	}

	for _, name := range []string{
		".windshare-output",
		".WINDSHARE-OUTPUT",
		".windshare-output.probe-1234",
		".windſhare-output",
	} {
		if !ReservedRootName(CallerProvidedContainer, name) {
			t.Errorf("container should reserve %q", name)
		}
		if ReservedRootName(AuthorityCreatedRoot, name) {
			t.Errorf("result root should not reserve %q", name)
		}
	}

	for _, name := range []string{
		".wsresume",
		".wsresume-notes.txt",
		".WSRESUME-state",
		".wſresume-part",
	} {
		if !ReservedRootName(AuthorityCreatedRoot, name) {
			t.Errorf("result root should reserve %q", name)
		}
		if ReservedRootName(CallerProvidedContainer, name) {
			t.Errorf("container should not reserve %q", name)
		}
	}

	for _, name := range []string{"readme.txt", "WSRESUME", "文档.txt", ".gitignore", ".wsresum"} {
		if ReservedRootName(CallerProvidedContainer, name) || ReservedRootName(AuthorityCreatedRoot, name) {
			t.Errorf("ordinary name %q was treated as reserved", name)
		}
	}
}
