package outputruntime

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"syscall"
	"testing"

	"github.com/windshare/windshare/core/catalog"
	"github.com/windshare/windshare/core/osfs/internal/outputcap"
	"github.com/windshare/windshare/core/osfs/internal/resumestate"
	"github.com/windshare/windshare/core/transfer"
)

func TestOutputV3DirectoryAuthorityFailurePausesAndPreservesPublishedWitness(t *testing.T) {
	payload := []byte("published-before-directory-denial")
	for _, test := range []struct {
		name   string
		faults outputV3PublicationDirectoryFaults
	}{
		{name: "finalize open", faults: outputV3PublicationDirectoryFaults{openDirectoryErr: syscall.EPERM}},
		{name: "finalize metadata", faults: outputV3PublicationDirectoryFaults{modifiedErr: syscall.EPERM}},
		{name: "finalize sync", faults: outputV3PublicationDirectoryFaults{syncErr: syscall.EPERM}},
		{name: "finalize close", faults: outputV3PublicationDirectoryFaults{closeErr: syscall.EPERM}},
	} {
		t.Run(test.name, func(t *testing.T) {
			root := v3RecoveryRoot(t)
			selection := outputV3PublicationAuthoritySelection(t, "scoped/file.bin", uint64(len(payload)))
			sessionIDs := &v3RecoverySessionIDs{}
			opened := v3RecoveryOpen(t, v3RecoveryAuthority(t, root, sessionIDs), root, selection)
			directory := transfer.OutputDirectory{
				Path: selection.Directories()[0].Path, ModifiedTime: selection.Directories()[0].ModifiedTime,
			}
			file := v3RecoveryOutputFile(t, opened.Session, selection, uint64(len(payload)))
			transaction := v3RecoveryBeginTransaction(t, opened.Session, file).(*FileTransaction)
			if err := transaction.WriteRange(context.Background(), 0, payload); err != nil {
				t.Fatal(err)
			}
			settlement, err := transaction.Commit(context.Background())
			if err != nil || settlement.Kind() != transfer.FilePublished {
				t.Fatalf("publish before directory denial = (kind=%v, err=%v)", settlement.Kind(), err)
			}
			record := transaction.resumable.Bound().Record()

			originalPlatform := opened.Session.platform
			faults := test.faults
			opened.Session.platform = &outputV3PublicationPlatform{
				Platform: originalPlatform,
				root: &outputV3PublicationDirectory{
					Directory: originalPlatform.Root(), faults: &faults,
				},
			}
			operationErr := opened.Session.FinalizeDirectory(context.Background(), directory)
			if !errors.Is(operationErr, syscall.EPERM) {
				t.Fatalf("directory authority error = %v, want EPERM", operationErr)
			}
			var fault *transfer.OutputFault
			if !errors.As(operationErr, &fault) || fault.Scope() != transfer.OutputFaultSession ||
				fault.Code() != transfer.OutputFaultStateIO || !fault.RequiresJobPause() {
				t.Fatalf("directory authority fault = %#v, want pausing session state-I/O", fault)
			}
			opened.Session.platform = originalPlatform
			outputV3AssertPublishedAuthorityRetained(t, root, selection, opened.Session.SessionID(), record, payload)

			paused, err := opened.Session.PauseJob(context.Background(), transfer.JobPauseOutputFailure)
			if err != nil || paused.Kind() != transfer.JobPaused {
				t.Fatalf("directory failure pause = (kind=%v, err=%v)", paused.Kind(), err)
			}
			outputV3AssertPublishedAuthorityRetained(t, root, selection, opened.Session.SessionID(), record, payload)

			reopened := v3RecoveryOpen(t, v3RecoveryAuthority(t, root, sessionIDs), root, selection)
			t.Cleanup(func() { v3RecoveryCloseSession(t, reopened.Session) })
			published, err := reopened.Session.BeginFile(
				context.Background(), v3RecoveryOutputFile(t, reopened.Session, selection, uint64(len(payload))),
			)
			immediateSettlement, immediate := published.ImmediateSettlement()
			if err != nil || !immediate || immediateSettlement.Kind() != transfer.FilePublished {
				t.Fatalf(
					"published resume without retransmission = (kind=%v, immediate=%t, err=%v)",
					immediateSettlement.Kind(), immediate, err,
				)
			}
			if err := reopened.Session.FinalizeDirectory(context.Background(), directory); err != nil {
				t.Fatal(err)
			}
			completed, err := reopened.Session.CompleteJob(context.Background(), transfer.JobSucceeded)
			if err != nil || completed.Kind() != transfer.JobClosed {
				t.Fatalf("completion after directory authority restore = (kind=%v, err=%v)", completed.Kind(), err)
			}
		})
	}
}

