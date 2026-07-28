package outputruntime

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"

	"github.com/windshare/windshare/core/osfs/internal/outputcap"
	"github.com/windshare/windshare/core/osfs/internal/outputfault"
	"github.com/windshare/windshare/core/osfs/internal/outputnamespace"
	"github.com/windshare/windshare/core/osfs/internal/resumestate"
	"github.com/windshare/windshare/core/transfer"
)

func TestOutputV3BoundaryListEmptyRootIsReadOnlyAndRejectsUninstalledCandidate(t *testing.T) {
	t.Parallel()
	root := v3RecoveryRoot(t)
	authority := v3RecoveryAuthority(t, root, nil)

	inventory, err := authority.ListResumeState(context.Background(), root)
	if err != nil {
		t.Fatal(err)
	}
	if summaries := inventory.Summaries(); len(summaries) != 0 {
		t.Fatalf("empty-root resume summaries = %+v", summaries)
	}
	if err := inventory.Close(); err != nil {
		t.Fatal(err)
	}
	if entries, err := os.ReadDir(root); err != nil || len(entries) != 0 {
		t.Fatalf("empty-root listing mutated namespace: entries=%v error=%v", entries, err)
	}

	candidateName := resumestate.BootstrapCandidatePrefix + "uninstalled"
	candidatePath := filepath.Join(root, candidateName)
	if err := os.Mkdir(candidatePath, 0o700); err != nil {
		t.Fatal(err)
	}
	failed, err := authority.ListResumeState(context.Background(), root)
	if failed != nil {
		defer v3RecoveryCloseInventory(t, failed)
	}
	assertOutputV3BoundaryFault(t, err, transfer.OutputFaultRoot, transfer.OutputFaultNamespaceUnsafe)
	if info, statErr := os.Stat(candidatePath); statErr != nil || !info.IsDir() {
		t.Fatalf("read-only listing changed uninstalled candidate: info=%v error=%v", info, statErr)
	}
}

func TestOutputV3BoundaryCanceledDiscardDoesNotConsumeReference(t *testing.T) {
	t.Parallel()
	root := v3RecoveryRoot(t)
	selection := v3RecoverySelection(t, false, 0)
	authority := v3RecoveryAuthority(t, root, nil)
	opened := v3RecoveryOpen(t, authority, root, selection)
	v3RecoveryCloseSession(t, opened.Session)

	inventory, err := authority.ListResumeState(context.Background(), root)
	if err != nil {
		t.Fatal(err)
	}
	defer v3RecoveryCloseInventory(t, inventory)
	summaries := inventory.Summaries()
	if len(summaries) != 1 {
		t.Fatalf("resume summaries before canceled discard = %+v", summaries)
	}
	reference := summaries[0].Reference
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := authority.DiscardResumeState(canceled, reference); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled discard = %v, want context cancellation", err)
	}
	settlement, err := authority.DiscardResumeState(context.Background(), reference)
	if err != nil || settlement.Kind != Discarded {
		t.Fatalf("discard after cancellation = (%+v, %v)", settlement, err)
	}

	after, err := authority.ListResumeState(context.Background(), root)
	if err != nil {
		t.Fatal(err)
	}
	defer v3RecoveryCloseInventory(t, after)
	if summaries := after.Summaries(); len(summaries) != 0 {
		t.Fatalf("resume summaries after discard = %+v", summaries)
	}
}

