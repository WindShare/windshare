package outputruntime

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/windshare/windshare/core/osfs/internal/outputcap"
	"github.com/windshare/windshare/core/osfs/internal/outputfault"
	"github.com/windshare/windshare/core/osfs/internal/outputnamespace"
	"github.com/windshare/windshare/core/osfs/internal/resumestate"
	"github.com/windshare/windshare/core/transfer"
)

func TestOutputV3ListingIsolatesMalformedNamesAndKeepsTheirExactDiscardIdentity(t *testing.T) {
	t.Parallel()
	root := v3RecoveryRoot(t)
	authority := v3RecoveryAuthority(t, root, nil)
	selection := v3RecoverySelection(t, false, 0)
	opened := v3RecoveryOpen(t, authority, root, selection)
	v3RecoveryCloseSession(t, opened.Session)

	platform, err := openOutputRuntimeTestPlatform(root, false)
	if err != nil {
		t.Fatal(err)
	}
	control, err := authority.namespaceController().OpenInstalledControl(platform.Root(), platform)
	if err != nil {
		t.Fatal(err)
	}
	intentName := resumestate.ResumeNamespaceName(selection.ResumeIntent())
	sessions := control.Sessions()
	intent, err := sessions.OpenDirectory(intentName, true)
	if err != nil {
		t.Fatal(err)
	}
	malformedSessionName := "malformed-session-entry"
	malformed, err := intent.CreateFile(malformedSessionName, true, 1)
	if err != nil {
		t.Fatal(err)
	}
	if written, writeErr := malformed.WriteAt([]byte{0x41}, 0); writeErr != nil || written != 1 {
		t.Fatalf("write malformed session entry = (%d, %v)", written, writeErr)
	}
	opaqueIntentName := "malformed-intent-entry"
	opaqueIntent, err := sessions.CreateDirectory(opaqueIntentName, true)
	if err != nil {
		t.Fatal(err)
	}
	if err := errors.Join(
		malformed.Sync(), intent.Sync(), malformed.Close(), intent.Close(),
		opaqueIntent.Sync(), sessions.Sync(), opaqueIntent.Close(),
		control.Close(), platform.Close(),
	); err != nil {
		t.Fatal(err)
	}

	inventory, err := authority.ListResumeState(context.Background(), root)
	if err != nil {
		t.Fatal(err)
	}
	defer v3RecoveryCloseInventory(t, inventory)
	summaries := inventory.Summaries()
	if len(summaries) != 3 {
		t.Fatalf("isolated listing = %+v, want healthy, malformed session, and malformed intent", summaries)
	}
	var malformedSession, malformedIntent *ResumeStateSummary
	for index := range summaries {
		summary := &summaries[index]
		if runtimeListingHasAttention(*summary, "malformed-session-namespace") {
			malformedSession = summary
		}
		if runtimeListingHasAttention(*summary, "unsafe-resume-namespace") {
			malformedIntent = summary
		}
	}
	if malformedSession == nil || malformedSession.Reference.Kind() != ResumeStateOpaqueUnsafe {
		t.Fatalf("malformed session summary = %+v", malformedSession)
	}
	if malformedIntent == nil || malformedIntent.Reference.Kind() != ResumeStateOpaqueUnsafe {
		t.Fatalf("malformed intent summary = %+v", malformedIntent)
	}

	settlement, err := authority.DiscardResumeState(context.Background(), malformedSession.Reference)
	if err != nil || settlement.Kind != Discarded {
		t.Fatalf("discard exact malformed session = (%+v, %v)", settlement, err)
	}
	if _, err := authority.DiscardResumeState(context.Background(), malformedIntent.Reference); err == nil {
		t.Fatal("intent-only opaque listing granted destructive authority")
	}
	malformedPath := filepath.Join(
		root, resumestate.ControlDirectoryName, resumestate.SessionsDirectoryName,
		intentName, malformedSessionName,
	)
	if _, err := os.Stat(malformedPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("discarded malformed entry remains visible: %v", err)
	}

	reopened, err := v3OpenSelection(context.Background(), authority, selection)
	if err != nil {
		t.Fatalf("healthy sibling did not survive malformed discard: %v", err)
	}
	v3RecoveryCloseSession(t, reopened.Session)
}