func TestOutputV3FreshAdmissionMaterializesAndBindsSelectedDirectory(t *testing.T) {
	root := v3RecoveryRoot(t)
	selection := outputV3DirectoryAuthoritySelection(t, "scoped")
	opened := v3RecoveryOpen(t, v3RecoveryAuthority(t, root, &v3RecoverySessionIDs{}), root, selection)
	t.Cleanup(func() { v3RecoveryCloseSession(t, opened.Session) })
	path := filepath.Join(root, filepath.FromSlash(selection.Directories()[0].Path))
	if info, err := os.Stat(path); err != nil || !info.IsDir() {
		t.Fatalf("materialized selected directory = (%v, %v)", info, err)
	}
	if _, bound := opened.Session.ancestry.claim(selection.Directories()[0].Path); !bound {
		t.Fatal("selected directory leaf is absent from the admitted ancestry binding")
	}
}

func TestOutputV3FreshDirectoryCreateDenialCreatesNoResumeState(t *testing.T) {
	root := v3RecoveryRoot(t)
	selection := outputV3DirectoryAuthoritySelection(t, "scoped")
	faults := outputV3PublicationDirectoryFaults{createDirectoryErr: syscall.EPERM}
	authority := outputV3DirectoryFaultAuthority(t, root, &faults)
	opened, err := v3OpenSelection(context.Background(), authority, selection)
	if opened.Session != nil || !errors.Is(err, syscall.EPERM) {
		t.Fatalf("fresh directory create denial = (session=%v, err=%v)", opened.Session, err)
	}
	var fault *transfer.OutputFault
	var sessionErr *transfer.OutputSessionError
	if !errors.As(err, &fault) || fault.Scope() != transfer.OutputFaultSession ||
		fault.Code() != transfer.OutputFaultNamespaceUnsafe || errors.As(err, &sessionErr) {
		t.Fatalf("fresh directory create fault = %v", err)
	}
	if _, statErr := os.Stat(filepath.Join(root, "scoped")); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("denied selected directory was materialized: %v", statErr)
	}
	if _, statErr := os.Stat(filepath.Join(root, resumestate.ControlDirectoryName)); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("fresh create denial wrote resume state: %v", statErr)
	}
}

func TestOutputV3FreshDirectorySyncFailureLeavesOnlyRequestedDirectory(t *testing.T) {
	root := v3RecoveryRoot(t)
	selection := outputV3DirectoryAuthoritySelection(t, "scoped")
	faults := outputV3PublicationDirectoryFaults{syncErr: syscall.EPERM}
	opened, err := v3OpenSelection(
		context.Background(), outputV3DirectoryFaultAuthority(t, root, &faults), selection,
	)
	if opened.Session != nil || !errors.Is(err, syscall.EPERM) {
		t.Fatalf("fresh directory sync failure = (session=%v, err=%v)", opened.Session, err)
	}
	var fault *transfer.OutputFault
	var sessionErr *transfer.OutputSessionError
	if !errors.As(err, &fault) || fault.Scope() != transfer.OutputFaultSession ||
		fault.Code() != transfer.OutputFaultNamespaceUnsafe || errors.As(err, &sessionErr) {
		t.Fatalf("fresh directory sync fault = %v", err)
	}
	if info, statErr := os.Stat(filepath.Join(root, "scoped")); statErr != nil || !info.IsDir() {
		t.Fatalf("requested directory after sync failure = (%v, %v)", info, statErr)
	}
	if _, statErr := os.Stat(filepath.Join(root, resumestate.ControlDirectoryName)); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("fresh directory sync failure wrote resume state: %v", statErr)
	}
}

