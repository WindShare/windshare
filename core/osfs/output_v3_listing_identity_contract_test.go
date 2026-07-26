package osfs

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/windshare/windshare/core/osfs/internal/resumestate"
	"github.com/windshare/windshare/core/transfer"
)

func TestOutputV3ListingIsolatesMalformedNamesAndKeepsTheirExactDiscardIdentity(t *testing.T) {
	root := v3RecoveryRoot(t)
	authority := v3RecoveryAuthority(t, root, nil)
	selection := v3RecoverySelection(t, false, 0)
	opened := v3RecoveryOpen(t, authority, root, selection)
	v3RecoveryCloseSession(t, opened.Session)

	platform, err := openOutputV3Platform(root, false)
	if err != nil {
		t.Fatal(err)
	}
	control, err := openInstalledControl(platform.Root(), platform)
	if err != nil {
		t.Fatal(err)
	}
	intentName := resumestate.ResumeNamespaceName(selection.ResumeIntent())
	intent, err := control.sessions.OpenDirectory(intentName, true)
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
	opaqueIntent, err := control.sessions.CreateDirectory(opaqueIntentName, true)
	if err != nil {
		t.Fatal(err)
	}
	if err := errors.Join(
		malformed.Sync(), intent.Sync(), malformed.Close(), intent.Close(),
		opaqueIntent.Sync(), control.sessions.Sync(), opaqueIntent.Close(),
		control.Close(), platform.Close(),
	); err != nil {
		t.Fatal(err)
	}

	inventory, err := authority.listResumeState(
		context.Background(), FilesystemResumeRoot{RootPath: root},
	)
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
		if v3RecoveryHasAttention(*summary, "malformed-session-namespace") {
			malformedSession = summary
		}
		if v3RecoveryHasAttention(*summary, "unsafe-resume-namespace") {
			malformedIntent = summary
		}
	}
	if malformedSession == nil || malformedSession.Reference.Kind() != ResumeStateOpaqueUnsafe {
		t.Fatalf("malformed session summary = %+v", malformedSession)
	}
	if malformedIntent == nil || malformedIntent.Reference.Kind() != ResumeStateOpaqueUnsafe {
		t.Fatalf("malformed intent summary = %+v", malformedIntent)
	}

	settlement, err := authority.discardResumeState(context.Background(), malformedSession.Reference)
	if err != nil || settlement.Kind != Discarded {
		t.Fatalf("discard exact malformed session = (%+v, %v)", settlement, err)
	}
	if _, err := authority.discardResumeState(context.Background(), malformedIntent.Reference); err == nil {
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
	root := v3RecoveryRoot(t)
	selection := v3RecoverySelection(t, false, 0)
	baseAuthority := v3RecoveryAuthority(t, root, nil)
	opened := v3RecoveryOpen(t, baseAuthority, root, selection)
	v3RecoveryCloseSession(t, opened.Session)

	controlPath := resumestate.ControlDirectoryName
	sessionsPath := outputV3ControlSessionChildPath(controlPath, resumestate.SessionsDirectoryName)
	intentName := resumestate.ResumeNamespaceName(selection.ResumeIntent())
	intentPath := outputV3ControlSessionChildPath(sessionsPath, intentName)
	for _, test := range []struct {
		name          string
		plan          *outputV3ControlSessionFaultPlan
		wantError     bool
		attentionCode string
	}{
		{
			name: "control-observation",
			plan: outputV3ControlSessionFailure(
				outputV3CSClassifyEntry, "", resumestate.ControlDirectoryName,
			),
			wantError: true,
		},
		{
			name: "coordinator-acquire",
			plan: outputV3ControlSessionFailure(
				outputV3CSAcquireLock, controlPath, resumestate.CoordinatorLockName,
			),
			wantError: true,
		},
		{
			name: "coordinator-recreated",
			plan: func() *outputV3ControlSessionFaultPlan {
				plan := outputV3ControlSessionFailure(
					outputV3CSAcquireLock, controlPath, resumestate.CoordinatorLockName,
				)
				plan.failure = nil
				plan.forceCreated = true
				return plan
			}(),
			wantError: true,
		},
		{
			name:      "resume-namespace-enumeration",
			plan:      outputV3ControlSessionFailure(outputV3CSNames, sessionsPath, ""),
			wantError: true,
		},
		{
			name:          "intent-open",
			plan:          outputV3ControlSessionFailure(outputV3CSOpenDirectory, sessionsPath, intentName),
			attentionCode: "unopenable-resume-namespace",
		},
		{
			name:          "intent-enumeration",
			plan:          outputV3ControlSessionFailure(outputV3CSNames, intentPath, ""),
			attentionCode: "uninspectable-resume-namespace",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			authority := v3RecoveryAuthority(t, root, nil)
			authority.platformFactory = outputV3ControlSessionFaultFactory(test.plan)
			inventory, err := authority.listResumeState(
				context.Background(), FilesystemResumeRoot{RootPath: root},
			)
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
			if len(summaries) != 1 || !v3RecoveryHasAttention(summaries[0], test.attentionCode) {
				t.Fatalf("intent-scoped listing fault = %+v, want %q", summaries, test.attentionCode)
			}
		})
	}

	t.Run("platform-certification", func(t *testing.T) {
		authority := v3RecoveryAuthority(t, root, nil)
		authority.platformFactory = func(string, bool) (outputV3Platform, error) {
			return nil, errStateTerminalInjected
		}
		if inventory, err := authority.listResumeState(
			context.Background(), FilesystemResumeRoot{RootPath: root},
		); err == nil || inventory != nil {
			t.Fatalf("platform certification fault = (%v, %v)", inventory, err)
		}
	})

	t.Run("canceled-between-root-and-intent", func(t *testing.T) {
		ctx := newV3RecoveryCancelAfterErrCalls(2)
		if inventory, err := baseAuthority.listResumeState(
			ctx, FilesystemResumeRoot{RootPath: root},
		); !errors.Is(err, context.Canceled) || inventory != nil {
			t.Fatalf("intent-loop cancellation = (%v, %v)", inventory, err)
		}
	})
}