func TestOutputV3ListingClassifiesRootAndIntentInspectionFaultsAtTheirScope(t *testing.T) {
	t.Parallel()
	root := v3RecoveryRoot(t)
	selection := v3RecoverySelection(t, false, 0)
	baseAuthority := v3RecoveryAuthority(t, root, nil)
	opened := v3RecoveryOpen(t, baseAuthority, root, selection)
	v3RecoveryCloseSession(t, opened.Session)

	controlPath := resumestate.ControlDirectoryName
	sessionsPath := runtimeListingControlChildPath(controlPath, resumestate.SessionsDirectoryName)
	intentName := resumestate.ResumeNamespaceName(selection.ResumeIntent())
	intentPath := runtimeListingControlChildPath(sessionsPath, intentName)
	for _, test := range []struct {
		name          string
		plan          *runtimeListingControlFaultPlan
		wantError     bool
		attentionCode string
	}{
		{
			name: "control-observation",
			plan: runtimeListingControlFailure(
				runtimeListingControlClassifyEntry, "", resumestate.ControlDirectoryName,
			),
			wantError: true,
		},
		{
			name: "coordinator-acquire",
			plan: runtimeListingControlFailure(
				runtimeListingControlAcquireLock, controlPath, resumestate.CoordinatorLockName,
			),
			wantError: true,
		},
		{
			name: "coordinator-recreated",
			plan: func() *runtimeListingControlFaultPlan {
				plan := runtimeListingControlFailure(
					runtimeListingControlAcquireLock, controlPath, resumestate.CoordinatorLockName,
				)
				plan.failure = nil
				plan.forceCreated = true
				return plan
			}(),
			wantError: true,
		},
		{
			name:      "resume-namespace-enumeration",
			plan:      runtimeListingControlFailure(runtimeListingControlNames, sessionsPath, ""),
			wantError: true,
		},
		{
			name:          "intent-open",
			plan:          runtimeListingControlFailure(runtimeListingControlOpenDirectory, sessionsPath, intentName),
			attentionCode: "unopenable-resume-namespace",
		},
		{
			name:          "intent-enumeration",
			plan:          runtimeListingControlFailure(runtimeListingControlNames, intentPath, ""),
			attentionCode: "uninspectable-resume-namespace",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			authority := v3RecoveryAuthority(t, root, nil)
			authority.platformFactory = runtimeListingControlFaultFactory(test.plan)
			inventory, err := authority.ListResumeState(context.Background(), root)
			test.plan.requireFired(t)
			if (err != nil) != test.wantError {
				if inventory != nil {
					_ = inventory.Close()
				}
				t.Fatalf("listing fault = (%v, %v), want error=%t", inventory, err, test.wantError)
			}
			if test.wantError {
				return
			}
			defer v3RecoveryCloseInventory(t, inventory)
			summaries := inventory.Summaries()
			if len(summaries) != 1 || !runtimeListingHasAttention(summaries[0], test.attentionCode) {
				t.Fatalf("intent-scoped listing fault = %+v, want %q", summaries, test.attentionCode)
			}
		})
	}

	t.Run("platform-certification", func(t *testing.T) {
		authority := v3RecoveryAuthority(t, root, nil)
		authority.platformFactory = func(string, bool) (outputcap.Platform, error) {
			return nil, errStateTerminalInjected
		}
		if inventory, err := authority.ListResumeState(context.Background(), root); err == nil || inventory != nil {
			t.Fatalf("platform certification fault = (%v, %v)", inventory, err)
		}
	})

	t.Run("canceled-between-root-and-intent", func(t *testing.T) {
		ctx := newV3RecoveryCancelAfterErrCalls(2)
		if inventory, err := baseAuthority.ListResumeState(ctx, root); !errors.Is(err, context.Canceled) || inventory != nil {
			t.Fatalf("intent-loop cancellation = (%v, %v)", inventory, err)
		}
	})
}

func TestOutputV3ReferenceAuthorityRejectsStructurallyIncompleteCapabilities(t *testing.T) {
	t.Parallel()
	if (ResumeStateRef{rootPath: "root"}).validAuthority() {
		t.Fatal("untyped reference was accepted")
	}
	if (ResumeStateRef{rootPath: "root", kind: ResumeStateOpaqueUnsafe}).validAuthority() {
		t.Fatal("opaque reference without root and namespace authority was accepted")
	}
	if err := releaseResumeStateAuthorities([]ResumeStateSummary{{}}); err != nil {
		t.Fatalf("release empty authority: %v", err)
	}
}

