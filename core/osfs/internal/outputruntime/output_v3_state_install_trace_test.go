package outputruntime

import (
	"errors"
	"testing"

	"github.com/windshare/windshare/core/osfs/internal/outputnamespace"
	"github.com/windshare/windshare/core/osfs/internal/resumestate"
)

const (
	stateInstallTraceRootName    = "state-install-trace"
	stateInstallTraceControlName = "control"
	stateInstallTraceHeaderName  = "header"
	stateInstallTraceFileName    = "file"
)

func TestOutputV3AdoptedStateInstallTraceDecodesEveryRecordScope(t *testing.T) {
	t.Parallel()
	root := v3RecoveryRoot(t)
	selection := v3RecoverySelection(t, true, 1)
	authority := v3RecoveryAuthority(t, root, nil)
	opened := v3RecoveryOpen(t, authority, root, selection)
	defer v3RecoveryCloseSession(t, opened.Session)
	session := opened.Session

	var traces []FilesystemOutputTrace
	authority.tracer = FilesystemOutputTraceFunc(func(trace FilesystemOutputTrace) {
		if trace.Operation == TraceStateInstallCutAdopted {
			traces = append(traces, trace)
		}
	})

	traceRoot, err := session.platform.Root().CreateDirectory(stateInstallTraceRootName, true)
	if err != nil {
		t.Fatal(err)
	}
	controlDirectory, err := traceRoot.CreateDirectory(stateInstallTraceControlName, true)
	if err != nil {
		_ = traceRoot.Close()
		t.Fatal(err)
	}
	headerDirectory, err := traceRoot.CreateDirectory(stateInstallTraceHeaderName, true)
	if err != nil {
		_ = errors.Join(controlDirectory.Close(), traceRoot.Close())
		t.Fatal(err)
	}
	fileDirectory, err := traceRoot.CreateDirectory(stateInstallTraceFileName, true)
	if err != nil {
		_ = errors.Join(headerDirectory.Close(), controlDirectory.Close(), traceRoot.Close())
		t.Fatal(err)
	}
	defer func() {
		if err := errors.Join(fileDirectory.Close(), headerDirectory.Close(), controlDirectory.Close(), traceRoot.Close()); err != nil {
			t.Error(err)
		}
	}()

	control := session.control.Control()
	controlEncoded, err := resumestate.EncodeControl(control)
	if err != nil {
		t.Fatal(err)
	}
	currentState := session.stateSnapshot()
	currentHeader := currentState.Header()
	headerEncoded, err := resumestate.EncodeHeader(currentHeader)
	if err != nil {
		t.Fatal(err)
	}
	nextState, err := currentState.WithLifecycle(resumestate.SessionPausing)
	if err != nil {
		t.Fatal(err)
	}
	nextHeader := nextState.Header()
	nextHeaderEncoded, err := resumestate.EncodeHeader(nextHeader)
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

	controlFault := &stateStoreFaultDirectory{
		Directory: controlDirectory, fault: stateStoreFaultLinkAfterMutation,
	}
	if outcome, err := rootStore.CreateRecord(
		controlFault, resumestate.ControlRecordName, controlEncoded, resumestate.MaxControlStateBytes,
	); outcome != outputnamespace.CreateAdopted || err != nil {
		t.Fatalf("create faulted control record = (%v, %v), want adopted with settled report", outcome, err)
	}

	if outcome, err := sessionStore.CreateRecord(
		headerDirectory, resumestate.HeaderRecordName, headerEncoded, resumestate.MaxSessionHeaderBytes,
	); outcome != outputnamespace.CreateAdopted || err != nil {
		t.Fatalf("create current header record = (%v, %v), want adopted", outcome, err)
	}
	headerFault := &stateStoreFaultDirectory{
		Directory: headerDirectory, target: resumestate.HeaderRecordName, fault: stateStoreFaultParentSync,
	}
	if outcome, err := sessionStore.ReplaceRecord(
		headerFault,
		resumestate.HeaderRecordName,
		outputnamespace.NewRecordImage(headerEncoded, currentHeader.StateGeneration()),
		outputnamespace.NewRecordImage(nextHeaderEncoded, nextHeader.StateGeneration()),
		resumestate.MaxSessionHeaderBytes,
	); outcome != outputnamespace.ReplaceAdopted || err != nil {
		t.Fatalf("replace faulted header record = (%v, %v), want adopted with settled report", outcome, err)
	}

	fileFault := &stateStoreFaultDirectory{
		Directory: fileDirectory, fault: stateStoreFaultLinkAfterMutationAndParentSync,
	}
	if outcome, err := sessionStore.CreateRecord(
		fileFault, fileName.Name(), fileEncoded, resumestate.MaxFileStateBytes,
	); outcome != outputnamespace.CreateAdopted || err != nil {
		t.Fatalf("create faulted file record = (%v, %v), want adopted with settled report", outcome, err)
	}
	if len(traces) != 3 {
		t.Fatalf("adopted state-install traces = %d, want 3", len(traces))
	}
	if traces[0].ResumeIntent != ([32]byte{}) || traces[0].SessionID != ([16]byte{}) ||
		traces[0].StateGeneration != control.Generation() ||
		traces[0].StateInstallStage != FilesystemOutputStateCreate ||
		!traces[0].MutationReportedFailure || traces[0].ParentSyncReportedFailure {
		t.Fatalf("control adopted-cut trace = %+v", traces[0])
	}
	if traces[1].ResumeIntent != selection.ResumeIntent() || traces[1].SessionID != session.SessionID() ||
		traces[1].StateGeneration != nextHeader.StateGeneration() ||
		traces[1].StateInstallStage != FilesystemOutputStateReplace ||
		traces[1].MutationReportedFailure || !traces[1].ParentSyncReportedFailure {
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
