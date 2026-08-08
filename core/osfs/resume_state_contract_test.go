package osfs

import (
	"encoding/json"
	"errors"
	"reflect"
	"testing"

	"github.com/windshare/windshare/core/transfer"
)

func TestResumeStateAuthorityIsSeparateFromOutputSessionLifecycle(t *testing.T) {
	outputSession := reflect.TypeFor[transfer.OutputSession]()
	if _, exists := outputSession.MethodByName("Discard"); exists {
		t.Fatal("PauseJob or CompleteJob gained resume-state deletion authority")
	}
	resumeAuthority := reflect.TypeFor[ResumeStateAuthority]()
	if _, exists := resumeAuthority.MethodByName("Discard"); !exists {
		t.Fatal("explicit resume authority lost its discard capability")
	}
}

func TestResumeStateReferenceCannotBecomeSerializedDeletionAuthority(t *testing.T) {
	if _, err := json.Marshal(ResumeStateRef{}); !errors.Is(err, ErrResumeStateRefNotSerializable) {
		t.Fatalf("reference serialization error = %v", err)
	}
	if ResumeStateDiscarded == ResumeStateAlreadyAbsent ||
		ResumeStateAlreadyAbsent == ResumeStateDiscardNeedsAttention ||
		!ResumeStateDiscarded.Valid() || !ResumeStateAlreadyAbsent.Valid() ||
		!ResumeStateDiscardNeedsAttention.Valid() {
		t.Fatal("public discard result vocabulary is not closed")
	}
}