func TestOutputV3SingleSessionListingConvertsObjectRacesIntoIntentAttention(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name          string
		plan          *stateListingIdentityFaultPlan
		attentionCode string
	}{
		{
			name: "session-entry-open",
			plan: &stateListingIdentityFaultPlan{
				operation: stateListingOpenEntry, err: errStateTerminalInjected,
			},
			attentionCode: "uninspectable-session-entry",
		},
		{
			name: "session-entry-disappeared",
			plan: &stateListingIdentityFaultPlan{
				operation: stateListingEntryKind, kind: outputcap.EntryAbsent,
			},
			attentionCode: "unstable-session-entry",
		},
		{
			name: "session-directory-open",
			plan: &stateListingIdentityFaultPlan{
				operation: stateListingOpenPinned, err: errStateTerminalInjected,
			},
			attentionCode: "unsafe-session-directory",
		},
		{
			name: "session-directory-open-and-entry-rebound",
			plan: &stateListingIdentityFaultPlan{
				operation: stateListingOpenPinnedThenMismatch, err: errStateTerminalInjected,
			},
			attentionCode: "unstable-session-entry",
		},
		{
			name: "session-lock-observation",
			plan: &stateListingIdentityFaultPlan{
				operation: stateListingClassifyLock, err: errStateTerminalInjected,
			},
			attentionCode: "uninspectable-session-lock",
		},
		{
			name: "session-lock-busy",
			plan: &stateListingIdentityFaultPlan{
				operation: stateListingAcquireLock, err: outputcap.ErrNamespaceLockBusy,
			},
			attentionCode: "session-active",
		},
		{
			name: "session-lock-acquire-failed",
			plan: &stateListingIdentityFaultPlan{
				operation: stateListingAcquireLock, err: errStateTerminalInjected,
			},
			attentionCode: "session-lock-unsafe",
		},
		{
			name: "session-entry-rebound-before-preview",
			plan: &stateListingIdentityFaultPlan{
				operation: stateListingEntryMismatch,
			},
			attentionCode: "unstable-session-entry",
		},
		{
			name: "session-tree-enumeration",
			plan: &stateListingIdentityFaultPlan{
				operation: stateListingNames, err: errStateTerminalInjected,
			},
			attentionCode: "uninspectable-session-tree",
		},
		{
			name: "header-update-enumeration",
			plan: &stateListingIdentityFaultPlan{
				operation: stateListingNamesWithPrefix, err: errStateTerminalInjected,
			},
			attentionCode: "uninspectable-session-header-updates",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			root := v3RecoveryRoot(t)
			authority := v3RecoveryAuthority(t, root, nil)
			selection := v3RecoverySelection(t, false, 0)
			opened := v3RecoveryOpen(t, authority, root, selection)
			sessionID := opened.Session.SessionID()
			v3RecoveryCloseSession(t, opened.Session)

			intentName := resumestate.ResumeNamespaceName(selection.ResumeIntent())
			sessionName := resumestate.SessionDirectoryName(sessionID)
			test.plan.intentName = intentName
			test.plan.sessionName = sessionName
			nativeFactory := authority.platformFactory
			authority.platformFactory = func(path string, create bool) (outputcap.Platform, error) {
				platform, err := nativeFactory(path, create)
				if err != nil {
					return nil, err
				}
				return &stateListingIdentityPlatform{Platform: platform, plan: test.plan}, nil
			}
			inventory, err := authority.ListResumeState(context.Background(), root)
			if err != nil {
				t.Fatalf("list one session: %v", err)
			}
			defer v3RecoveryCloseInventory(t, inventory)
			summaries := inventory.Summaries()
			if len(summaries) != 1 || !runtimeListingHasAttention(summaries[0], test.attentionCode) {
				t.Fatalf("listing attention = %+v, want %q", summaries, test.attentionCode)
			}
			if test.plan.fired == 0 {
				t.Fatalf("fault %q did not fire", test.plan.operation)
			}
		})
	}
}

