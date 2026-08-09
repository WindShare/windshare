package osfs

import (
	"reflect"
	"testing"

	"github.com/windshare/windshare/core/transfer"
)

func TestResumeStateAuthorityIsSeparateFromOutputSessionLifecycle(t *testing.T) {
	outputSession := reflect.TypeFor[transfer.DirectTreeSession]()
	if _, exists := outputSession.MethodByName("Discard"); exists {
		t.Fatal("PauseJob or CompleteJob gained resume-state deletion authority")
	}
	resumeAuthority := reflect.TypeFor[ResumeStateAuthority]()
	if _, exists := resumeAuthority.MethodByName("Discard"); !exists {
		t.Fatal("explicit resume authority lost its discard capability")
	}
}

func TestResumeStateAuthorityRequiresRepositoryLeaseForMutation(t *testing.T) {
	repository := reflect.TypeFor[ResumeStateRepository]()
	if _, exists := repository.MethodByName("Acquire"); !exists {
		t.Fatal("resume repository lost its fresh operation lease boundary")
	}
	lease := reflect.TypeFor[ResumeStateRepositoryLease]()
	for _, method := range []string{"Snapshot", "ObserveRecovery", "CleanupOwned", "InstallReceipt", "ReplaceLifecycle"} {
		if _, exists := lease.MethodByName(method); !exists {
			t.Fatalf("resume repository lease lost %s", method)
		}
	}
}