func TestOutputV3MatchingRestartRejectsMissingSelectedDirectory(t *testing.T) {
	root := v3RecoveryRoot(t)
	selection := outputV3DirectoryAuthoritySelection(t, "scoped")
	sessionIDs := &v3RecoverySessionIDs{}
	authority := v3RecoveryAuthority(t, root, sessionIDs)
	opened := v3RecoveryOpen(t, authority, root, selection)
	sessionID := opened.Session.SessionID()
	path := filepath.Join(root, filepath.FromSlash(selection.Directories()[0].Path))
	v3RecoveryCloseSession(t, opened.Session)
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	reopened, err := v3OpenSelection(
		context.Background(), v3RecoveryAuthority(t, root, sessionIDs), selection,
	)
	if reopened.Session != nil || !errors.Is(err, errOutputAncestryMismatch) {
		t.Fatalf("restart with missing selected directory = (session=%v, err=%v)", reopened.Session, err)
	}
	outputV3RequirePausingNamespaceFault(t, err)
	if _, statErr := os.Stat(path); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("restart recreated missing selected directory: %v", statErr)
	}
	if _, statErr := os.Stat(v3RecoverySessionPath(root, selection, sessionID)); statErr != nil {
		t.Fatalf("restart mismatch removed matching session state: %v", statErr)
	}
}

func TestOutputV3MatchingRestartRejectsReplacedSelectedDirectory(t *testing.T) {
	root := v3RecoveryRoot(t)
	selection := outputV3DirectoryAuthoritySelection(t, "scoped")
	sessionIDs := &v3RecoverySessionIDs{}
	opened := v3RecoveryOpen(t, v3RecoveryAuthority(t, root, sessionIDs), root, selection)
	sessionID := opened.Session.SessionID()
	v3RecoveryCloseSession(t, opened.Session)
	path := filepath.Join(root, "scoped")
	displaced := filepath.Join(root, "scoped-displaced")
	if err := os.Rename(path, displaced); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(path, 0o700); err != nil {
		t.Fatal(err)
	}
	reopened, err := v3OpenSelection(
		context.Background(), v3RecoveryAuthority(t, root, sessionIDs), selection,
	)
	if reopened.Session != nil || !errors.Is(err, errOutputAncestryMismatch) {
		t.Fatalf("restart with replaced selected directory = (session=%v, err=%v)", reopened.Session, err)
	}
	outputV3RequirePausingNamespaceFault(t, err)
	if _, statErr := os.Stat(v3RecoverySessionPath(root, selection, sessionID)); statErr != nil {
		t.Fatalf("restart replacement removed matching session state: %v", statErr)
	}
}

func TestOutputV3FreshAdmissionRejectsSelectedDirectoryIdentityRaceBeforeState(t *testing.T) {
	root := v3RecoveryRoot(t)
	selection := outputV3DirectoryAuthoritySelection(t, "scoped")
	// Root consistency and the root duplicate used to exact-reopen a top-level
	// leaf consume the first two comparisons. The third is the retained selected
	// directory against its exact reopen.
	faults := &outputV3PublicationDirectoryFaults{sameDirectoryAt: 3}
	authority := outputV3DirectoryFaultAuthority(t, root, faults)
	opened, err := v3OpenSelection(context.Background(), authority, selection)
	if opened.Session != nil || !errors.Is(err, errOutputAncestryMismatch) {
		t.Fatalf("fresh selected-directory identity race = (session=%v, err=%v)", opened.Session, err)
	}
	var fault *transfer.OutputFault
	if !errors.As(err, &fault) || fault.Scope() != transfer.OutputFaultSession ||
		fault.Code() != transfer.OutputFaultNamespaceUnsafe {
		t.Fatalf("fresh selected-directory identity-race fault = %#v", fault)
	}
	if _, found := errors.AsType[*transfer.OutputSessionError](err); found {
		t.Fatalf("fresh selected-directory identity race requested preservation: %v", err)
	}
	if _, statErr := os.Stat(filepath.Join(root, resumestate.ControlDirectoryName)); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("fresh selected-directory identity race wrote resume state: %v", statErr)
	}
}

