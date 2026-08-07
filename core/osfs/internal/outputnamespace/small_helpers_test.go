package outputnamespace

import (
	"testing"

	"github.com/windshare/windshare/core/osfs/internal/resumestate"
	"github.com/windshare/windshare/core/transfer"
)

func TestObserverAdaptersIgnoreNilAndForwardCuts(t *testing.T) {
	var nilObserver ObserverFunc
	nilObserver.ObserveStateInstall(StateInstallEvent{})

	var got StateInstallEvent
	ObserverFunc(func(event StateInstallEvent) { got = event }).ObserveStateInstall(StateInstallEvent{
		IntentDigest: transfer.TransferIntentDigest{1},
		SessionID:    transfer.OutputSessionID{2},
		Cut:          StateInstallCut{stage: StateInstallCreate, targetName: resumestate.HeaderRecordName},
	})
	if got.IntentDigest.IsZero() || got.SessionID.IsZero() || got.Cut.Stage() != StateInstallCreate {
		t.Fatalf("observer did not receive event: %+v", got)
	}

	var nilStateObserver StateInstallObserverFunc
	nilStateObserver.ObserveStateInstall(StateInstallCut{})
	var observed StateInstallCut
	StateInstallObserverFunc(func(cut StateInstallCut) { observed = cut }).ObserveStateInstall(StateInstallCut{
		stage: StateInstallReplace, targetName: resumestate.ControlRecordName,
		encoded: []byte("state"), mutationReportedFailure: true, parentSyncReportedFailure: true,
	})
	if observed.Stage() != StateInstallReplace || observed.TargetName() != resumestate.ControlRecordName ||
		string(observed.Encoded()) != "state" || !observed.MutationReportedFailure() || !observed.ParentSyncReportedFailure() {
		t.Fatalf("state observer did not receive cut: %+v", observed)
	}
}

func TestNamespaceAndSessionValueAccessors(t *testing.T) {
	directory := &ControlNamespace{}
	if directory.Directory() != nil || directory.Sessions() != nil || directory.Control() != (resumestate.Control{}) {
		t.Fatalf("empty control namespace accessors returned unexpected values")
	}
	if (*ControlNamespace)(nil).Close() != nil {
		t.Fatal("nil control namespace close should be harmless")
	}

	var mismatch *AncestryMismatchError
	if mismatch.Error() != "osfs: output session ancestry binding changed" || !mismatch.SessionID().IsZero() {
		t.Fatal("nil ancestry mismatch should have stable zero identity")
	}
	if (&AncestryMismatchError{sessionID: transfer.OutputSessionID{3}}).SessionID().IsZero() {
		t.Fatal("non-nil ancestry mismatch lost session identity")
	}
	if (&AncestryMismatchError{}).Error() == "" {
		t.Fatal("ancestry mismatch error must be descriptive")
	}
}

func TestStateInstallCutAndTerminalLayoutAccessors(t *testing.T) {
	cut := StateInstallCut{
		stage: StateInstallReplace, targetName: "target", encoded: []byte("encoded"),
		mutationReportedFailure: true, parentSyncReportedFailure: true,
	}
	encoded := cut.Encoded()
	encoded[0] = 'X'
	if cut.Stage() != StateInstallReplace || cut.TargetName() != "target" || string(cut.Encoded()) != "encoded" ||
		!cut.MutationReportedFailure() || !cut.ParentSyncReportedFailure() {
		t.Fatalf("cut accessors = %+v", cut)
	}

	layout := &TerminalLayout{cut: 7}
	if layout.Cut() != 7 || layout.Stages() != nil || layout.Anchors() != nil || layout.Files() != nil || layout.Lock() != nil {
		t.Fatalf("empty terminal layout accessors returned unexpected values")
	}
	if layout.Close() != nil || (*TerminalLayout)(nil).Close() != nil {
		t.Fatal("terminal layout close should tolerate empty ownership")
	}
}
