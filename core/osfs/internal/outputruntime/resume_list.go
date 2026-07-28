package outputruntime

import (
	"context"
	"errors"
	"fmt"
	"slices"

	"github.com/windshare/windshare/core/osfs/internal/outputcap"
	"github.com/windshare/windshare/core/osfs/internal/outputfault"
	"github.com/windshare/windshare/core/osfs/internal/outputnamespace"
	"github.com/windshare/windshare/core/osfs/internal/resumestate"
	"github.com/windshare/windshare/core/transfer"
)

const (
	legacyOutputStatePrefix     = ".wsresume-output-"
	legacyOutputStagePrefix     = ".wsresume-output-stage-"
	legacyOutputJournalSuffix   = ".journal"
	maxLegacyOutputJournalBytes = 64 << 20
	resumeTreeEntryLimit        = resumestate.MaxFilesPerSession*4 +
		resumestate.MaxFileStateShardDirectories*3 + 6
	resumeDirectoryChildLimit = resumestate.MaxFileStateEntriesPerSession + 1
)

// resumePublicRootOperation keeps the canonical output-root placement fixed
// while a resume-management operation crosses public and private namespaces.
// A second guarded open at the end proves that the operation never fell back to
// the platform's long-lived primary root authority.
type resumePublicRootOperation struct {
	platform outputcap.Platform
	guard    outputcap.PublicOperationGuard
	root     outputcap.Directory
}

func acquireResumePublicRootOperation(
	platform outputcap.Platform,
) (*resumePublicRootOperation, error, error) {
	if platform == nil {
		return nil, transfer.ErrInvalidOutputBinding, nil
	}
	guard, err := acquireOutputPublicOperationGuard(platform)
	if err != nil {
		return nil, err, nil
	}
	root := guard.Root()
	if root == nil {
		return nil, errors.Join(outputcap.ErrUnsafeNamespace, errors.New("resume public-operation guard has no root authority")),
			guard.Close()
	}
	return &resumePublicRootOperation{platform: platform, guard: guard, root: root}, nil, nil
}

func (operation *resumePublicRootOperation) finish() (
	revalidationErr error,
	revalidationCloseErr error,
	guardCloseErr error,
) {
	if operation == nil || operation.platform == nil || operation.guard == nil || operation.root == nil {
		return errors.Join(outputcap.ErrUnsafeNamespace, errors.New("resume public-root operation is incomplete")), nil, nil
	}
	revalidationErr, revalidationCloseErr = operation.revalidateRoot()
	guardCloseErr = operation.guard.Close()
	operation.platform = nil
	operation.guard = nil
	operation.root = nil
	return revalidationErr, revalidationCloseErr, guardCloseErr
}

func (operation *resumePublicRootOperation) revalidateRoot() (error, error) {
	revalidated, err := acquireOutputPublicOperationGuard(operation.platform)
	if err != nil {
		return err, nil
	}
	root := revalidated.Root()
	if root == nil {
		closeErr := revalidated.Close()
		return errors.Join(outputcap.ErrUnsafeNamespace, errors.New("resume root revalidation guard has no root authority")), closeErr
	}
	same, compareErr := operation.root.SameDirectory(root)
	if compareErr == nil && same {
		return nil, revalidated.Close()
	}
	validationErr := errors.Join(
		outputcap.ErrUnsafeNamespace,
		errors.New("resume root changed during public operation"),
		compareErr,
	)
	return validationErr, revalidated.Close()
}

func resumeRootGuardCleanupFault(operation string, cause error) error {
	if cause == nil {
		return nil
	}
	return outputfault.New(
		transfer.OutputFaultRoot,
		transfer.OutputFaultStateIO,
		fmt.Errorf("%s: %w", operation, cause),
	)
}

func resumeRootRevalidationFault(operation string, cause error) error {
	if cause == nil {
		return nil
	}
	return outputnamespace.RootFault(operation, cause)
}