func TestOutputV3LegacyListingRejectsUnboundedOrUnpinnableState(t *testing.T) {
	t.Parallel()
	if _, _, err := digestLegacyOutputJournal(&stateListingErrorReader{}); !errors.Is(err, errStateTerminalInjected) {
		t.Fatalf("legacy digest read error = %v", err)
	}
	if _, _, err := digestLegacyOutputJournal(
		io.LimitReader(&stateListingZeroReader{}, maxLegacyOutputJournalBytes+1),
	); !errors.Is(err, outputfault.ErrLegacyState) {
		t.Fatalf("oversized legacy digest error = %v", err)
	}

	root := v3RecoveryRoot(t)
	stageName := legacyOutputStagePrefix + "manual"
	journalDirectoryName := legacyOutputStatePrefix + "unsafe" + legacyOutputJournalSuffix
	if err := writeStateListingLegacyFixtures(root, stageName, journalDirectoryName); err != nil {
		t.Fatal(err)
	}
	summaries, err := listLegacyResumeState(context.Background(), root)
	if err != nil {
		t.Fatal(err)
	}
	if len(summaries) != 2 || !runtimeListingHasAttention(summaries[0], "legacy-v2-stage-manual") ||
		!runtimeListingHasAttention(summaries[1], "legacy-v2-journal-unsafe") {
		t.Fatalf("legacy unsafe listing = %+v", summaries)
	}

	platform, err := openOutputRuntimeTestPlatform(root, false)
	if err != nil {
		t.Fatal(err)
	}
	defer platform.Close()
	removable := []ResumeStateSummary{{
		Reference: ResumeStateRef{
			kind: ResumeStateLegacyUntrusted, legacyName: "legacy", legacyRemovable: true,
		},
	}}
	wrapped := &stateListingRootPlatform{
		Platform: platform,
		root: &stateListingDuplicateDirectory{
			Directory: platform.Root(), duplicateErr: errStateTerminalInjected,
		},
	}
	if err := attachLegacyResumePins(wrapped.Root(), removable); err != nil {
		t.Fatal(err)
	}
	if removable[0].Reference.legacyRemovable ||
		!runtimeListingHasAttention(removable[0], "legacy-v2-root-pin-unavailable") {
		t.Fatalf("unpinnable legacy summary = %+v", removable[0])
	}

	different := []ResumeStateSummary{{
		Reference: ResumeStateRef{
			kind: ResumeStateLegacyUntrusted, legacyName: "legacy", legacyRemovable: true,
		},
	}}
	differentRoot := &stateListingRootPlatform{
		Platform: platform,
		root: &stateListingDuplicateDirectory{
			Directory: platform.Root(), forceDifferent: true,
		},
	}
	if err := attachLegacyResumePins(differentRoot.Root(), different); err != nil {
		t.Fatal(err)
	}
	if different[0].Reference.legacyRemovable ||
		!runtimeListingHasAttention(different[0], "legacy-v2-root-pin-unavailable") {
		t.Fatalf("differently bound legacy root = %+v", different[0])
	}
}

func TestOutputV3ListingRecognizesAValidLocklessTerminalSuffix(t *testing.T) {
	t.Parallel()
	fixture := newTerminalRecoveryFaultFixture(t, resumestate.SessionCompleting)
	root := fixture.session.owner.rootPath
	authority := fixture.session.owner
	fixture.sessionDirectory.syncErrAt = 4
	err := outputnamespace.RecoverTerminalNamespace(
		fixture.control, fixture.intentDirectory, fixture.sessionDirectory,
		fixture.header, fixture.layout, false,
	)
	if !errors.Is(err, errTerminalRecoveryInjected) {
		fixture.close(t)
		t.Fatalf("construct lockless terminal suffix: %v", err)
	}
	if err := fixture.layout.Close(); err != nil {
		fixture.close(t)
		t.Fatal(err)
	}
	fixture.layout = &outputnamespace.TerminalLayout{}
	if err := fixture.session.closeHandles(); err != nil {
		fixture.close(t)
		t.Fatal(err)
	}
	defer fixture.close(t)

	inventory, err := authority.ListResumeState(context.Background(), root)
	if err != nil {
		t.Fatal(err)
	}
	defer v3RecoveryCloseInventory(t, inventory)
	summaries := inventory.Summaries()
	if len(summaries) != 1 || summaries[0].Lifecycle != ResumeSessionCompleting ||
		!runtimeListingHasAttention(summaries[0], "terminal-transition-pending") ||
		runtimeListingHasAttention(summaries[0], "invalid-terminal-cut") {
		t.Fatalf("lockless terminal listing = %+v", summaries)
	}
}

