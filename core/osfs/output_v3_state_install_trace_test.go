package osfs

import (
	"testing"

	"github.com/windshare/windshare/core/osfs/internal/resumestate"
)

func TestOutputV3AdoptedStateInstallTraceDecodesEveryRecordScope(t *testing.T) {
	root := v3RecoveryRoot(t)
	selection := v3RecoverySelection(t, true, 1)
	authority := v3RecoveryAuthority(t, root, nil)
	var traces []FilesystemOutputTrace
	authority.tracer = FilesystemOutputTraceFunc(func(trace FilesystemOutputTrace) {
		if trace.Operation == TraceStateInstallCutAdopted {
			traces = append(traces, trace)
		}
	})
	opened := v3RecoveryOpen(t, authority, root, selection)
	defer v3RecoveryCloseSession(t, opened.Session)
	session := opened.Session

	controlEncoded, err := resumestate.EncodeControl(session.control.control)
	if err != nil {
		t.Fatal(err)
	}
	headerEncoded, err := resumestate.EncodeHeader(session.state.Header())
	if err != nil {
		t.Fatal(err)
	}
	file := v3RecoveryOutputFile(t, session, selection, 1)
	var objectID resumestate.OutputObjectID
	objectID[0] = 0xd1
	fileAuthority, err := resumestate.NewFileRecord(resumestate.FileRecordSpec{
		Session: session.stateSnapshot(), Descriptor: file.Descriptor,
		CanonicalLocator: file.Path,
		OutputObject:     objectID,
	})
	if err != nil {
		t.Fatal(err)
	}
	fileEncoded, err := resumestate.EncodeFileRecord(fileAuthority.Bound())
	if err != nil {
		t.Fatal(err)
	}
	locator := resumestate.DigestCanonicalLocator(file.Path)
	fileName := resumestate.FileRecordName(locator)

	rootStore := authority.stateStore([32]byte{}, [16]byte{})
	sessionStore := authority.stateStore(selection.ResumeIntent(), session.SessionID())
	for _, cut := range []struct {
		store  outputStateStore
		name   string
		image  []byte
		stage  FilesystemOutputStateInstallStage
		mutate bool
		sync   bool
	}{
		{store: rootStore, name: resumestate.ControlRecordName, image: controlEncoded, stage: FilesystemOutputStateCreate, mutate: true},
		{store: sessionStore, name: resumestate.HeaderRecordName, image: headerEncoded, stage: FilesystemOutputStateReplace, sync: true},
		{store: sessionStore, name: fileName.Name(), image: fileEncoded, stage: FilesystemOutputStateCreate, mutate: true, sync: true},
	} {
		if cut.store.traceAdoptedInstall == nil {
			t.Fatal("authority state store omitted the adopted-cut trace adapter")
		}
		cut.store.traceAdoptedInstall(outputStateInstallCut{
			stage: cut.stage, targetName: cut.name, encoded: cut.image,
			mutationReportedFailure: cut.mutate, parentSyncReportedFailure: cut.sync,
		})
	}
	if len(traces) != 3 {
		t.Fatalf("adopted state-install traces = %d, want 3", len(traces))
	}
	if traces[0].ResumeIntent != ([32]byte{}) || traces[0].SessionID != ([16]byte{}) ||
		traces[0].StateGeneration != session.control.control.Generation() ||
		traces[0].StateInstallStage != FilesystemOutputStateCreate || !traces[0].MutationReportedFailure {
		t.Fatalf("control adopted-cut trace = %+v", traces[0])
	}
	if traces[1].ResumeIntent != selection.ResumeIntent() || traces[1].SessionID != session.SessionID() ||
		traces[1].StateGeneration != session.state.Header().StateGeneration() ||
		traces[1].StateInstallStage != FilesystemOutputStateReplace || !traces[1].ParentSyncReportedFailure {
		t.Fatalf("header adopted-cut trace = %+v", traces[1])
	}
	if traces[2].ResumeIntent != selection.ResumeIntent() || traces[2].SessionID != session.SessionID() ||
		traces[2].LocatorDigest != outputLocatorDigestFromState(locator) ||
		traces[2].StateGeneration != fileAuthority.Bound().Record().StateGeneration() ||
		traces[2].StateInstallStage != FilesystemOutputStateCreate ||
		!traces[2].MutationReportedFailure || !traces[2].ParentSyncReportedFailure {
		t.Fatalf("file adopted-cut trace = %+v", traces[2])
	}
}