func TestOutputV3ReferenceAuthorityRejectsStructurallyIncompleteCapabilities(t *testing.T) {
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
				operation: stateListingEntryKind, kind: outputV3EntryAbsent,
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
				operation: stateListingAcquireLock, err: errOutputV3LockBusy,
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

			platform, err := openOutputV3Platform(root, false)
			if err != nil {
				t.Fatal(err)
			}
			control, err := openInstalledControl(platform.Root(), platform)
			if err != nil {
				t.Fatal(err)
			}
			intentName := resumestate.ResumeNamespaceName(selection.ResumeIntent())
			sessionName := resumestate.SessionDirectoryName(sessionID)
			test.plan.intentName = intentName
			test.plan.sessionName = sessionName
			sessions := wrapStateListingIdentityDirectory(
				control.sessions, test.plan, stateListingSessionsPath,
			)
			intent, err := sessions.OpenDirectory(intentName, true)
			if err != nil {
				t.Fatal(err)
			}
			listedControl := *control
			listedControl.sessions = sessions
			summary, summaryErr := authority.listOneSession(
				root, &listedControl, intent, intentName, selection.ResumeIntent(), sessionName,
			)
			if summaryErr != nil {
				t.Fatalf("list one session: %v", summaryErr)
			}
			if !v3RecoveryHasAttention(summary, test.attentionCode) {
				t.Fatalf("listing attention = %+v, want %q", summary, test.attentionCode)
			}
			if test.plan.fired == 0 {
				t.Fatalf("fault %q did not fire", test.plan.operation)
			}
			if err := errors.Join(intent.Close(), control.Close(), platform.Close()); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestOutputV3LegacyListingRejectsUnboundedOrUnpinnableState(t *testing.T) {
	if _, _, err := digestLegacyOutputJournal(&stateListingErrorReader{}); !errors.Is(err, errStateTerminalInjected) {
		t.Fatalf("legacy digest read error = %v", err)
	}
	if _, _, err := digestLegacyOutputJournal(
		io.LimitReader(&stateListingZeroReader{}, maxLegacyOutputJournalBytes+1),
	); !errors.Is(err, errLegacyOutputState) {
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
	if len(summaries) != 2 || !v3RecoveryHasAttention(summaries[0], "legacy-v2-stage-manual") ||
		!v3RecoveryHasAttention(summaries[1], "legacy-v2-journal-unsafe") {
		t.Fatalf("legacy unsafe listing = %+v", summaries)
	}

	platform, err := openOutputV3Platform(root, false)
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
		outputV3Platform: platform,
		root: &stateListingDuplicateDirectory{
			outputV3Directory: platform.Root(), duplicateErr: errStateTerminalInjected,
		},
	}
	if err := attachLegacyResumePins(wrapped.Root(), removable); err != nil {
		t.Fatal(err)
	}
	if removable[0].Reference.legacyRemovable ||
		!v3RecoveryHasAttention(removable[0], "legacy-v2-root-pin-unavailable") {
		t.Fatalf("unpinnable legacy summary = %+v", removable[0])
	}

	different := []ResumeStateSummary{{
		Reference: ResumeStateRef{
			kind: ResumeStateLegacyUntrusted, legacyName: "legacy", legacyRemovable: true,
		},
	}}
	differentRoot := &stateListingRootPlatform{
		outputV3Platform: platform,
		root: &stateListingDuplicateDirectory{
			outputV3Directory: platform.Root(), forceDifferent: true,
		},
	}
	if err := attachLegacyResumePins(differentRoot.Root(), different); err != nil {
		t.Fatal(err)
	}
	if different[0].Reference.legacyRemovable ||
		!v3RecoveryHasAttention(different[0], "legacy-v2-root-pin-unavailable") {
		t.Fatalf("differently bound legacy root = %+v", different[0])
	}
}

func TestOutputV3ListingRecognizesAValidLocklessTerminalSuffix(t *testing.T) {
	fixture := newTerminalRecoveryFaultFixture(t, resumestate.SessionCompleting)
	root := fixture.session.owner.rootPath
	authority := fixture.session.owner
	fixture.sessionDirectory.syncErrAt = 4
	err := recoverTerminalNamespace(
		fixture.control, fixture.intentDirectory, fixture.sessionDirectory,
		fixture.header, fixture.layout, false,
	)
	if !errors.Is(err, errTerminalRecoveryInjected) {
		fixture.close(t)
		t.Fatalf("construct lockless terminal suffix: %v", err)
	}
	if err := fixture.layout.close(); err != nil {
		fixture.close(t)
		t.Fatal(err)
	}
	fixture.layout = &outputTerminalLayout{}
	if err := fixture.session.closeHandles(); err != nil {
		fixture.close(t)
		t.Fatal(err)
	}
	defer fixture.close(t)

	inventory, err := authority.listResumeState(
		context.Background(), FilesystemResumeRoot{RootPath: root},
	)
	if err != nil {
		t.Fatal(err)
	}
	defer v3RecoveryCloseInventory(t, inventory)
	summaries := inventory.Summaries()
	if len(summaries) != 1 || summaries[0].Lifecycle != ResumeSessionCompleting ||
		!v3RecoveryHasAttention(summaries[0], "terminal-transition-pending") ||
		v3RecoveryHasAttention(summaries[0], "invalid-terminal-cut") {
		t.Fatalf("lockless terminal listing = %+v", summaries)
	}
}

func TestOutputV3LegacyDiscardRequiresLiveRootAndEntryPins(t *testing.T) {
	if _, err := discardLegacyState(nil, ResumeStateRef{
		kind: ResumeStateLegacyUntrusted, legacyName: "legacy.journal", legacyRemovable: true,
	}); !errors.Is(err, transfer.ErrInvalidOutputBinding) {
		// The exact public classification matters; the nil root capability must be
		// rejected before any platform dereference.
		t.Fatalf("legacy discard without root pin = %v", err)
	}

	root := v3RecoveryRoot(t)
	platform, err := openOutputV3Platform(root, false)
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
	foreign, err := openOutputV3Platform(foreignRoot, false)
	if err != nil {
		t.Fatal(err)
	}
	defer foreign.Close()
	if _, err := discardLegacyState(foreign.Root(), reference); err == nil || !errors.Is(err, errOutputRootUnsafe) {
		t.Fatalf("legacy discard with foreign root = %v", err)
	}
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
	kind        outputV3EntryKind
	intentName  string
	sessionName string
	fired       int
}

func (plan *stateListingIdentityFaultPlan) sessionPath() string {
	return filepath.Join(stateListingSessionsPath, plan.intentName, plan.sessionName)
}

type stateListingIdentityDirectory struct {
	outputV3Directory
	plan *stateListingIdentityFaultPlan
	path string
}

func wrapStateListingIdentityDirectory(
	directory outputV3Directory,
	plan *stateListingIdentityFaultPlan,
	path string,
) outputV3Directory {
	if directory == nil {
		return nil
	}
	return &stateListingIdentityDirectory{outputV3Directory: directory, plan: plan, path: path}
}

func unwrapStateListingIdentityDirectory(directory outputV3Directory) outputV3Directory {
	for {
		wrapped, ok := directory.(*stateListingIdentityDirectory)
		if !ok {
			return directory
		}
		directory = wrapped.outputV3Directory
	}
}

func unwrapStateListingIdentityEntry(entry outputV3EntryRef) outputV3EntryRef {
	if wrapped, ok := entry.(*stateListingIdentityEntry); ok {
		return wrapped.outputV3EntryRef
	}
	return entry
}

func (directory *stateListingIdentityDirectory) OpenDirectory(
	name string,
	private bool,
) (outputV3Directory, error) {
	opened, err := directory.outputV3Directory.OpenDirectory(name, private)
	return wrapStateListingIdentityDirectory(opened, directory.plan, filepath.Join(directory.path, name)), err
}

func (directory *stateListingIdentityDirectory) SameDirectory(other outputV3Directory) (bool, error) {
	return unwrapStateListingIdentityDirectory(directory.outputV3Directory).SameDirectory(
		unwrapStateListingIdentityDirectory(other),
	)
}

func (directory *stateListingIdentityDirectory) OpenEntry(name string) (outputV3EntryRef, error) {
	if directory.path == filepath.Join(stateListingSessionsPath, directory.plan.intentName) &&
		name == directory.plan.sessionName && directory.plan.operation == stateListingOpenEntry {
		directory.plan.fired++
		return nil, directory.plan.err
	}
	entry, err := directory.outputV3Directory.OpenEntry(name)
	if err != nil {
		return nil, err
	}
	return &stateListingIdentityEntry{
		outputV3EntryRef: entry, plan: directory.plan, path: directory.path, name: name,
	}, nil
}

func (directory *stateListingIdentityDirectory) EntryMatches(
	name string,
	expected outputV3EntryRef,
) (bool, error) {
	if directory.path == filepath.Join(stateListingSessionsPath, directory.plan.intentName) &&
		name == directory.plan.sessionName &&
		(directory.plan.operation == stateListingEntryMismatch ||
			directory.plan.operation == stateListingOpenPinnedThenMismatch) {
		directory.plan.fired++
		return false, nil
	}
	return directory.outputV3Directory.EntryMatches(name, unwrapStateListingIdentityEntry(expected))
}

func (directory *stateListingIdentityDirectory) OpenPinnedDirectory(
	expected outputV3EntryRef,
	private bool,
) (outputV3Directory, error) {
	entry, _ := expected.(*stateListingIdentityEntry)
	if entry != nil && directory.path == filepath.Join(stateListingSessionsPath, directory.plan.intentName) &&
		entry.name == directory.plan.sessionName &&
		(directory.plan.operation == stateListingOpenPinned ||
			directory.plan.operation == stateListingOpenPinnedThenMismatch) {
		directory.plan.fired++
		return nil, directory.plan.err
	}
	opened, err := directory.outputV3Directory.OpenPinnedDirectory(
		unwrapStateListingIdentityEntry(expected), private,
	)
	name := "pinned"
	if entry != nil {
		name = entry.name
	}
	return wrapStateListingIdentityDirectory(opened, directory.plan, filepath.Join(directory.path, name)), err
}

func (directory *stateListingIdentityDirectory) ClassifyExactEntry(
	name string,
) (outputV3EntryKind, bool, error) {
	if directory.path == directory.plan.sessionPath() && name == resumestate.SessionLockName &&
		directory.plan.operation == stateListingClassifyLock {
		directory.plan.fired++
		return outputV3EntryAbsent, false, directory.plan.err
	}
	return directory.outputV3Directory.ClassifyExactEntry(name)
}

func (directory *stateListingIdentityDirectory) AcquireLock(
	name string,
	existingOnly bool,
) (outputV3Lock, bool, error) {
	if directory.path == directory.plan.sessionPath() && name == resumestate.SessionLockName &&
		directory.plan.operation == stateListingAcquireLock {
		directory.plan.fired++
		return nil, false, directory.plan.err
	}
	return directory.outputV3Directory.AcquireLock(name, existingOnly)
}

func (directory *stateListingIdentityDirectory) Names(limit int) ([]string, error) {
	if directory.path == directory.plan.sessionPath() && directory.plan.operation == stateListingNames {
		directory.plan.fired++
		return nil, directory.plan.err
	}
	return directory.outputV3Directory.Names(limit)
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
	return directory.outputV3Directory.NamesWithPrefix(prefix, limit)
}

type stateListingIdentityEntry struct {
	outputV3EntryRef
	plan *stateListingIdentityFaultPlan
	path string
	name string
}

func (entry *stateListingIdentityEntry) Kind() outputV3EntryKind {
	if entry.path == filepath.Join(stateListingSessionsPath, entry.plan.intentName) &&
		entry.name == entry.plan.sessionName && entry.plan.operation == stateListingEntryKind {
		entry.plan.fired++
		return entry.plan.kind
	}
	return entry.outputV3EntryRef.Kind()
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
	outputV3Platform
	root outputV3Directory
}

func (platform *stateListingRootPlatform) Root() outputV3Directory { return platform.root }

func (platform *stateListingRootPlatform) AcquirePublicOperationGuard() (
	outputV3PublicOperationGuard,
	error,
) {
	decorated := platform.root.(*stateListingDuplicateDirectory)
	return acquireOutputV3DecoratedPublicOperationGuard(
		platform.outputV3Platform,
		func(root outputV3Directory) outputV3Directory {
			return &stateListingDuplicateDirectory{
				outputV3Directory: root,
				duplicateErr:      decorated.duplicateErr,
				forceDifferent:    decorated.forceDifferent,
			}
		},
	)
}

type stateListingDuplicateDirectory struct {
	outputV3Directory
	duplicateErr   error
	forceDifferent bool
}

func (directory *stateListingDuplicateDirectory) Duplicate() (outputV3Directory, error) {
	if directory.duplicateErr != nil {
		return nil, directory.duplicateErr
	}
	duplicate, err := directory.outputV3Directory.Duplicate()
	if err != nil {
		return nil, err
	}
	return &stateListingDuplicateDirectory{
		outputV3Directory: duplicate, forceDifferent: directory.forceDifferent,
	}, nil
}

func (directory *stateListingDuplicateDirectory) SameDirectory(other outputV3Directory) (bool, error) {
	if directory.forceDifferent {
		return false, nil
	}
	if wrapped, ok := other.(*stateListingDuplicateDirectory); ok {
		other = wrapped.outputV3Directory
	}
	return directory.outputV3Directory.SameDirectory(other)
}