func TestOutputV3LegacyDiscardRequiresLiveRootAndEntryPins(t *testing.T) {
	t.Parallel()
	if _, err := discardLegacyState(nil, ResumeStateRef{
		kind: ResumeStateLegacyUntrusted, legacyName: "legacy.journal", legacyRemovable: true,
	}); !errors.Is(err, transfer.ErrInvalidOutputBinding) {
		// The exact public classification matters; the nil root capability must be
		// rejected before any platform dereference.
		t.Fatalf("legacy discard without root pin = %v", err)
	}

	root := v3RecoveryRoot(t)
	platform, err := openOutputRuntimeTestPlatform(root, false)
	if err != nil {
		t.Fatal(err)
	}
	defer platform.Close()
	duplicate, err := platform.Root().Duplicate()
	if err != nil {
		t.Fatal(err)
	}
	rootPin := newResumeStateDirectoryPin(duplicate)
	defer rootPin.Close()
	reference := ResumeStateRef{
		kind: ResumeStateLegacyUntrusted, legacyName: "legacy.journal", legacyRemovable: true,
		legacyRoot: rootPin,
	}
	if _, err := discardLegacyState(platform.Root(), reference); !errors.Is(err, transfer.ErrInvalidOutputBinding) {
		t.Fatalf("legacy discard without entry pin = %v", err)
	}

	foreignRoot := v3RecoveryRoot(t)
	foreign, err := openOutputRuntimeTestPlatform(foreignRoot, false)
	if err != nil {
		t.Fatal(err)
	}
	defer foreign.Close()
	if _, err := discardLegacyState(foreign.Root(), reference); err == nil || !errors.Is(err, outputfault.ErrRootUnsafe) {
		t.Fatalf("legacy discard with foreign root = %v", err)
	}
}

func runtimeListingHasAttention(summary ResumeStateSummary, code string) bool {
	for _, attention := range summary.Attention {
		if attention.Code == code {
			return true
		}
	}
	return false
}

const (
	runtimeListingControlClassifyEntry = "classify-entry"
	runtimeListingControlAcquireLock   = "acquire-lock"
	runtimeListingControlNames         = "names"
	runtimeListingControlOpenDirectory = "open-directory"
)

type runtimeListingControlFaultPlan struct {
	mu           sync.Mutex
	operation    string
	path         string
	name         string
	failure      error
	forceCreated bool
	fired        int
}

func runtimeListingControlFailure(operation, path, name string) *runtimeListingControlFaultPlan {
	return &runtimeListingControlFaultPlan{
		operation: operation, path: path, name: name, failure: errStateTerminalInjected,
	}
}

func (plan *runtimeListingControlFaultPlan) trigger(operation, path, name string) (bool, error) {
	if plan == nil || operation != plan.operation || path != plan.path || name != plan.name {
		return false, nil
	}
	plan.mu.Lock()
	defer plan.mu.Unlock()
	plan.fired++
	return true, plan.failure
}

func (plan *runtimeListingControlFaultPlan) requireFired(t *testing.T) {
	t.Helper()
	plan.mu.Lock()
	defer plan.mu.Unlock()
	if plan.fired != 1 {
		t.Fatalf("fault %s path=%q name=%q fired %d times, want once", plan.operation, plan.path, plan.name, plan.fired)
	}
}

type runtimeListingControlFaultPlatform struct {
	outputcap.Platform
	plan *runtimeListingControlFaultPlan
}

func runtimeListingControlFaultFactory(plan *runtimeListingControlFaultPlan) PlatformFactory {
	return func(path string, create bool) (outputcap.Platform, error) {
		platform, err := openOutputRuntimeTestPlatform(path, create)
		if err != nil {
			return nil, err
		}
		return &runtimeListingControlFaultPlatform{Platform: platform, plan: plan}, nil
	}
}

func (platform *runtimeListingControlFaultPlatform) Root() outputcap.Directory {
	if platform == nil || platform.Platform == nil {
		return nil
	}
	return wrapRuntimeListingControlDirectory(platform.Platform.Root(), "", platform.plan)
}

func (platform *runtimeListingControlFaultPlatform) AcquirePublicOperationGuard() (
	outputcap.PublicOperationGuard,
	error,
) {
	if platform == nil || platform.Platform == nil {
		return nil, transfer.ErrInvalidOutputBinding
	}
	return acquireRuntimeTestDecoratedPublicOperationGuard(
		platform.Platform,
		func(root outputcap.Directory) outputcap.Directory {
			return wrapRuntimeListingControlDirectory(root, "", platform.plan)
		},
	)
}

type runtimeListingControlDirectory struct {
	outputcap.Directory
	path string
	plan *runtimeListingControlFaultPlan
}