func TestOutputV3FreshAdmissionRejectsGuardRootDisplacementBeforeMaterialization(t *testing.T) {
	root := v3RecoveryRoot(t)
	displacedRoot := v3RecoveryRoot(t)
	selection := outputV3DirectoryAuthoritySelection(t, "scoped")
	authority := v3RecoveryAuthority(t, root, nil)
	authority.platformFactory = func(path string, create bool) (outputcap.Platform, error) {
		platform, err := openOutputRuntimeTestPlatform(path, create)
		if err != nil {
			return nil, err
		}
		displaced, err := openOutputRuntimeTestPlatform(displacedRoot, false)
		if err != nil {
			return nil, errors.Join(err, platform.Close())
		}
		return &outputV3AdmissionDisplacementPlatform{
			Platform:  platform,
			displaced: displaced,
		}, nil
	}
	opened, err := v3OpenSelection(context.Background(), authority, selection)
	if opened.Session != nil || !errors.Is(err, errOutputAncestryMismatch) {
		t.Fatalf("guard-root displacement = (session=%v, err=%v)", opened.Session, err)
	}
	if _, found := errors.AsType[*transfer.OutputSessionError](err); found {
		t.Fatalf("fresh guard-root displacement requested resume preservation: %v", err)
	}
	for _, candidate := range []string{root, displacedRoot} {
		if _, statErr := os.Stat(filepath.Join(candidate, "scoped")); !errors.Is(statErr, os.ErrNotExist) {
			t.Fatalf("selected directory was created through displaced root %q: %v", candidate, statErr)
		}
		if _, statErr := os.Stat(filepath.Join(candidate, resumestate.ControlDirectoryName)); !errors.Is(statErr, os.ErrNotExist) {
			t.Fatalf("guard-root displacement wrote resume state under %q: %v", candidate, statErr)
		}
	}
}

func TestOutputV3FinalizeDirectoryCannotRecreateBoundSelectedDirectory(t *testing.T) {
	root := v3RecoveryRoot(t)
	selection := outputV3DirectoryAuthoritySelection(t, "scoped")
	opened := v3RecoveryOpen(t, v3RecoveryAuthority(t, root, nil), root, selection)
	t.Cleanup(func() { v3RecoveryCloseSession(t, opened.Session) })
	directory := transfer.OutputDirectory{
		Path: selection.Directories()[0].Path, ModifiedTime: selection.Directories()[0].ModifiedTime,
	}
	path := filepath.Join(root, filepath.FromSlash(directory.Path))
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	err := opened.Session.FinalizeDirectory(context.Background(), directory)
	if !errors.Is(err, errOutputAncestryMismatch) {
		t.Fatalf("missing selected directory error = %v", err)
	}
	var fault *transfer.OutputFault
	if !errors.As(err, &fault) || fault.Scope() != transfer.OutputFaultSession ||
		fault.Code() != transfer.OutputFaultNamespaceUnsafe {
		t.Fatalf("missing selected directory fault = %#v", fault)
	}
	var sessionErr *transfer.OutputSessionError
	if !errors.As(err, &sessionErr) || !sessionErr.RequiresJobPause() {
		t.Fatalf("missing selected directory did not require session preservation: %v", err)
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("unsafe selected directory was recreated: %v", err)
	}
}

func outputV3RequirePausingNamespaceFault(t *testing.T, err error) {
	t.Helper()
	var fault *transfer.OutputFault
	var sessionErr *transfer.OutputSessionError
	if !errors.As(err, &fault) || fault.Scope() != transfer.OutputFaultSession ||
		fault.Code() != transfer.OutputFaultNamespaceUnsafe ||
		!errors.As(err, &sessionErr) || !sessionErr.RequiresJobPause() {
		t.Fatalf("resume ancestry fault = %v, want pausing Session/NamespaceUnsafe", err)
	}
}

