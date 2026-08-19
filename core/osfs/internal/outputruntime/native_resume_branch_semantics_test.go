package outputruntime

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"testing"

	"github.com/windshare/windshare/core/osfs/internal/checkpointmodel"
	"github.com/windshare/windshare/core/osfs/internal/checkpointstore"
	"github.com/windshare/windshare/core/osfs/internal/destinationauthority"
	"github.com/windshare/windshare/core/osfs/internal/outputcap"
	"github.com/windshare/windshare/core/osfs/internal/resumeauthority"
	"github.com/windshare/windshare/core/transfer"
	"github.com/windshare/windshare/core/transfer/receivecontract"
)

type nilRootResumePlatform struct{ outputcap.Platform }

func (nilRootResumePlatform) Root() outputcap.Directory { return nil }

func TestNativeResumePresenceRejectsForeignOrUnavailablePrivateState(t *testing.T) {
	root := newRuntimeTestRootSpec(t).path
	failure := errors.New("platform unavailable")
	failed, err := NewNativeResumeRepository(
		root,
		func(string, bool) (outputcap.Platform, error) { return nil, failure },
		nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := failed.Page(context.Background(), resumeauthority.PageCursor{}, 1); !errors.Is(err, failure) {
		t.Fatalf("page platform failure = %v", err)
	} else if diagnostic, ok := FilesystemOutputDiagnosticFor(err); !ok ||
		diagnostic.Stage != FilesystemOutputFailureDestinationBinding {
		t.Fatalf("page platform diagnostic = (%+v, %t)", diagnostic, ok)
	}
	operation := incrementalTestIdentity16[receivecontract.OperationID](0xd1)
	if _, err := failed.Acquire(context.Background(), operation); !errors.Is(err, failure) {
		t.Fatalf("acquire platform failure = %v", err)
	} else if diagnostic, ok := FilesystemOutputDiagnosticFor(err); !ok ||
		diagnostic.Stage != FilesystemOutputFailureDestinationBinding {
		t.Fatalf("acquire platform diagnostic = (%+v, %t)", diagnostic, ok)
	}

	nilRoot, err := NewNativeResumeRepository(
		root,
		func(path string, create bool) (outputcap.Platform, error) {
			platform, err := openOutputRuntimeTestPlatform(path, create)
			if err != nil {
				return nil, err
			}
			return nilRootResumePlatform{Platform: platform}, nil
		},
		nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := nilRoot.Page(context.Background(), resumeauthority.PageCursor{}, 1); !errors.Is(err, outputcap.ErrRecoverableOutputUnsupported) {
		t.Fatalf("nil platform root = %v", err)
	}

	foreignRoot := newRuntimeTestRootSpec(t).path
	if err := os.WriteFile(
		filepath.Join(foreignRoot, checkpointstore.ControlDirectory), []byte("foreign"), 0o600,
	); err != nil {
		t.Fatal(err)
	}
	foreign, err := NewNativeResumeRepository(foreignRoot, openOutputRuntimeTestPlatform, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := foreign.Page(context.Background(), resumeauthority.PageCursor{}, 1); !errors.Is(err, outputcap.ErrUnsafeNamespace) {
		t.Fatalf("foreign control file = %v", err)
	}

	foreignRegistryRoot := newRuntimeTestRootSpec(t).path
	control := filepath.Join(foreignRegistryRoot, checkpointstore.ControlDirectory)
	if err := os.Mkdir(control, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(
		filepath.Join(control, checkpointstore.OrdinaryRegistryDirectoryV1), []byte("foreign"), 0o600,
	); err != nil {
		t.Fatal(err)
	}
	foreignRegistry, err := NewNativeResumeRepository(
		foreignRegistryRoot, openOutputRuntimeTestPlatform, nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := foreignRegistry.Page(
		context.Background(), resumeauthority.PageCursor{}, 1,
	); !errors.Is(err, outputcap.ErrUnsafeNamespace) {
		t.Fatalf("foreign ordinary registry file = %v", err)
	}
}

func TestNativeResumePagingStaysOperationScopedAcrossMultipleSiblings(t *testing.T) {
	root := newRuntimeTestRootSpec(t).path
	first := openOrdinaryResumeSession(t, root, 0xd2, 1)
	pauseOrdinaryResumeFixture(t, first)
	second := openOrdinaryResumeSession(t, root, 0xd8, 1)
	pauseOrdinaryResumeFixture(t, second)

	repository, err := NewNativeResumeRepository(root, openOutputRuntimeTestPlatform, nil)
	if err != nil {
		t.Fatal(err)
	}
	page, err := repository.Page(
		context.Background(), resumeauthority.PageCursor{}, 1,
	)
	if err != nil || len(page.Headers()) != 1 || page.Next().IsZero() {
		t.Fatalf("first bounded page = (headers %d next %t, %v)",
			len(page.Headers()), page.Next().IsZero(), err)
	}
	next, err := repository.Page(context.Background(), page.Next(), 1)
	if err != nil || len(next.Headers()) != 1 {
		t.Fatalf("second bounded page = (headers %d, %v)", len(next.Headers()), err)
	}
	if next.Headers()[0].Record().OperationID() == page.Headers()[0].Record().OperationID() {
		t.Fatal("operation cursor replayed the same sibling")
	}
	if _, err := repository.Page(
		context.Background(), resumeauthority.PageCursor{}, 0,
	); err == nil {
		t.Fatal("invalid bounded page size succeeded")
	}
	missing := incrementalTestIdentity16[receivecontract.OperationID](0xee)
	if _, err := repository.Acquire(context.Background(), missing); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("missing operation in existing registry = %v", err)
	}
}

func TestNativeResumeLeaseCancellationAndCachedAuthorityAreExplicit(t *testing.T) {
	root := newRuntimeTestRootSpec(t).path
	fixture := openOrdinaryResumeSession(t, root, 0xe1, 1)
	pauseOrdinaryResumeFixture(t, fixture)
	repository, err := NewNativeResumeRepository(root, openOutputRuntimeTestPlatform, nil)
	if err != nil {
		t.Fatal(err)
	}
	acquired, err := repository.Acquire(context.Background(), fixture.intent.OperationID())
	if err != nil {
		t.Fatal(err)
	}
	lease := acquired.(*NativeResumeLease)
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := lease.Snapshot(canceled); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled snapshot = %v", err)
	}
	if _, err := lease.Transition(
		canceled, checkpointmodel.OrdinaryLifecycleContinue, checkpointmodel.OrdinaryReasonNone,
	); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled transition = %v", err)
	}
	if _, err := lease.Cleanup(canceled); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled cleanup = %v", err)
	}
	if _, err := lease.Transition(
		context.Background(), checkpointmodel.OrdinaryLifecycleEvent(255),
		checkpointmodel.OrdinaryReasonNone,
	); err == nil {
		t.Fatal("unknown lifecycle transition succeeded")
	}
	if _, err := lease.Snapshot(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := lease.Snapshot(context.Background()); err != nil {
		t.Fatalf("cached snapshot authority = %v", err)
	}
	if err := lease.Close(); err != nil {
		t.Fatal(err)
	}
	if err := lease.Close(); err != nil {
		t.Fatalf("idempotent lease close = %v", err)
	}
	if _, err := lease.Snapshot(context.Background()); !errors.Is(err, transfer.ErrInvalidOutputBinding) {
		t.Fatalf("closed snapshot = %v", err)
	}
	if closeNativeResumeRepository(nil) != nil ||
		closeNativeResumeTopLevel(nil) != nil ||
		closeNativeResumeOperationRegistryLease(nil) != nil {
		t.Fatal("nil native resume resources were not inert")
	}
}

func TestResultRootResumeDispositionUsesAuthorityCreatedRoot(t *testing.T) {
	root := newRuntimeTestRootSpec(t).path
	selection := nativeReservationTestSelection(t, 0xe8)
	layout := receivecontract.NewSyntheticSelectionResultRoot()
	artifact, err := receivecontract.NewResultRootDirectoryTree(layout)
	if err != nil {
		t.Fatal(err)
	}
	authority := newNativeReservationTestAuthority(t, root)
	if _, err := authority.BindDestination(context.Background()); err != nil {
		t.Fatal(err)
	}
	lookup, err := authority.LookupActive(context.Background(), selection)
	if err != nil {
		t.Fatal(err)
	}
	operation, err := authority.CreateOperation(context.Background(), lookup, artifact)
	if err != nil {
		t.Fatal(err)
	}
	intent, ok := operation.ReceiveIntent()
	if !ok {
		t.Fatal("result-root operation omitted intent")
	}
	disposition, err := ordinaryResumeRootDisposition(intent)
	if err != nil || disposition != outputcap.AuthorityCreatedRoot {
		t.Fatalf("result-root disposition = (%q, %v)", disposition, err)
	}
	if err := authority.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestObserveOrdinaryResumeFinalParameterValidationAndHelpers(t *testing.T) {
	var nilContext context.Context

	// Parameter validations on observeOrdinaryResumeFinal
	if _, err := observeOrdinaryResumeFinal(nilContext, nil, nil, checkpointmodel.Record{}); !errors.Is(err, transfer.ErrInvalidOutputBinding) {
		t.Fatalf("nil params error = %v", err)
	}

	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	topLevel := &destinationauthority.TopLevelReservation{}
	store := &checkpointstore.FileExecutionStore{}
	if _, err := observeOrdinaryResumeFinal(canceled, topLevel, store, checkpointmodel.Record{}); !errors.Is(err, transfer.ErrInvalidOutputBinding) {
		t.Fatalf("invalid record error = %v", err)
	}

	// ordinaryResumeItems with invalid parameters
	if _, _, err := ordinaryResumeItems(nilContext, nil, nil); !errors.Is(err, resumeauthority.ErrInvalidContract) {
		t.Fatalf("nil ordinaryResumeItems error = %v", err)
	}

	// Helper functions with nil handles
	if closeNativeResumeDirectory(nil) != nil {
		t.Fatal("closeNativeResumeDirectory(nil) was not nil")
	}
	if closeNativeResumeFile(nil) != nil {
		t.Fatal("closeNativeResumeFile(nil) was not nil")
	}
	if closeNativeResumeEntry(nil) != nil {
		t.Fatal("closeNativeResumeEntry(nil) was not nil")
	}
	if closeNativeResumeOwnedFile(nil) != nil {
		t.Fatal("closeNativeResumeOwnedFile(nil) was not nil")
	}
	if closeOrdinaryResumeDirectories(nil) != nil {
		t.Fatal("closeOrdinaryResumeDirectories(nil) was not nil")
	}
}