func wrapRuntimeListingControlDirectory(
	directory outputcap.Directory,
	path string,
	plan *runtimeListingControlFaultPlan,
) outputcap.Directory {
	if directory == nil {
		return nil
	}
	return &runtimeListingControlDirectory{Directory: directory, path: path, plan: plan}
}

func unwrapRuntimeListingControlDirectory(directory outputcap.Directory) outputcap.Directory {
	if wrapped, ok := directory.(*runtimeListingControlDirectory); ok {
		return wrapped.Directory
	}
	return directory
}

func runtimeListingControlChildPath(parent, name string) string {
	if parent == "" {
		return name
	}
	return parent + "/" + name
}

func (directory *runtimeListingControlDirectory) Duplicate() (outputcap.Directory, error) {
	duplicate, err := directory.Directory.Duplicate()
	if err != nil {
		return nil, err
	}
	return wrapRuntimeListingControlDirectory(duplicate, directory.path, directory.plan), nil
}

func (directory *runtimeListingControlDirectory) SameDirectory(other outputcap.Directory) (bool, error) {
	return directory.Directory.SameDirectory(unwrapRuntimeListingControlDirectory(other))
}

func (directory *runtimeListingControlDirectory) ClassifyExactEntry(
	name string,
) (outputcap.EntryKind, bool, error) {
	if matched, err := directory.plan.trigger(runtimeListingControlClassifyEntry, directory.path, name); matched {
		return outputcap.EntryAbsent, false, err
	}
	return directory.Directory.ClassifyExactEntry(name)
}

func (directory *runtimeListingControlDirectory) Names(limit int) ([]string, error) {
	if matched, err := directory.plan.trigger(runtimeListingControlNames, directory.path, ""); matched {
		return nil, err
	}
	return directory.Directory.Names(limit)
}

func (directory *runtimeListingControlDirectory) OpenDirectory(
	name string,
	private bool,
) (outputcap.Directory, error) {
	if matched, err := directory.plan.trigger(runtimeListingControlOpenDirectory, directory.path, name); matched {
		return nil, err
	}
	opened, err := directory.Directory.OpenDirectory(name, private)
	if err != nil {
		return nil, err
	}
	return wrapRuntimeListingControlDirectory(
		opened, runtimeListingControlChildPath(directory.path, name), directory.plan,
	), nil
}

func (directory *runtimeListingControlDirectory) AcquireLock(
	name string,
	existingOnly bool,
) (outputcap.Lock, bool, error) {
	if matched, err := directory.plan.trigger(runtimeListingControlAcquireLock, directory.path, name); matched {
		if err != nil || !directory.plan.forceCreated {
			return nil, false, err
		}
		lock, _, lockErr := directory.Directory.AcquireLock(name, existingOnly)
		return lock, true, lockErr
	}
	return directory.Directory.AcquireLock(name, existingOnly)
}

const (
	stateListingSessionsPath           = "sessions"
	stateListingOpenEntry              = "open-entry"
	stateListingEntryKind              = "entry-kind"
	stateListingOpenPinned             = "open-pinned"
	stateListingOpenPinnedThenMismatch = "open-pinned-then-mismatch"
	stateListingClassifyLock           = "classify-lock"
	stateListingAcquireLock            = "acquire-lock"
	stateListingEntryMismatch          = "entry-mismatch"
	stateListingNames                  = "names"
	stateListingNamesWithPrefix        = "names-with-prefix"
)

type stateListingIdentityFaultPlan struct {
	operation   string
	err         error
	kind        outputcap.EntryKind
	intentName  string
	sessionName string
	fired       int
}

func (plan *stateListingIdentityFaultPlan) sessionPath() string {
	return filepath.Join(stateListingSessionsPath, plan.intentName, plan.sessionName)
}

// stateListingIdentityPlatform inserts race observations before outputnamespace
// owns its control handles. Keeping the fault at the capability edge exercises
// the production construction path without reaching into private namespace state.
type stateListingIdentityPlatform struct {
	outputcap.Platform
	plan *stateListingIdentityFaultPlan
}

func (platform *stateListingIdentityPlatform) Root() outputcap.Directory {
	if platform == nil || platform.Platform == nil {
		return nil
	}
	return wrapStateListingIdentityDirectory(platform.Platform.Root(), platform.plan, "")
}

