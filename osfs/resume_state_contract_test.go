package osfs

import (
	"reflect"
	"testing"

	"github.com/windshare/windshare/core/transfer"
	"github.com/windshare/windshare/osfs/internal/resumeauthority"
)

func TestRecoveryAuthorityIsSeparateFromOutputSessionLifecycle(t *testing.T) {
	outputSession := reflect.TypeFor[transfer.DirectTreeSession]()
	if _, exists := outputSession.MethodByName("Discard"); exists {
		t.Fatal("output session gained resume-state deletion authority")
	}
	recoveryAuthority := reflect.TypeFor[*RecoveryAuthority]()
	for _, method := range []string{"List", "Discard"} {
		if _, exists := recoveryAuthority.MethodByName(method); !exists {
			t.Fatalf("explicit recovery authority lost %s", method)
		}
	}
	for _, retired := range []string{"Recover", "InstallReceipt", "ReplaceLifecycle"} {
		if _, exists := recoveryAuthority.MethodByName(retired); exists {
			t.Fatalf("public recovery authority retained terminal/history method %s", retired)
		}
	}
}

func TestResumeStateAuthorityRequiresExactOperationLeaseForMutation(t *testing.T) {
	store := reflect.TypeFor[resumeauthority.Store]()
	for _, method := range []string{"Page", "Acquire"} {
		if _, exists := store.MethodByName(method); !exists {
			t.Fatalf("resume store lost %s", method)
		}
	}
	lease := reflect.TypeFor[resumeauthority.OperationLease]()
	for _, method := range []string{"Snapshot", "Transition", "Cleanup", "Close"} {
		if _, exists := lease.MethodByName(method); !exists {
			t.Fatalf("operation lease lost %s", method)
		}
	}
	for _, retired := range []string{"ObserveRecovery", "InstallReceipt", "ReplaceLifecycle"} {
		if _, exists := lease.MethodByName(retired); exists {
			t.Fatalf("operation lease retained retired method %s", retired)
		}
	}
}