func outputV3DirectoryFaultAuthority(
	t *testing.T,
	root string,
	faults *outputV3PublicationDirectoryFaults,
) *Authority {
	t.Helper()
	authority := v3RecoveryAuthority(t, root, nil)
	authority.platformFactory = func(path string, create bool) (outputcap.Platform, error) {
		platform, err := openOutputRuntimeTestPlatform(path, create)
		if err != nil {
			return nil, err
		}
		decoratedRoot := &outputV3PublicationDirectory{
			Directory: platform.Root(), faults: faults,
		}
		return &outputV3PublicationPlatform{
			Platform: platform,
			root:     decoratedRoot,
		}, nil
	}
	return authority
}

type outputV3AdmissionDisplacementPlatform struct {
	outputcap.Platform
	displaced     outputcap.Platform
	guardAcquired bool
}

func (platform *outputV3AdmissionDisplacementPlatform) Root() outputcap.Directory {
	if platform.guardAcquired {
		return platform.displaced.Root()
	}
	return platform.Platform.Root()
}

func (platform *outputV3AdmissionDisplacementPlatform) AcquirePublicOperationGuard() (
	outputcap.PublicOperationGuard,
	error,
) {
	guard, err := platform.Platform.AcquirePublicOperationGuard()
	if err == nil {
		platform.guardAcquired = true
	}
	return guard, err
}

func (platform *outputV3AdmissionDisplacementPlatform) Close() error {
	return errors.Join(platform.Platform.Close(), platform.displaced.Close())
}

func outputV3DirectoryAuthoritySelection(t *testing.T, path string) transfer.OutputSelection {
	t.Helper()
	share := v3RecoveryIdentity16[catalog.ShareInstance](0x91)
	root := v3RecoveryIdentity16[catalog.DirectoryID](0x92)
	generation := v3RecoveryIdentity16[catalog.DirectoryGeneration](0x93)
	plan, err := transfer.NewOutputSelection(
		share,
		root,
		generation,
		[]transfer.OutputSelectionDirectory{{
			Path: path, DirectoryID: v3RecoveryIdentity16[catalog.DirectoryID](0x94),
			Generation:   v3RecoveryIdentity16[catalog.DirectoryGeneration](0x95),
			ModifiedTime: v3RecoveryModifiedTime(t),
		}},
		nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	rules, err := transfer.NewSelectionRules(true, nil)
	if err != nil {
		t.Fatal(err)
	}
	request, err := transfer.NewCanonicalSelectionRequest(share, root, rules)
	if err != nil {
		t.Fatal(err)
	}
	canonical, err := transfer.NewCanonicalSelectionV1(request, plan)
	if err != nil {
		t.Fatal(err)
	}
	selection, err := canonical.BindPlan(plan)
	if err != nil {
		t.Fatal(err)
	}
	return selection
}

func outputV3AssertPublishedAuthorityRetained(
	t *testing.T,
	root string,
	selection transfer.OutputSelection,
	sessionID transfer.OutputSessionID,
	expected resumestate.FileRecord,
	payload []byte,
) {
	t.Helper()
	record := readOutputV3PublicationAuthorityRecord(t, root, selection, sessionID, expected)
	if record.Phase() != resumestate.FilePublished || record.OutputObject() != expected.OutputObject() ||
		record.QuarantineReason().Valid() || record.RetirementReason().Valid() {
		t.Fatalf(
			"retained published record = (phase=%v, object=%v, quarantine=%v, retirement=%v)",
			record.Phase(), record.OutputObject(), record.QuarantineReason(), record.RetirementReason(),
		)
	}
	anchorPath := outputV3PublicationAuthorityAnchorPath(root, selection, sessionID, record)
	if _, err := os.Stat(anchorPath); err != nil {
		t.Fatalf("published anchor was not retained: %v", err)
	}
	actual, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(record.CanonicalLocator())))
	if err != nil || !bytes.Equal(actual, payload) {
		t.Fatalf("retained final = %q, %v, want %q", actual, err, payload)
	}
}