func (platform *stateListingIdentityPlatform) AcquirePublicOperationGuard() (
	outputcap.PublicOperationGuard,
	error,
) {
	if platform == nil || platform.Platform == nil {
		return nil, transfer.ErrInvalidOutputBinding
	}
	return acquireRuntimeTestDecoratedPublicOperationGuard(
		platform.Platform,
		func(root outputcap.Directory) outputcap.Directory {
			return wrapStateListingIdentityDirectory(root, platform.plan, "")
		},
	)
}

type stateListingIdentityDirectory struct {
	outputcap.Directory
	plan *stateListingIdentityFaultPlan
	path string
}

func wrapStateListingIdentityDirectory(
	directory outputcap.Directory,
	plan *stateListingIdentityFaultPlan,
	path string,
) outputcap.Directory {
	if directory == nil {
		return nil
	}
	return &stateListingIdentityDirectory{Directory: directory, plan: plan, path: path}
}

func unwrapStateListingIdentityDirectory(directory outputcap.Directory) outputcap.Directory {
	for {
		wrapped, ok := directory.(*stateListingIdentityDirectory)
		if !ok {
			return directory
		}
		directory = wrapped.Directory
	}
}

func unwrapStateListingIdentityEntry(entry outputcap.CurrentEntryReference) outputcap.CurrentEntryReference {
	if wrapped, ok := entry.(*stateListingIdentityEntry); ok {
		return wrapped.CurrentEntryReference
	}
	return entry
}

func stateListingIdentityChildPath(parent, name string) string {
	// The fault plan scopes session observations below the fixed control
	// namespace. Normalizing that structural wrapper keeps each injected cut
	// focused on the listing boundary it is meant to classify.
	if parent == "" && name == resumestate.ControlDirectoryName {
		return ""
	}
	if parent == "" {
		return name
	}
	return filepath.Join(parent, name)
}

func (directory *stateListingIdentityDirectory) OpenDirectory(
	name string,
	private bool,
) (outputcap.Directory, error) {
	opened, err := directory.Directory.OpenDirectory(name, private)
	return wrapStateListingIdentityDirectory(
		opened, directory.plan, stateListingIdentityChildPath(directory.path, name),
	), err
}

func (directory *stateListingIdentityDirectory) SameDirectory(other outputcap.Directory) (bool, error) {
	return unwrapStateListingIdentityDirectory(directory.Directory).SameDirectory(
		unwrapStateListingIdentityDirectory(other),
	)
}

func (directory *stateListingIdentityDirectory) OpenEntry(name string) (outputcap.CurrentEntryReference, error) {
	if directory.path == filepath.Join(stateListingSessionsPath, directory.plan.intentName) &&
		name == directory.plan.sessionName && directory.plan.operation == stateListingOpenEntry {
		directory.plan.fired++
		return nil, directory.plan.err
	}
	entry, err := directory.Directory.OpenEntry(name)
	if err != nil {
		return nil, err
	}
	return &stateListingIdentityEntry{
		CurrentEntryReference: entry, plan: directory.plan, path: directory.path, name: name,
	}, nil
}

func (directory *stateListingIdentityDirectory) EntryMatches(
	name string,
	expected outputcap.CurrentEntryReference,
) (bool, error) {
	if directory.path == filepath.Join(stateListingSessionsPath, directory.plan.intentName) &&
		name == directory.plan.sessionName &&
		(directory.plan.operation == stateListingEntryMismatch ||
			directory.plan.operation == stateListingOpenPinnedThenMismatch) {
		directory.plan.fired++
		return false, nil
	}
	return directory.Directory.EntryMatches(name, unwrapStateListingIdentityEntry(expected))
}

func (directory *stateListingIdentityDirectory) OpenPinnedDirectory(
	expected outputcap.CurrentEntryReference,
	private bool,
) (outputcap.Directory, error) {
	entry, _ := expected.(*stateListingIdentityEntry)
	if entry != nil && directory.path == filepath.Join(stateListingSessionsPath, directory.plan.intentName) &&
		entry.name == directory.plan.sessionName &&
		(directory.plan.operation == stateListingOpenPinned ||
			directory.plan.operation == stateListingOpenPinnedThenMismatch) {
		directory.plan.fired++
		return nil, directory.plan.err
	}
	opened, err := directory.Directory.OpenPinnedDirectory(
		unwrapStateListingIdentityEntry(expected), private,
	)
	name := "pinned"
	if entry != nil {
		name = entry.name
	}
	return wrapStateListingIdentityDirectory(
		opened, directory.plan, stateListingIdentityChildPath(directory.path, name),
	), err
}