func TestOutputV3BoundaryMultipleSessionsRemainIndividuallyDiscardable(t *testing.T) {
	t.Parallel()
	root := v3RecoveryRoot(t)
	selection := v3RecoverySelection(t, false, 0)
	sessionIDs := &v3RecoverySessionIDs{}
	authority := v3RecoveryAuthority(t, root, sessionIDs)
	first := v3RecoveryOpen(t, authority, root, selection)
	v3RecoveryCloseSession(t, first.Session)

	platform, err := openOutputRuntimeTestPlatform(root, false)
	if err != nil {
		t.Fatal(err)
	}
	controller := outputnamespace.NewController(outputnamespace.ControllerConfig{
		Backend:    filesystemOutputBackendID,
		Random:     bytes.NewReader(bytes.Repeat([]byte{0x5e}, 256)),
		SessionIDs: sessionIDs,
	})
	control, err := controller.OpenInstalledControl(platform.Root(), platform)
	if err != nil {
		_ = platform.Close()
		t.Fatal(err)
	}
	intentName := resumestate.ResumeNamespaceName(selection.ResumeIntent())
	intent, err := control.Sessions().OpenDirectory(intentName, true)
	if err != nil {
		t.Fatal(err)
	}
	// Masking the incumbent enumeration models a concurrent creator that began
	// from an earlier empty cut while keeping every mutation on the real intent.
	concurrentIntent := &outputV3BoundaryEmptyNamesDirectory{Directory: intent}
	secondResult, err := controller.OpenOrCreateSession(
		concurrentIntent, control.Control(), selection,
		v3RecoveryAncestryBinding(t, control.Control().OutputRoot(), selection),
	)
	if err != nil || secondResult.Disposition != outputnamespace.SessionInstalled {
		t.Fatalf("create second fixed session = disposition %v, error %v", secondResult.Disposition, err)
	}
	if err := errors.Join(secondResult.Directory.Close(), intent.Close(), control.Close(), platform.Close()); err != nil {
		t.Fatal(err)
	}

	inventory, err := authority.ListResumeState(context.Background(), root)
	if err != nil {
		t.Fatal(err)
	}
	defer v3RecoveryCloseInventory(t, inventory)
	summaries := inventory.Summaries()
	if len(summaries) != 2 {
		t.Fatalf("duplicate-intent session summaries = %+v, want two", summaries)
	}
	for _, summary := range summaries {
		if summary.Reference.Kind() != ResumeStateNeedsAttention ||
			!runtimeBoundaryHasAttention(summary, "multiple-sessions-for-intent") {
			t.Fatalf("duplicate-intent summary = %+v, want intent-scoped attention", summary)
		}
	}
	for index, summary := range summaries {
		settlement, err := authority.DiscardResumeState(context.Background(), summary.Reference)
		if err != nil || settlement.Kind != Discarded {
			t.Fatalf("discard duplicate-intent session %d = (%+v, %v)", index, settlement, err)
		}
		if index == 0 {
			intentPath := filepath.Join(
				root, resumestate.ControlDirectoryName, resumestate.SessionsDirectoryName, intentName,
			)
			if info, statErr := os.Stat(intentPath); statErr != nil || !info.IsDir() {
				t.Fatalf("first discard removed non-empty intent shell: info=%v error=%v", info, statErr)
			}
		}
	}
	intentPath := filepath.Join(root, resumestate.ControlDirectoryName, resumestate.SessionsDirectoryName, intentName)
	if _, err := os.Stat(intentPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("last discard retained empty intent shell: %v", err)
	}
}

