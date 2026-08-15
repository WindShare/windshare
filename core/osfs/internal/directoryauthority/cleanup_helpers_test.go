package directoryauthority

import "testing"

func TestCleanupHelpersCloseOnlyPresentNativeAuthority(t *testing.T) {
	if err := closeEntry(nil); err != nil {
		t.Fatalf("nil entry close = %v", err)
	}
	if err := closeEntry(&fakeEntryReference{}); err != nil {
		t.Fatalf("entry close = %v", err)
	}
	if err := closeDirectory(nil); err != nil {
		t.Fatalf("nil directory close = %v", err)
	}
}