func (directory *stateListingIdentityDirectory) ClassifyExactEntry(
	name string,
) (outputcap.EntryKind, bool, error) {
	if directory.path == directory.plan.sessionPath() && name == resumestate.SessionLockName &&
		directory.plan.operation == stateListingClassifyLock {
		directory.plan.fired++
		return outputcap.EntryAbsent, false, directory.plan.err
	}
	return directory.Directory.ClassifyExactEntry(name)
}

func (directory *stateListingIdentityDirectory) AcquireLock(
	name string,
	existingOnly bool,
) (outputcap.Lock, bool, error) {
	if directory.path == directory.plan.sessionPath() && name == resumestate.SessionLockName &&
		directory.plan.operation == stateListingAcquireLock {
		directory.plan.fired++
		return nil, false, directory.plan.err
	}
	return directory.Directory.AcquireLock(name, existingOnly)
}

func (directory *stateListingIdentityDirectory) Names(limit int) ([]string, error) {
	if directory.path == directory.plan.sessionPath() && directory.plan.operation == stateListingNames {
		directory.plan.fired++
		return nil, directory.plan.err
	}
	return directory.Directory.Names(limit)
}

func (directory *stateListingIdentityDirectory) NamesWithPrefix(
	prefix string,
	limit int,
) ([]string, error) {
	if directory.path == directory.plan.sessionPath() &&
		prefix == resumestate.HeaderUpdateTemporaryPrefix &&
		directory.plan.operation == stateListingNamesWithPrefix {
		directory.plan.fired++
		return nil, directory.plan.err
	}
	return directory.Directory.NamesWithPrefix(prefix, limit)
}

type stateListingIdentityEntry struct {
	outputcap.CurrentEntryReference
	plan *stateListingIdentityFaultPlan
	path string
	name string
}

func (entry *stateListingIdentityEntry) Kind() outputcap.EntryKind {
	if entry.path == filepath.Join(stateListingSessionsPath, entry.plan.intentName) &&
		entry.name == entry.plan.sessionName && entry.plan.operation == stateListingEntryKind {
		entry.plan.fired++
		return entry.plan.kind
	}
	return entry.CurrentEntryReference.Kind()
}

type stateListingErrorReader struct{}

func (*stateListingErrorReader) Read([]byte) (int, error) { return 0, errStateTerminalInjected }

type stateListingZeroReader struct{}

func (*stateListingZeroReader) Read(target []byte) (int, error) {
	clear(target)
	return len(target), nil
}

func writeStateListingLegacyFixtures(root, stageName, journalDirectoryName string) error {
	if err := os.WriteFile(filepath.Join(root, stageName), []byte("stage"), 0o600); err != nil {
		return err
	}
	// Legacy discovery must classify directory journal names as manual state; it
	// never follows or interprets them.
	return os.Mkdir(filepath.Join(root, journalDirectoryName), 0o700)
}

type stateListingRootPlatform struct {
	outputcap.Platform
	root outputcap.Directory
}

func (platform *stateListingRootPlatform) Root() outputcap.Directory { return platform.root }

func (platform *stateListingRootPlatform) AcquirePublicOperationGuard() (
	outputcap.PublicOperationGuard,
	error,
) {
	decorated := platform.root.(*stateListingDuplicateDirectory)
	return acquireRuntimeTestDecoratedPublicOperationGuard(
		platform.Platform,
		func(root outputcap.Directory) outputcap.Directory {
			return &stateListingDuplicateDirectory{
				Directory:      root,
				duplicateErr:   decorated.duplicateErr,
				forceDifferent: decorated.forceDifferent,
			}
		},
	)
}

type stateListingDuplicateDirectory struct {
	outputcap.Directory
	duplicateErr   error
	forceDifferent bool
}

func (directory *stateListingDuplicateDirectory) Duplicate() (outputcap.Directory, error) {
	if directory.duplicateErr != nil {
		return nil, directory.duplicateErr
	}
	duplicate, err := directory.Directory.Duplicate()
	if err != nil {
		return nil, err
	}
	return &stateListingDuplicateDirectory{
		Directory: duplicate, forceDifferent: directory.forceDifferent,
	}, nil
}

func (directory *stateListingDuplicateDirectory) SameDirectory(other outputcap.Directory) (bool, error) {
	if directory.forceDifferent {
		return false, nil
	}
	if wrapped, ok := other.(*stateListingDuplicateDirectory); ok {
		other = wrapped.Directory
	}
	return directory.Directory.SameDirectory(other)
}