func TestOutputV3BoundaryStaticAdmissionPrecedesPlatformAuthority(t *testing.T) {
	t.Parallel()
	root := v3RecoveryRoot(t)
	selection := v3RecoverySelection(t, false, 0)
	authority := v3RecoveryAuthority(t, root, nil)
	platformCalls := 0
	authority.platformFactory = func(string, bool) (outputcap.Platform, error) {
		platformCalls++
		return nil, errors.Join(outputcap.ErrRecoverableOutputUnsupported, errors.New("injected unsupported platform"))
	}

	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := v3OpenSelection(canceled, authority, selection); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled admission = %v, want context cancellation", err)
	}
	if _, err := v3OpenSelection(context.Background(), authority, transfer.OutputSelection{}); !errors.Is(err, transfer.ErrInvalidOutputSelection) {
		t.Fatalf("invalid selection = %v, want invalid selection", err)
	}
	if platformCalls != 0 {
		t.Fatalf("static admission failures opened platform %d times", platformCalls)
	}

	if session, err := authority.OpenSelection(context.Background(), selection); session != nil {
		_, _ = session.PauseJob(context.Background(), transfer.JobPauseOutputFailure)
		t.Fatal("unsupported platform returned an output session")
	} else {
		assertOutputV3BoundaryFault(t, err, transfer.OutputFaultRoot, transfer.OutputFaultUnsupportedFilesystem)
		if !errors.Is(err, outputfault.ErrUnsupportedVolume) {
			t.Fatalf("unsupported platform error = %v, want unsupported output volume", err)
		}
	}
	if platformCalls != 1 {
		t.Fatalf("unsupported admission platform calls = %d, want 1", platformCalls)
	}
	if entries, err := os.ReadDir(root); err != nil || len(entries) != 0 {
		t.Fatalf("rejected admissions mutated root: entries=%v error=%v", entries, err)
	}
}

func TestOutputV3BoundaryForcedDirectoryPinRevokesAllRetains(t *testing.T) {
	t.Parallel()
	if pin := newResumeStateDirectoryPin(nil); pin != nil {
		t.Fatalf("nil directory produced authority pin %+v", pin)
	}
	var nilPin *resumeStateDirectoryPin
	if nilPin.retain() || nilPin.available() || nilPin.fixedDirectory() != nil {
		t.Fatal("nil directory pin granted authority")
	}
	if err := errors.Join(nilPin.Close(), nilPin.forceClose()); err != nil {
		t.Fatalf("close nil directory pin: %v", err)
	}

	platform, err := openOutputRuntimeTestPlatform(v3RecoveryRoot(t), false)
	if err != nil {
		t.Fatal(err)
	}
	defer platform.Close()
	duplicate, err := platform.Root().Duplicate()
	if err != nil {
		t.Fatal(err)
	}
	directory := &outputV3BoundaryCountingDirectory{Directory: duplicate}
	pin := newResumeStateDirectoryPin(directory)
	if pin == nil || !pin.retain() {
		t.Fatal("live directory pin could not be retained")
	}
	if err := pin.forceClose(); err != nil {
		t.Fatal(err)
	}
	if directory.closes.Load() != 1 || pin.available() || pin.fixedDirectory() != nil || pin.retain() {
		t.Fatalf("forced pin remained available: closes=%d available=%t fixed=%v",
			directory.closes.Load(), pin.available(), pin.fixedDirectory())
	}
	if err := errors.Join(pin.Close(), pin.forceClose()); err != nil || directory.closes.Load() != 1 {
		t.Fatalf("revoked pin closed twice: closes=%d error=%v", directory.closes.Load(), err)
	}
}

type outputV3BoundaryEmptyNamesDirectory struct{ outputcap.Directory }

func (*outputV3BoundaryEmptyNamesDirectory) Names(int) ([]string, error) { return nil, nil }

type outputV3BoundaryCountingDirectory struct {
	outputcap.Directory
	closes atomic.Int64
}

func (directory *outputV3BoundaryCountingDirectory) Close() error {
	directory.closes.Add(1)
	return directory.Directory.Close()
}

func runtimeBoundaryHasAttention(summary ResumeStateSummary, code string) bool {
	for _, attention := range summary.Attention {
		if attention.Code == code {
			return true
		}
	}
	return false
}

func assertOutputV3BoundaryFault(
	t *testing.T,
	err error,
	scope transfer.OutputFaultScope,
	code transfer.OutputFaultCode,
) {
	t.Helper()
	var fault *transfer.OutputFault
	if !errors.As(err, &fault) || fault.Scope() != scope || fault.Code() != code {
		t.Fatalf("output fault = %v, want scope=%v code=%v", err, scope, code)
	}
}