func finishResumePublicRootOperation(
	operation *resumePublicRootOperation,
	operationName string,
	resultErr *error,
) {
	if resultErr == nil {
		return
	}
	revalidationErr, revalidationCloseErr, guardCloseErr := operation.finish()
	*resultErr = errors.Join(
		*resultErr,
		resumeRootRevalidationFault("revalidate "+operationName+" root", revalidationErr),
		resumeRootGuardCleanupFault("close "+operationName+" revalidation guard", revalidationCloseErr),
		resumeRootGuardCleanupFault("close "+operationName+" retained guard", guardCloseErr),
	)
}

func (authority *Authority) ListResumeState(
	ctx context.Context,
	rootPath string,
) (resultInventory *ResumeStateInventory, resultErr error) {
	if authority == nil || authority.platformFactory == nil || rootPath == "" {
		return nil, transfer.ErrInvalidOutputBinding
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	platform, err := authority.platformFactory(rootPath, false)
	if err != nil {
		if !errors.Is(err, outputcap.ErrRecoverableOutputUnsupported) {
			return nil, outputnamespace.RootFault("certify resume root", err)
		}
		summaries, legacyErr := listLegacyResumeState(ctx, rootPath)
		if legacyErr != nil {
			return nil, outputnamespace.RootFault("inspect legacy resume state", legacyErr)
		}
		return newResumeStateInventory(summaries), nil
	}
	defer platform.Close()
	operation, acquireErr, acquireCloseErr := acquireResumePublicRootOperation(platform)
	if acquireErr != nil || acquireCloseErr != nil {
		return nil, errors.Join(
			resumeRootRevalidationFault("acquire resume listing root guard", acquireErr),
			resumeRootGuardCleanupFault("close unusable resume listing root guard", acquireCloseErr),
		)
	}
	defer func() {
		finishResumePublicRootOperation(operation, "resume listing", &resultErr)
		if resultErr == nil || resultInventory == nil {
			return
		}
		releaseErr := resultInventory.Close()
		resultInventory = nil
		resultErr = errors.Join(
			resultErr,
			resumeRootGuardCleanupFault("release failed resume listing inventory", releaseErr),
		)
	}()
	guardedRoot := operation.root
	summaries, err := listGuardedLegacyResumeState(ctx, rootPath, guardedRoot)
	if err != nil {
		return nil, outputnamespace.RootFault("inspect guarded legacy resume state", err)
	}
	defer func() {
		if resultErr != nil && resultInventory == nil {
			resultErr = errors.Join(resultErr, releaseResumeStateAuthorities(summaries))
		}
	}()
	if err := attachLegacyResumePins(guardedRoot, summaries); err != nil {
		return nil, outputnamespace.RootFault("pin legacy resume state", err)
	}
	summaries, err = authority.listResumeControlState(ctx, rootPath, platform, guardedRoot, summaries)
	if err != nil {
		return nil, err
	}
	resultInventory = newResumeStateInventory(summaries)
	return resultInventory, nil
}

func (authority *Authority) listResumeControlState(
	ctx context.Context,
	rootPath string,
	platform outputcap.Platform,
	root outputcap.Directory,
	summaries []ResumeStateSummary,
) ([]ResumeStateSummary, error) {
	controlKind, err := outputnamespace.ObserveExactEntry(root, resumestate.ControlDirectoryName)
	if err != nil {
		return summaries, outputnamespace.RootFault("observe global control namespace", err)
	}
	switch controlKind {
	case outputcap.EntryAbsent:
		candidates, candidateErr := root.NamesWithPrefix(
			resumestate.BootstrapCandidatePrefix, outputnamespace.RootInspectionLimit,
		)
		if candidateErr != nil || len(candidates) != 0 {
			return summaries, outputnamespace.RootFault(
				"classify uninstalled control candidate", errors.Join(candidateErr, outputfault.ErrRootUnsafe),
			)
		}
		return summaries, nil
	case outputcap.EntryDirectory:
		return authority.listInstalledResumeControl(ctx, rootPath, platform, root, summaries)
	default:
		return summaries, outputnamespace.RootFault("validate global control namespace", outputfault.ErrRootUnsafe)
	}
}

func (authority *Authority) listInstalledResumeControl(
	ctx context.Context,
	rootPath string,
	platform outputcap.Platform,
	root outputcap.Directory,
	summaries []ResumeStateSummary,
) ([]ResumeStateSummary, error) {
	control, err := authority.namespaceController().OpenInstalledControl(root, platform)
	if err != nil {
		return summaries, err
	}
	defer control.Close()
	coordinator, err := authority.acquireRuntimeNativeLock(
		func() (outputcap.Lock, bool, error) {
			return control.Directory().AcquireLock(resumestate.CoordinatorLockName, true)
		},
		filesystemOutputNativeLockContext{
			scope: FilesystemOutputNativeLockCoordinator, failureScope: transfer.OutputFaultRoot,
		},
		outputnamespace.RootFault("acquire listing coordinator lock", outputfault.ErrRootUnsafe),
	)
	if err != nil {
		return summaries, err
	}
	defer coordinator.Close()
	return authority.listResumeIntentNamespaces(ctx, rootPath, control, summaries)
}

func (authority *Authority) listResumeIntentNamespaces(
	ctx context.Context,
	rootPath string,
	control *outputnamespace.ControlNamespace,
	summaries []ResumeStateSummary,
) ([]ResumeStateSummary, error) {
	intentNames, err := control.Sessions().Names(outputnamespace.RootInspectionLimit)
	if err != nil {
		return summaries, outputnamespace.RootFault("inspect resume namespaces", err)
	}
	slices.Sort(intentNames)
	for _, intentName := range intentNames {
		if err := ctx.Err(); err != nil {
			return summaries, err
		}
		classified := resumestate.ClassifyResumeNamespaceName(intentName)
		if classified.Classification() != resumestate.ResumeNamespaceCanonical {
			summaries = append(summaries, unsafeIntentSummary(
				rootPath, control.Control().OutputRoot(), classified.Intent(), intentName, "unsafe-resume-namespace",
			))
			continue
		}
		summaries, err = authority.listCanonicalResumeIntent(
			rootPath, control, intentName, classified.Intent(), summaries,
		)
		if err != nil {
			return summaries, err
		}
	}
	return summaries, nil
}

func (authority *Authority) listCanonicalResumeIntent(
	rootPath string,
	control *outputnamespace.ControlNamespace,
	intentName string,
	intent transfer.ResumeIntent,
	summaries []ResumeStateSummary,
) ([]ResumeStateSummary, error) {
	unsafe := func(code string) ResumeStateSummary {
		return unsafeIntentSummary(rootPath, control.Control().OutputRoot(), intent, intentName, code)
	}
	intentDirectory, err := control.Sessions().OpenDirectory(intentName, true)
	if err != nil {
		return append(summaries, unsafe("unopenable-resume-namespace")), nil
	}
	sessionNames, err := intentDirectory.Names(resumestate.MaxSessionsPerIntent + 1)
	if err != nil {
		_ = intentDirectory.Close()
		return append(summaries, unsafe("uninspectable-resume-namespace")), nil
	}
	slices.Sort(sessionNames)
	if len(sessionNames) == 0 {
		_ = intentDirectory.Close()
		return append(summaries, unsafe("empty-resume-namespace")), nil
	}
	for _, sessionName := range sessionNames {
		summary, summaryErr := authority.listOneSession(
			rootPath, control, intentDirectory, intentName, intent, sessionName,
		)
		if summaryErr != nil {
			_ = intentDirectory.Close()
			return summaries, summaryErr
		}
		if len(sessionNames) != 1 {
			if summary.Reference.kind == ResumeStateRecoverable {
				summary.Reference.kind = ResumeStateNeedsAttention
			}
			summary.Attention = append(summary.Attention, ResumeAttention{
				Scope: ResumeAttentionIntent, Code: "multiple-sessions-for-intent", State: intentName,
			})
		}
		summaries = append(summaries, summary)
	}
	if err := intentDirectory.Close(); err != nil {
		summaries = append(summaries, unsafe("resume-namespace-close-failed"))
	}
	return summaries, nil
}

// resumeSessionListing keeps the names, live entry witness, and transferred pin
// in one object so every inspection phase classifies races in the same intent
// namespace instead of accidentally widening a corrupt session into a root fault.
type resumeSessionListing struct {
	authority                         *Authority
	rootPath, intentName, sessionName string
	control                           *outputnamespace.ControlNamespace
	intentDirectory, sessionDirectory outputcap.Directory
	intent                            transfer.ResumeIntent
	entry                             outputcap.CurrentEntryReference
	entryKind                         outputcap.EntryKind
	entryPin                          *resumeStateEntryPin
	sessionID                         transfer.OutputSessionID
	sessionLock                       outputcap.Lock
	lockPresent, lockUnsafe           bool
	reference                         ResumeStateRef
	transferredPin                    bool
}

func newResumeSessionListing(
	authority *Authority,
	rootPath string,
	control *outputnamespace.ControlNamespace,
	intentDirectory outputcap.Directory,
	intentName string,
	intent transfer.ResumeIntent,
	sessionName string,
) (*resumeSessionListing, error) {
	entry, err := intentDirectory.OpenEntry(sessionName)
	if err != nil {
		return nil, err
	}
	listing := &resumeSessionListing{
		authority: authority, rootPath: rootPath, control: control,
		intentDirectory: intentDirectory, intentName: intentName, intent: intent,
		sessionName: sessionName, entry: entry, entryPin: newResumeStateEntryPin(entry),
	}
	listing.entryKind = entry.Kind()
	return listing, nil
}

func (listing *resumeSessionListing) close() {
	if listing.sessionDirectory != nil {
		_ = listing.sessionDirectory.Close()
	}
	if !listing.transferredPin {
		_ = listing.entryPin.Close()
	}
}

func (listing *resumeSessionListing) intentFailure(code string) ResumeStateSummary {
	return unsafeIntentSummary(listing.rootPath, listing.control.Control().OutputRoot(), listing.intent, listing.intentName, code)
}

func (listing *resumeSessionListing) sessionFailure(code string) ResumeStateSummary {
	listing.transferredPin = true
	return unsafeSessionSummary(listing.rootPath, listing.control.Control().OutputRoot(), listing.intent, listing.intentName, listing.sessionName, listing.entryKind, listing.entryPin, code)
}

func (listing *resumeSessionListing) finish(summary ResumeStateSummary) ResumeStateSummary {
	listing.transferredPin = true
	return summary
}

func (listing *resumeSessionListing) verifyListedEntry() error {
	matches, err := listing.intentDirectory.EntryMatches(listing.sessionName, listing.entry)
	if err != nil || !matches {
		return errors.Join(outputfault.ErrIntentUnsafe, err)
	}
	return outputnamespace.VerifyPinnedDirectoryEntry(listing.control.Sessions(), listing.intentName, listing.intentDirectory)
}

func (listing *resumeSessionListing) verifyListedSession() error {
	if err := listing.verifyListedEntry(); err != nil {
		return err
	}
	return verifyPinnedOutputSession(listing.control.Sessions(), listing.intentDirectory, listing.sessionDirectory, listing.intentName, listing.sessionName)
}

func (listing *resumeSessionListing) openSessionDirectory() (ResumeStateSummary, bool) {
	if listing.entryKind == outputcap.EntryAbsent {
		return listing.intentFailure("unstable-session-entry"), true
	}
	sessionID, err := resumestate.ParseSessionDirectoryName(listing.sessionName)
	if err != nil || listing.entryKind != outputcap.EntryDirectory {
		return listing.failureAfterVerification("malformed-session-namespace", listing.verifyListedEntry), true
	}
	sessionDirectory, err := listing.intentDirectory.OpenPinnedDirectory(listing.entry, true)
	if err != nil {
		return listing.failureAfterVerification("unsafe-session-directory", listing.verifyListedEntry), true
	}
	listing.sessionID = sessionID
	listing.sessionDirectory = sessionDirectory
	listing.reference = ResumeStateRef{
		rootPath: listing.rootPath, root: listing.control.Control().OutputRoot(),
		intent: listing.intent, session: sessionID, kind: ResumeStateRecoverable,
		namespaceName: listing.intentName, sessionName: listing.sessionName,
		sessionKind: listing.entryKind, sessionPin: listing.entryPin,
	}
	return ResumeStateSummary{}, false
}

func (listing *resumeSessionListing) failureAfterVerification(code string, verify func() error) ResumeStateSummary {
	if err := verify(); err != nil {
		return listing.intentFailure("unstable-session-entry")
	}
	return listing.sessionFailure(code)
}

func (listing *resumeSessionListing) inspectLock() *ResumeStateSummary {
	lockKind, err := outputnamespace.ObserveExactEntry(
		listing.sessionDirectory, resumestate.SessionLockName,
	)
	if err != nil {
		summary := listing.failureAfterVerification("uninspectable-session-lock", listing.verifyListedSession)
		return &summary
	}
	listing.lockPresent = lockKind == outputcap.EntryRegularFile
	listing.lockUnsafe = lockKind == outputcap.EntryOther
	if !listing.lockPresent {
		return nil
	}
	return listing.acquireListingLock()
}

func (listing *resumeSessionListing) acquireListingLock() *ResumeStateSummary {
	lock, err := listing.acquireSessionLock("acquire listing session lock")
	if errors.Is(err, outputcap.ErrNamespaceLockBusy) {
		return listing.activeSessionLockInspection()
	}
	if err != nil {
		listing.lockPresent = false
		listing.lockUnsafe = true
		return nil
	}
	listing.sessionLock = lock
	return nil
}

func (listing *resumeSessionListing) activeSessionLockInspection() *ResumeStateSummary {
	if err := listing.verifyListedSession(); err != nil {
		summary := listing.intentFailure("unstable-session-entry")
		return &summary
	}
	summary := markResumeSessionAttention(
		ResumeStateSummary{Reference: listing.reference}, "session-active", listing.sessionName,
	)
	summary = listing.finish(summary)
	return &summary
}

func (listing *resumeSessionListing) acquireSessionLock(operation string) (outputcap.Lock, error) {
	return listing.authority.acquireRuntimeNativeLock(
		func() (outputcap.Lock, bool, error) {
			return listing.sessionDirectory.AcquireLock(resumestate.SessionLockName, true)
		},
		filesystemOutputNativeLockContext{
			resumeIntent: listing.intent, sessionID: listing.sessionID,
			scope: FilesystemOutputNativeLockSession, failureScope: transfer.OutputFaultSession,
		},
		intentOutputFault(operation, outputfault.ErrIntentUnsafe),
	)
}

func (listing *resumeSessionListing) previewSummary(lockUnsafe bool) (ResumeStateSummary, bool) {
	preview, err := previewPrivateTree(listing.sessionDirectory)
	if err != nil {
		return listing.failureAfterVerification("uninspectable-session-tree", listing.verifyListedSession), true
	}
	summary := ResumeStateSummary{
		Reference: listing.reference, FileRecords: preview.fileRecords, AllocatedBytes: preview.allocatedBytes,
		Attention: preview.attention,
	}
	if len(preview.attention) != 0 {
		summary.Reference.kind = ResumeStateNeedsAttention
	}
	if lockUnsafe {
		summary = markResumeSessionAttention(summary, "session-lock-unsafe", listing.sessionName)
	}
	return summary, false
}

func (listing *resumeSessionListing) inspectHeaderUpdates(
	summary ResumeStateSummary,
) (ResumeStateSummary, bool) {
	headerTemporaries, err := listing.sessionDirectory.NamesWithPrefix(
		resumestate.HeaderUpdateTemporaryPrefix, outputnamespace.AllocationAttempts+1,
	)
	if err != nil {
		return listing.failureAfterVerification("uninspectable-session-header-updates", listing.verifyListedSession), true
	}
	if len(headerTemporaries) == 0 {
		return summary, false
	}
	return markResumeSessionAttention(summary, "session-header-update-pending", listing.sessionName), false
}

func (listing *resumeSessionListing) readBoundHeader(
	summary ResumeStateSummary,
) (resumestate.Header, ResumeStateSummary, bool) {
	headerBytes, err := outputnamespace.ReadRecord(
		listing.sessionDirectory, resumestate.HeaderRecordName, resumestate.MaxSessionHeaderBytes,
	)
	if err != nil {
		return resumestate.Header{}, listing.finish(markResumeSessionAttention(
			summary, "session-header-unreadable", listing.sessionName,
		)), true
	}
	header, err := resumestate.DecodeHeader(headerBytes)
	if err != nil {
		return resumestate.Header{}, listing.finish(markResumeSessionAttention(
			summary, "session-header-corrupt", listing.sessionName,
		)), true
	}
	namespace, err := resumestate.BindSessionNamespaceAuthority(
		listing.control.Control(), header, listing.intentName, listing.sessionName,
	)
	if err != nil || namespace.Header().SessionID() != listing.sessionID || namespace.Header().ResumeIntent() != listing.intent {
		return resumestate.Header{}, listing.finish(markResumeSessionAttention(
			summary, "session-header-binding", listing.sessionName,
		)), true
	}
	return header, summary, false
}

func (listing *resumeSessionListing) inspectLifecycle(
	summary ResumeStateSummary,
	header resumestate.Header,
	lockPresent bool,
) ResumeStateSummary {
	lifecycle := header.Lifecycle()
	summary.Lifecycle = resumeSessionLifecycleFromState(lifecycle)
	terminal := lifecycle == resumestate.SessionCompleting || lifecycle == resumestate.SessionDiscarding
	switch {
	case terminal && lockPresent:
		return markResumeSessionAttention(summary, "terminal-transition-pending", lifecycle.String())
	case terminal:
		return listing.inspectLocklessTerminal(summary, header)
	case !lockPresent:
		return markResumeSessionAttention(summary, "session-lock-missing", listing.sessionName)
	default:
		return summary
	}
}

func (listing *resumeSessionListing) inspectLocklessTerminal(
	summary ResumeStateSummary,
	header resumestate.Header,
) ResumeStateSummary {
	layout, err := outputnamespace.InspectTerminalLayout(
		listing.sessionDirectory,
		header,
		func() (outputcap.Lock, error) {
			return listing.acquireSessionLock("acquire terminal listing session lock")
		},
	)
	if layout != nil {
		_ = layout.Close()
	}
	if err != nil {
		return markResumeSessionAttention(summary, "invalid-terminal-cut", header.Lifecycle().String())
	}
	return markResumeSessionAttention(summary, "terminal-transition-pending", header.Lifecycle().String())
}

func markResumeSessionAttention(summary ResumeStateSummary, code, state string) ResumeStateSummary {
	summary.Reference.kind = ResumeStateNeedsAttention
	summary.Attention = append(summary.Attention, ResumeAttention{Scope: ResumeAttentionIntent, Code: code, State: state})
	return summary
}

func (authority *Authority) listOneSession(
	rootPath string,
	control *outputnamespace.ControlNamespace,
	intentDirectory outputcap.Directory,
	intentName string,
	intent transfer.ResumeIntent,
	sessionName string,
) (ResumeStateSummary, error) {
	listing, err := newResumeSessionListing(
		authority, rootPath, control, intentDirectory, intentName, intent, sessionName,
	)
	if err != nil {
		return unsafeIntentSummary(
			rootPath, control.Control().OutputRoot(), intent, intentName, "uninspectable-session-entry",
		), nil
	}
	defer listing.close()
	if summary, done := listing.openSessionDirectory(); done {
		return summary, nil
	}
	if summary := listing.inspectLock(); summary != nil {
		return *summary, nil
	}
	if listing.sessionLock != nil {
		defer listing.sessionLock.Close()
	}
	if err := listing.verifyListedSession(); err != nil {
		return listing.intentFailure("unstable-session-entry"), nil
	}
	summary, done := listing.previewSummary(listing.lockUnsafe)
	if done {
		return summary, nil
	}
	summary, done = listing.inspectHeaderUpdates(summary)
	if done {
		return summary, nil
	}
	header, summary, done := listing.readBoundHeader(summary)
	if done {
		return summary, nil
	}
	summary = listing.inspectLifecycle(summary, header, listing.lockPresent)
	return listing.finish(summary), nil
}

var _ transfer.OutputAuthority = (*Authority)(nil)
