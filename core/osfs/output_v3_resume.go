package osfs

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"math"
	"os"
	"slices"
	"strings"

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
	platform outputV3Platform
	guard    outputV3PublicOperationGuard
	root     outputV3Directory
}

func acquireResumePublicRootOperation(
	platform outputV3Platform,
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
		return nil, errors.Join(errOutputV3Unsafe, errors.New("resume public-operation guard has no root authority")),
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
		return errors.Join(errOutputV3Unsafe, errors.New("resume public-root operation is incomplete")), nil, nil
	}

	revalidated, err := acquireOutputPublicOperationGuard(operation.platform)
	if err != nil {
		revalidationErr = err
	} else {
		revalidatedRoot := revalidated.Root()
		if revalidatedRoot == nil {
			revalidationErr = errors.Join(errOutputV3Unsafe, errors.New("resume root revalidation guard has no root authority"))
		} else {
			same, compareErr := operation.root.SameDirectory(revalidatedRoot)
			if compareErr != nil || !same {
				revalidationErr = errors.Join(
					errOutputV3Unsafe,
					errors.New("resume root changed during public operation"),
					compareErr,
				)
			}
		}
		revalidationCloseErr = revalidated.Close()
	}

	guardCloseErr = operation.guard.Close()
	operation.platform = nil
	operation.guard = nil
	operation.root = nil
	return revalidationErr, revalidationCloseErr, guardCloseErr
}

func resumeRootGuardCleanupFault(operation string, cause error) error {
	if cause == nil {
		return nil
	}
	return outputFault(
		transfer.OutputFaultRoot,
		transfer.OutputFaultStateIO,
		fmt.Errorf("%s: %w", operation, cause),
	)
}

func resumeRootRevalidationFault(operation string, cause error) error {
	if cause == nil {
		return nil
	}
	return rootOutputFault(operation, cause)
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

func (authority *FilesystemOutputAuthority) listResumeState(
	ctx context.Context,
	root FilesystemResumeRoot,
) (resultInventory *ResumeStateInventory, resultErr error) {
	if authority == nil || authority.platformFactory == nil || root.RootPath == "" {
		return nil, transfer.ErrInvalidOutputBinding
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	platform, err := authority.platformFactory(root.RootPath, false)
	if err != nil {
		if errors.Is(err, errOutputV3Unsupported) {
			summaries, legacyErr := listLegacyResumeState(ctx, root.RootPath)
			if legacyErr != nil {
				return nil, rootOutputFault("inspect legacy resume state", legacyErr)
			}
			return newResumeStateInventory(summaries), nil
		}
		return nil, rootOutputFault("certify resume root", err)
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
		if resultErr != nil && resultInventory != nil {
			releaseErr := resultInventory.Close()
			resultInventory = nil
			resultErr = errors.Join(
				resultErr,
				resumeRootGuardCleanupFault("release failed resume listing inventory", releaseErr),
			)
		}
	}()
	guardedRoot := operation.root
	summaries, err := listGuardedLegacyResumeState(ctx, root.RootPath, guardedRoot)
	if err != nil {
		return nil, rootOutputFault("inspect guarded legacy resume state", err)
	}
	defer func() {
		if resultErr != nil && resultInventory == nil {
			resultErr = errors.Join(resultErr, releaseResumeStateAuthorities(summaries))
		}
	}()
	if err := attachLegacyResumePins(guardedRoot, summaries); err != nil {
		return nil, rootOutputFault("pin legacy resume state", err)
	}
	controlKind, err := observeExactOutputEntry(guardedRoot, resumestate.ControlDirectoryName)
	if err != nil {
		return nil, rootOutputFault("observe global control namespace", err)
	}
	if controlKind == outputV3EntryAbsent {
		candidates, candidateErr := guardedRoot.NamesWithPrefix(
			resumestate.BootstrapCandidatePrefix, outputRootInspectionLimit,
		)
		if candidateErr != nil || len(candidates) != 0 {
			return nil, rootOutputFault(
				"classify uninstalled control candidate", errors.Join(candidateErr, errOutputRootUnsafe),
			)
		}
		resultInventory = newResumeStateInventory(summaries)
		return resultInventory, nil
	}
	if controlKind != outputV3EntryDirectory {
		return nil, rootOutputFault("validate global control namespace", errOutputRootUnsafe)
	}
	control, err := openInstalledControl(guardedRoot, platform)
	if err != nil {
		return nil, err
	}
	defer control.Close()
	coordinator, err := authority.acquireRuntimeNativeLock(
		func() (outputV3Lock, bool, error) {
			return control.directory.AcquireLock(resumestate.CoordinatorLockName, true)
		},
		filesystemOutputNativeLockContext{
			scope: FilesystemOutputNativeLockCoordinator, failureScope: transfer.OutputFaultRoot,
		},
		rootOutputFault("acquire listing coordinator lock", errOutputRootUnsafe),
	)
	if err != nil {
		return nil, err
	}
	defer coordinator.Close()

	intentNames, err := control.sessions.Names(outputRootInspectionLimit)
	if err != nil {
		return nil, rootOutputFault("inspect resume namespaces", err)
	}
	slices.Sort(intentNames)
	for _, intentName := range intentNames {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		classified := resumestate.ClassifyResumeNamespaceName(intentName)
		if classified.Classification() != resumestate.ResumeNamespaceCanonical {
			summaries = append(summaries, ResumeStateSummary{
				Reference: ResumeStateRef{
					rootPath: root.RootPath, root: control.control.OutputRoot(), kind: ResumeStateOpaqueUnsafe,
					namespaceName: intentName, intent: classified.Intent(),
				},
				Attention: []ResumeAttention{{
					Scope: ResumeAttentionIntent, Code: "unsafe-resume-namespace", State: intentName,
				}},
			})
			continue
		}
		intentDir, err := control.sessions.OpenDirectory(intentName, true)
		if err != nil {
			summaries = append(summaries, unsafeIntentSummary(
				root.RootPath, control.control.OutputRoot(), classified.Intent(), intentName,
				"unopenable-resume-namespace",
			))
			continue
		}
		sessionNames, listErr := intentDir.Names(resumestate.MaxSessionsPerIntent + 1)
		if listErr != nil {
			_ = intentDir.Close()
			summaries = append(summaries, unsafeIntentSummary(
				root.RootPath, control.control.OutputRoot(), classified.Intent(), intentName,
				"uninspectable-resume-namespace",
			))
			continue
		}
		slices.Sort(sessionNames)
		if len(sessionNames) == 0 {
			summaries = append(summaries, unsafeIntentSummary(
				root.RootPath, control.control.OutputRoot(), classified.Intent(), intentName, "empty-resume-namespace",
			))
			_ = intentDir.Close()
			continue
		}
		for _, sessionName := range sessionNames {
			summary, summaryErr := authority.listOneSession(
				root.RootPath, control, intentDir, intentName, classified.Intent(), sessionName,
			)
			if summaryErr != nil {
				_ = intentDir.Close()
				return nil, summaryErr
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
		if err := intentDir.Close(); err != nil {
			summaries = append(summaries, unsafeIntentSummary(
				root.RootPath, control.control.OutputRoot(), classified.Intent(), intentName,
				"resume-namespace-close-failed",
			))
		}
	}
	resultInventory = newResumeStateInventory(summaries)
	return resultInventory, nil
}

func (authority *FilesystemOutputAuthority) listOneSession(
	rootPath string,
	control *outputControlNamespace,
	intentDir outputV3Directory,
	intentName string,
	intent transfer.ResumeIntent,
	sessionName string,
) (ResumeStateSummary, error) {
	intentFailure := func(code string) (ResumeStateSummary, error) {
		return unsafeIntentSummary(
			rootPath, control.control.OutputRoot(), intent, intentName, code,
		), nil
	}
	entry, err := intentDir.OpenEntry(sessionName)
	if err != nil {
		return intentFailure("uninspectable-session-entry")
	}
	pin := newResumeStateEntryPin(entry)
	transferredPin := false
	defer func() {
		if !transferredPin {
			_ = pin.Close()
		}
	}()
	kind := entry.Kind()
	if kind == outputV3EntryAbsent {
		return intentFailure("unstable-session-entry")
	}
	pinnedFailure := func(code string) (ResumeStateSummary, error) {
		transferredPin = true
		return unsafeSessionSummary(
			rootPath, control.control.OutputRoot(), intent, intentName, sessionName,
			kind, pin, code,
		), nil
	}
	verifyListedEntry := func() error {
		matches, err := intentDir.EntryMatches(sessionName, entry)
		if err != nil || !matches {
			return errors.Join(errOutputIntentUnsafe, err)
		}
		return verifyPinnedDirectoryEntry(control.sessions, intentName, intentDir)
	}
	sessionID, parseErr := resumestate.ParseSessionDirectoryName(sessionName)
	if parseErr != nil || kind != outputV3EntryDirectory {
		if err := verifyListedEntry(); err != nil {
			return intentFailure("unstable-session-entry")
		}
		return pinnedFailure("malformed-session-namespace")
	}
	sessionDir, err := intentDir.OpenPinnedDirectory(entry, true)
	if err != nil {
		if verifyErr := verifyListedEntry(); verifyErr != nil {
			return intentFailure("unstable-session-entry")
		}
		return pinnedFailure("unsafe-session-directory")
	}
	defer sessionDir.Close()
	reference := ResumeStateRef{
		rootPath: rootPath, root: control.control.OutputRoot(), intent: intent, session: sessionID,
		kind: ResumeStateRecoverable, namespaceName: intentName, sessionName: sessionName,
		sessionKind: kind, sessionPin: pin,
	}
	finish := func(summary ResumeStateSummary) (ResumeStateSummary, error) {
		transferredPin = true
		return summary, nil
	}
	verifyListedSession := func() error {
		if err := verifyListedEntry(); err != nil {
			return err
		}
		return verifyPinnedOutputSession(
			control.sessions, intentDir, sessionDir, intentName, sessionName,
		)
	}
	var lock outputV3Lock
	lockKind, err := observeExactOutputEntry(sessionDir, resumestate.SessionLockName)
	if err != nil {
		if verifyErr := verifyListedSession(); verifyErr != nil {
			return intentFailure("unstable-session-entry")
		}
		return pinnedFailure("uninspectable-session-lock")
	}
	lockPresent := lockKind == outputV3EntryRegularFile
	lockUnsafe := lockKind == outputV3EntryOther
	if lockPresent {
		lock, err = authority.acquireRuntimeNativeLock(
			func() (outputV3Lock, bool, error) {
				return sessionDir.AcquireLock(resumestate.SessionLockName, true)
			},
			filesystemOutputNativeLockContext{
				resumeIntent: intent, sessionID: sessionID,
				scope: FilesystemOutputNativeLockSession, failureScope: transfer.OutputFaultSession,
			},
			intentOutputFault("acquire listing session lock", errOutputIntentUnsafe),
		)
		if errors.Is(err, errOutputV3LockBusy) {
			if verifyErr := verifyListedSession(); verifyErr != nil {
				return intentFailure("unstable-session-entry")
			}
			reference.kind = ResumeStateNeedsAttention
			return finish(ResumeStateSummary{
				Reference: reference,
				Attention: []ResumeAttention{{
					Scope: ResumeAttentionIntent, Code: "session-active", State: sessionName,
				}},
			})
		}
		if err != nil {
			lock = nil
			lockPresent = false
			lockUnsafe = true
		} else {
			defer lock.Close()
		}
	}
	if err := verifyListedSession(); err != nil {
		return intentFailure("unstable-session-entry")
	}
	preview, err := previewPrivateTree(sessionDir)
	if err != nil {
		if verifyErr := verifyListedSession(); verifyErr != nil {
			return intentFailure("unstable-session-entry")
		}
		return pinnedFailure("uninspectable-session-tree")
	}
	summary := ResumeStateSummary{
		Reference: reference, FileRecords: preview.fileRecords, AllocatedBytes: preview.allocatedBytes,
		Attention: preview.attention,
	}
	if len(preview.attention) != 0 {
		summary.Reference.kind = ResumeStateNeedsAttention
	}
	if lockUnsafe {
		summary.Reference.kind = ResumeStateNeedsAttention
		summary.Attention = append(summary.Attention, ResumeAttention{
			Scope: ResumeAttentionIntent, Code: "session-lock-unsafe", State: sessionName,
		})
	}
	headerTemporaries, err := sessionDir.NamesWithPrefix(
		resumestate.HeaderUpdateTemporaryPrefix, outputStateAllocationAttempts+1,
	)
	if err != nil {
		if verifyErr := verifyListedSession(); verifyErr != nil {
			return intentFailure("unstable-session-entry")
		}
		return pinnedFailure("uninspectable-session-header-updates")
	}
	if len(headerTemporaries) != 0 {
		summary.Reference.kind = ResumeStateNeedsAttention
		summary.Attention = append(summary.Attention, ResumeAttention{
			Scope: ResumeAttentionIntent, Code: "session-header-update-pending", State: sessionName,
		})
	}
	headerBytes, err := readStateRecord(sessionDir, resumestate.HeaderRecordName, resumestate.MaxSessionHeaderBytes)
	if err != nil {
		summary.Reference.kind = ResumeStateNeedsAttention
		summary.Attention = append(summary.Attention, ResumeAttention{
			Scope: ResumeAttentionIntent, Code: "session-header-unreadable", State: sessionName,
		})
		return finish(summary)
	}
	header, err := resumestate.DecodeHeader(headerBytes)
	if err != nil {
		summary.Reference.kind = ResumeStateNeedsAttention
		summary.Attention = append(summary.Attention, ResumeAttention{
			Scope: ResumeAttentionIntent, Code: "session-header-corrupt", State: sessionName,
		})
		return finish(summary)
	}
	namespace, err := resumestate.BindSessionNamespaceAuthority(control.control, header, intentName, sessionName)
	if err != nil || namespace.Header().SessionID() != sessionID || namespace.Header().ResumeIntent() != intent {
		summary.Reference.kind = ResumeStateNeedsAttention
		summary.Attention = append(summary.Attention, ResumeAttention{
			Scope: ResumeAttentionIntent, Code: "session-header-binding", State: sessionName,
		})
		return finish(summary)
	}
	summary.Lifecycle = resumeSessionLifecycleFromState(header.Lifecycle())
	if header.Lifecycle() == resumestate.SessionCompleting || header.Lifecycle() == resumestate.SessionDiscarding {
		if !lockPresent {
			layout, layoutErr := inspectTerminalSessionLayoutWithLock(
				sessionDir,
				header,
				func() (outputV3Lock, error) {
					return authority.acquireRuntimeNativeLock(
						func() (outputV3Lock, bool, error) {
							return sessionDir.AcquireLock(resumestate.SessionLockName, true)
						},
						filesystemOutputNativeLockContext{
							resumeIntent: intent, sessionID: sessionID,
							scope: FilesystemOutputNativeLockSession, failureScope: transfer.OutputFaultSession,
						},
						intentOutputFault("acquire terminal listing session lock", errOutputIntentUnsafe),
					)
				},
			)
			if layout != nil {
				_ = layout.close()
			}
			if layoutErr != nil {
				summary.Reference.kind = ResumeStateNeedsAttention
				summary.Attention = append(summary.Attention, ResumeAttention{
					Scope: ResumeAttentionIntent, Code: "invalid-terminal-cut", State: header.Lifecycle().String(),
				})
				return finish(summary)
			}
		}
		summary.Reference.kind = ResumeStateNeedsAttention
		summary.Attention = append(summary.Attention, ResumeAttention{
			Scope: ResumeAttentionIntent, Code: "terminal-transition-pending", State: header.Lifecycle().String(),
		})
	} else if !lockPresent {
		summary.Reference.kind = ResumeStateNeedsAttention
		summary.Attention = append(summary.Attention, ResumeAttention{
			Scope: ResumeAttentionIntent, Code: "session-lock-missing", State: sessionName,
		})
	}
	return finish(summary)
}

type privateTreePreview struct {
	allocatedBytes uint64
	fileRecords    uint64
	entries        int
	attention      []ResumeAttention
}

func previewPrivateTree(root outputV3Directory) (privateTreePreview, error) {
	preview := privateTreePreview{}
	if err := previewPrivateDirectory(root, "", 0, &preview); err != nil {
		return privateTreePreview{}, err
	}
	return preview, nil
}

func previewPrivateDirectory(
	directory outputV3Directory,
	prefix string,
	depth int,
	preview *privateTreePreview,
) error {
	if depth > resumestate.MaxStateNestingDepth || preview.entries > resumeTreeEntryLimit {
		return errOutputInspectionLimit
	}
	names, err := directory.Names(resumeDirectoryChildLimit)
	if err != nil {
		return err
	}
	for _, name := range names {
		preview.entries++
		if preview.entries > resumeTreeEntryLimit {
			return errOutputInspectionLimit
		}
		state := name
		if prefix != "" {
			state = prefix + "/" + name
		}
		entry, err := directory.OpenEntry(name)
		if err != nil {
			return err
		}
		switch entry.Kind() {
		case outputV3EntryRegularFile:
			strict, strictErr := directory.OpenFile(name, true, false)
			if strictErr == nil {
				strictErr = strict.Close()
			}
			matches, matchErr := directory.EntryMatches(name, entry)
			if matchErr != nil || !matches {
				_ = entry.Close()
				return errors.Join(errOutputV3Unsafe, matchErr)
			}
			if strictErr != nil {
				preview.attention = append(preview.attention, ResumeAttention{
					Scope: ResumeAttentionFile, Code: "unsafe-private-entry", State: state,
					Detail: strictErr.Error(),
				})
			}
			allocated, sizeErr := entry.AllocatedSize()
			closeErr := entry.Close()
			if sizeErr != nil || closeErr != nil || math.MaxUint64-preview.allocatedBytes < allocated {
				return errors.Join(errOutputV3Unsafe, sizeErr, closeErr)
			}
			preview.allocatedBytes += allocated
			if strings.HasPrefix(state, resumestate.FilesDirectoryName+"/") && strings.HasSuffix(name, ".state") {
				preview.fileRecords++
			}
		case outputV3EntryDirectory:
			child, err := directory.OpenPinnedDirectory(entry, true)
			if err != nil {
				preview.attention = append(preview.attention, ResumeAttention{
					Scope: ResumeAttentionFile, Code: "unsafe-private-entry", State: state,
					Detail: err.Error(),
				})
				child, err = directory.OpenPinnedDirectory(entry, false)
			}
			if err != nil {
				_ = entry.Close()
				return err
			}
			err = previewPrivateDirectory(child, state, depth+1, preview)
			closeErr := errors.Join(child.Close(), entry.Close())
			if err != nil || closeErr != nil {
				return errors.Join(err, closeErr)
			}
		case outputV3EntryOther:
			if err := entry.Close(); err != nil {
				return err
			}
			preview.attention = append(preview.attention, ResumeAttention{
				Scope: ResumeAttentionFile, Code: "unsafe-private-entry", State: state,
			})
		case outputV3EntryAbsent:
			_ = entry.Close()
			return errOutputV3Unsafe
		}
	}
	return nil
}

func listLegacyResumeState(ctx context.Context, rootPath string) ([]ResumeStateSummary, error) {
	root, err := os.OpenRoot(rootPath)
	if err != nil {
		return nil, err
	}
	defer root.Close()
	directory, err := root.Open(".")
	if err != nil {
		return nil, err
	}
	const legacyDirectoryReadBatch = 256
	names := make([]string, 0)
	for {
		if err := ctx.Err(); err != nil {
			_ = directory.Close()
			return nil, err
		}
		batch, readErr := directory.Readdirnames(legacyDirectoryReadBatch)
		for _, name := range batch {
			if strings.HasPrefix(name, legacyOutputStatePrefix) {
				names = append(names, name)
				if len(names) > outputRootInspectionLimit {
					_ = directory.Close()
					return nil, errOutputInspectionLimit
				}
			}
		}
		if errors.Is(readErr, io.EOF) {
			break
		}
		if readErr != nil {
			_ = directory.Close()
			return nil, readErr
		}
	}
	if err := directory.Close(); err != nil {
		return nil, err
	}
	slices.Sort(names)
	result := make([]ResumeStateSummary, 0)
	for _, name := range names {
		info, err := root.Lstat(name)
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				continue
			}
			result = append(result, ResumeStateSummary{
				Reference: ResumeStateRef{
					rootPath: rootPath, kind: ResumeStateLegacyUntrusted, legacyName: name,
				},
				Attention: []ResumeAttention{{
					Scope: ResumeAttentionLegacy, Code: "legacy-v2-entry-unreadable",
					State: name, Detail: err.Error(),
				}},
			})
			continue
		}
		if strings.HasPrefix(name, legacyOutputStagePrefix) {
			result = append(result, ResumeStateSummary{
				Reference: ResumeStateRef{
					rootPath: rootPath, kind: ResumeStateLegacyUntrusted, legacyName: name,
				},
				Attention: []ResumeAttention{{
					Scope: ResumeAttentionLegacy, Code: "legacy-v2-stage-manual", State: name,
				}},
			})
			continue
		}
		if !strings.HasSuffix(name, legacyOutputJournalSuffix) {
			continue
		}
		if !info.Mode().IsRegular() {
			result = append(result, ResumeStateSummary{
				Reference: ResumeStateRef{
					rootPath: rootPath, kind: ResumeStateLegacyUntrusted, legacyName: name,
				},
				Attention: []ResumeAttention{{
					Scope: ResumeAttentionLegacy, Code: "legacy-v2-journal-unsafe", State: name,
				}},
			})
			continue
		}
		summary := ResumeStateSummary{
			Reference: ResumeStateRef{
				rootPath: rootPath, kind: ResumeStateLegacyUntrusted, legacyName: name,
			},
			Attention: []ResumeAttention{{
				Scope: ResumeAttentionLegacy, Code: "legacy-v2-untrusted", State: name,
			}},
		}
		file, openErr := root.Open(name)
		if openErr == nil {
			openedInfo, statErr := file.Stat()
			digest, size, digestErr := digestLegacyOutputJournal(file)
			closeErr := file.Close()
			if statErr == nil && openedInfo.Mode().IsRegular() && openedInfo.Size() >= 0 &&
				uint64(openedInfo.Size()) == size && digestErr == nil && closeErr == nil {
				summary.Reference.legacyRemovable = true
				summary.Reference.legacySize = size
				summary.Reference.legacyDigest = digest
			} else {
				openErr = errors.Join(statErr, digestErr, closeErr)
			}
		}
		if openErr != nil {
			summary.Attention = append(summary.Attention, ResumeAttention{
				Scope: ResumeAttentionLegacy, Code: "legacy-v2-journal-unreadable", State: name,
				Detail: openErr.Error(),
			})
		}
		result = append(result, summary)
	}
	return result, nil
}

func listGuardedLegacyResumeState(
	ctx context.Context,
	rootPath string,
	root outputV3Directory,
) ([]ResumeStateSummary, error) {
	if root == nil {
		return nil, errOutputV3Unsafe
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	names, err := root.NamesWithPrefix(legacyOutputStatePrefix, outputRootInspectionLimit+1)
	if err != nil {
		return nil, err
	}
	if len(names) > outputRootInspectionLimit {
		return nil, errOutputInspectionLimit
	}
	slices.Sort(names)
	result := make([]ResumeStateSummary, 0, len(names))
	for _, name := range names {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		kind, exact, inspectErr := root.ClassifyExactEntry(name)
		if kind == outputV3EntryAbsent && inspectErr == nil {
			continue
		}
		if inspectErr != nil || !exact {
			result = append(result, ResumeStateSummary{
				Reference: ResumeStateRef{
					rootPath: rootPath, kind: ResumeStateLegacyUntrusted, legacyName: name,
				},
				Attention: []ResumeAttention{{
					Scope: ResumeAttentionLegacy, Code: "legacy-v2-entry-unreadable",
					State: name, Detail: errors.Join(errOutputV3Unsafe, inspectErr).Error(),
				}},
			})
			continue
		}
		if strings.HasPrefix(name, legacyOutputStagePrefix) {
			result = append(result, ResumeStateSummary{
				Reference: ResumeStateRef{
					rootPath: rootPath, kind: ResumeStateLegacyUntrusted, legacyName: name,
				},
				Attention: []ResumeAttention{{
					Scope: ResumeAttentionLegacy, Code: "legacy-v2-stage-manual", State: name,
				}},
			})
			continue
		}
		if !strings.HasSuffix(name, legacyOutputJournalSuffix) {
			continue
		}
		if kind != outputV3EntryRegularFile {
			result = append(result, ResumeStateSummary{
				Reference: ResumeStateRef{
					rootPath: rootPath, kind: ResumeStateLegacyUntrusted, legacyName: name,
				},
				Attention: []ResumeAttention{{
					Scope: ResumeAttentionLegacy, Code: "legacy-v2-journal-unsafe", State: name,
				}},
			})
			continue
		}

		summary := ResumeStateSummary{
			Reference: ResumeStateRef{
				rootPath: rootPath, kind: ResumeStateLegacyUntrusted, legacyName: name,
			},
			Attention: []ResumeAttention{{
				Scope: ResumeAttentionLegacy, Code: "legacy-v2-untrusted", State: name,
			}},
		}
		file, openErr := root.OpenFile(name, false, false)
		if openErr == nil {
			digest, size, digestErr := digestLegacyOutputV3Journal(file)
			closeErr := file.Close()
			if digestErr == nil && closeErr == nil {
				summary.Reference.legacyRemovable = true
				summary.Reference.legacySize = size
				summary.Reference.legacyDigest = digest
			} else {
				openErr = errors.Join(digestErr, closeErr)
			}
		}
		if openErr != nil {
			summary.Attention = append(summary.Attention, ResumeAttention{
				Scope: ResumeAttentionLegacy, Code: "legacy-v2-journal-unreadable", State: name,
				Detail: openErr.Error(),
			})
		}
		result = append(result, summary)
	}
	return result, nil
}

func digestLegacyOutputV3Journal(file outputV3File) ([sha256.Size]byte, uint64, error) {
	if file == nil {
		return [sha256.Size]byte{}, 0, errOutputV3Unsafe
	}
	size, err := file.Size()
	if err != nil {
		return [sha256.Size]byte{}, 0, err
	}
	if size > maxLegacyOutputJournalBytes {
		return [sha256.Size]byte{}, 0, errLegacyOutputState
	}
	digest, readSize, err := digestLegacyOutputJournal(
		io.NewSectionReader(file, 0, maxLegacyOutputJournalBytes+1),
	)
	if err != nil || readSize != size {
		return [sha256.Size]byte{}, 0, errors.Join(io.ErrUnexpectedEOF, err)
	}
	return digest, size, nil
}

func digestLegacyOutputJournal(reader io.Reader) ([sha256.Size]byte, uint64, error) {
	digest := sha256.New()
	size, err := io.Copy(digest, io.LimitReader(reader, maxLegacyOutputJournalBytes+1))
	if err != nil {
		return [sha256.Size]byte{}, 0, err
	}
	if size > maxLegacyOutputJournalBytes {
		return [sha256.Size]byte{}, 0, errLegacyOutputState
	}
	var result [sha256.Size]byte
	copy(result[:], digest.Sum(nil))
	return result, uint64(size), nil
}

func attachLegacyResumePins(root outputV3Directory, summaries []ResumeStateSummary) error {
	removable := make([]int, 0, len(summaries))
	for index := range summaries {
		reference := &summaries[index].Reference
		if reference.kind == ResumeStateLegacyUntrusted && reference.legacyRemovable {
			removable = append(removable, index)
		}
	}
	if len(removable) == 0 {
		return nil
	}
	duplicate, err := root.Duplicate()
	if err == nil {
		var same bool
		same, err = duplicate.SameDirectory(root)
		if !same {
			err = errors.Join(errOutputRootUnsafe, err)
		}
	}
	if err != nil {
		_ = closeOutputV3Directory(duplicate)
		for _, index := range removable {
			reference := &summaries[index].Reference
			reference.legacyRemovable = false
			summaries[index].Attention = append(summaries[index].Attention, ResumeAttention{
				Scope: ResumeAttentionLegacy, Code: "legacy-v2-root-pin-unavailable",
				State: reference.legacyName, Detail: err.Error(),
			})
		}
		return nil
	}
	rootPin := newResumeStateDirectoryPin(duplicate)
	defer func() { _ = rootPin.Close() }()
	for _, index := range removable {
		reference := &summaries[index].Reference
		markChanged := func(cause error) {
			reference.legacyRemovable = false
			summaries[index].Attention = append(summaries[index].Attention, ResumeAttention{
				Scope: ResumeAttentionLegacy, Code: "legacy-v2-journal-replaced",
				State: reference.legacyName, Detail: cause.Error(),
			})
		}
		kind, exact, err := root.ClassifyExactEntry(reference.legacyName)
		if err != nil || !exact || kind != outputV3EntryRegularFile {
			markChanged(errors.Join(errOutputRootUnsafe, err))
			continue
		}
		entry, err := root.OpenEntry(reference.legacyName)
		if err != nil {
			markChanged(err)
			continue
		}
		if entry.Kind() != outputV3EntryRegularFile {
			_ = entry.Close()
			markChanged(errOutputRootUnsafe)
			continue
		}
		matches, matchErr := root.EntryMatches(reference.legacyName, entry)
		file, openErr := root.OpenFile(reference.legacyName, false, false)
		var encoded []byte
		var readErr, closeErr error
		if openErr == nil {
			encoded, readErr = readStateFile(file, maxLegacyOutputJournalBytes)
			closeErr = file.Close()
		}
		stable, stableErr := root.EntryMatches(reference.legacyName, entry)
		digest := sha256.Sum256(encoded)
		if matchErr != nil || !matches || openErr != nil || readErr != nil || closeErr != nil ||
			stableErr != nil || !stable || uint64(len(encoded)) != reference.legacySize ||
			digest != reference.legacyDigest {
			_ = entry.Close()
			markChanged(errors.Join(
				errOutputRootUnsafe, matchErr, openErr, readErr, closeErr, stableErr,
			))
			continue
		}
		if !rootPin.retain() {
			_ = entry.Close()
			markChanged(errOutputRootUnsafe)
			continue
		}
		reference.legacyRoot = rootPin
		reference.legacyPin = newResumeStateEntryPin(entry)
	}
	return nil
}

func unsafeIntentSummary(
	rootPath string,
	root resumestate.OutputRootBinding,
	intent transfer.ResumeIntent,
	intentName string,
	code string,
) ResumeStateSummary {
	return ResumeStateSummary{
		Reference: ResumeStateRef{
			rootPath: rootPath, root: root, intent: intent, kind: ResumeStateOpaqueUnsafe,
			namespaceName: intentName,
		},
		Attention: []ResumeAttention{{Scope: ResumeAttentionIntent, Code: code, State: intentName}},
	}
}

func unsafeSessionSummary(
	rootPath string,
	root resumestate.OutputRootBinding,
	intent transfer.ResumeIntent,
	intentName string,
	sessionName string,
	sessionKind outputV3EntryKind,
	sessionPin *resumeStateEntryPin,
	code string,
) ResumeStateSummary {
	return ResumeStateSummary{
		Reference: ResumeStateRef{
			rootPath: rootPath, root: root, intent: intent, kind: ResumeStateOpaqueUnsafe,
			namespaceName: intentName, sessionName: sessionName,
			sessionKind: sessionKind, sessionPin: sessionPin,
		},
		Attention: []ResumeAttention{{Scope: ResumeAttentionIntent, Code: code, State: sessionName}},
	}
}

func (authority *FilesystemOutputAuthority) discardResumeState(
	ctx context.Context,
	reference ResumeStateRef,
) (resultSettlement DiscardSettlement, resultErr error) {
	if authority == nil || authority.platformFactory == nil || reference.inventory == nil {
		return DiscardSettlement{}, transfer.ErrInvalidOutputBinding
	}
	if err := ctx.Err(); err != nil {
		return DiscardSettlement{}, err
	}
	resolved, err := reference.inventory.consume(reference)
	if err != nil {
		return DiscardSettlement{}, err
	}
	reference = resolved
	defer reference.releaseAuthority()
	if !reference.validAuthority() {
		return DiscardSettlement{}, transfer.ErrInvalidOutputBinding
	}
	platform, err := authority.platformFactory(reference.rootPath, false)
	if err != nil {
		return DiscardSettlement{}, rootOutputFault("certify discard root", err)
	}
	defer platform.Close()
	operation, acquireErr, acquireCloseErr := acquireResumePublicRootOperation(platform)
	if acquireErr != nil || acquireCloseErr != nil {
		return DiscardSettlement{}, errors.Join(
			resumeRootRevalidationFault("acquire resume discard root guard", acquireErr),
			resumeRootGuardCleanupFault("close unusable resume discard root guard", acquireCloseErr),
		)
	}
	defer func() {
		finishResumePublicRootOperation(operation, "resume discard", &resultErr)
		if resultErr != nil {
			resultSettlement = DiscardSettlement{}
		}
	}()
	guardedRoot := operation.root
	if reference.kind == ResumeStateLegacyUntrusted {
		return discardLegacyState(guardedRoot, reference)
	}
	listedEntry := reference.sessionPin.take()
	if listedEntry == nil {
		return DiscardSettlement{}, transfer.ErrInvalidOutputBinding
	}
	defer listedEntry.Close()
	if reference.sessionName == "" || reference.namespaceName == "" {
		return DiscardSettlement{}, outputFault(
			transfer.OutputFaultSession, transfer.OutputFaultContract, transfer.ErrInvalidOutputBinding,
		)
	}
	control, err := openInstalledControl(guardedRoot, platform)
	if errors.Is(err, fs.ErrNotExist) {
		return DiscardSettlement{Kind: DiscardAlreadyAbsent}, nil
	}
	if err != nil {
		return DiscardSettlement{}, err
	}
	defer control.Close()
	if control.control.OutputRoot() != reference.root {
		return DiscardSettlement{}, rootOutputFault("bind discard root", errOutputRootUnsafe)
	}
	coordinator, err := authority.acquireRuntimeNativeLock(
		func() (outputV3Lock, bool, error) {
			return control.directory.AcquireLock(resumestate.CoordinatorLockName, true)
		},
		filesystemOutputNativeLockContext{
			resumeIntent: reference.intent, sessionID: reference.session,
			scope: FilesystemOutputNativeLockCoordinator, failureScope: transfer.OutputFaultRoot,
		},
		rootOutputFault("acquire discard coordinator", errOutputRootUnsafe),
	)
	if err != nil {
		return DiscardSettlement{}, err
	}
	defer coordinator.Close()
	intentDir, err := control.sessions.OpenDirectory(reference.namespaceName, true)
	if errors.Is(err, fs.ErrNotExist) {
		return DiscardSettlement{Kind: DiscardAlreadyAbsent}, nil
	}
	if err != nil {
		return DiscardSettlement{}, intentOutputFault("open discard intent", err)
	}
	defer intentDir.Close()
	entryKind, err := intentDir.ObserveEntry(reference.sessionName)
	if err != nil {
		return DiscardSettlement{}, intentOutputFault("observe discard session entry", err)
	}
	if entryKind == outputV3EntryAbsent {
		return DiscardSettlement{Kind: DiscardAlreadyAbsent}, nil
	}
	if entryKind != reference.sessionKind || listedEntry.Kind() != reference.sessionKind {
		return DiscardSettlement{}, intentOutputFault(
			"bind listed discard session entry", errOutputIntentUnsafe,
		)
	}
	verifyDiscardEntry := func() error {
		if err := verifyPinnedDirectoryEntry(control.sessions, reference.namespaceName, intentDir); err != nil {
			return intentOutputFault("verify discard intent", err)
		}
		matches, err := intentDir.EntryMatches(reference.sessionName, listedEntry)
		if err != nil || !matches {
			return intentOutputFault("verify discard entry identity", errors.Join(errOutputIntentUnsafe, err))
		}
		return nil
	}
	if err := verifyDiscardEntry(); err != nil {
		return DiscardSettlement{}, err
	}
	if entryKind != outputV3EntryDirectory {
		return discardOpaqueSessionEntry(
			control.sessions, intentDir, reference, listedEntry, verifyDiscardEntry,
		)
	}
	privateSession := reference.kind != ResumeStateOpaqueUnsafe
	sessionDir, err := intentDir.OpenPinnedDirectory(listedEntry, privateSession)
	if err != nil {
		return DiscardSettlement{}, intentOutputFault("open discard session directory", err)
	}
	defer sessionDir.Close()
	if err := verifyPinnedDirectoryEntry(control.sessions, reference.namespaceName, intentDir); err != nil {
		return DiscardSettlement{}, intentOutputFault("pin discard intent", err)
	}
	var lock outputV3Lock
	lockKind, err := observeExactOutputEntry(sessionDir, resumestate.SessionLockName)
	if err != nil {
		return DiscardSettlement{}, intentOutputFault("observe discard session lock", err)
	}
	switch lockKind {
	case outputV3EntryRegularFile:
		lock, err = authority.acquireRuntimeNativeLock(
			func() (outputV3Lock, bool, error) {
				return sessionDir.AcquireLock(resumestate.SessionLockName, true)
			},
			filesystemOutputNativeLockContext{
				resumeIntent: reference.intent, sessionID: reference.session,
				scope: FilesystemOutputNativeLockSession, failureScope: transfer.OutputFaultSession,
			},
			intentOutputFault("revalidate discard session lock", errOutputIntentUnsafe),
		)
		if err != nil {
			return DiscardSettlement{}, err
		}
	case outputV3EntryAbsent:
		// A valid terminal suffix is verified below before any child is removed.
	default:
		return DiscardSettlement{}, intentOutputFault(
			"classify discard session lock", errOutputIntentUnsafe,
		)
	}
	lockOwned := true
	defer func() {
		if lockOwned && lock != nil {
			_ = lock.Close()
		}
	}()
	verifyDiscardAuthority := func() error {
		if err := verifyDiscardEntry(); err != nil {
			return err
		}
		return nil
	}
	if err := verifyDiscardAuthority(); err != nil {
		return DiscardSettlement{}, err
	}
	preview, err := previewPrivateTree(sessionDir)
	if err != nil {
		return DiscardSettlement{}, intentOutputFault("preview discard session", err)
	}
	store := authority.stateStore(reference.intent, reference.session)
	discardState, validHeader, corruptHeader, err := installDiscardingHeader(
		store, control.control, sessionDir, reference, lock != nil, verifyDiscardAuthority,
	)
	if err != nil {
		return DiscardSettlement{}, err
	}
	if lock == nil {
		if !validHeader {
			var remaining []string
			var inspectErr error
			if !corruptHeader {
				remaining, inspectErr = sessionDir.Names(1)
			}
			if inspectErr != nil || len(remaining) != 0 {
				return DiscardSettlement{}, intentOutputFault(
					"authorize lockless explicit discard",
					errors.Join(errOutputIntentUnsafe, inspectErr),
				)
			}
		} else {
			layout, layoutErr := inspectTerminalSessionLayoutWithLock(
				sessionDir,
				discardState.Header(),
				func() (outputV3Lock, error) {
					return authority.acquireRuntimeNativeLock(
						func() (outputV3Lock, bool, error) {
							return sessionDir.AcquireLock(resumestate.SessionLockName, true)
						},
						filesystemOutputNativeLockContext{
							resumeIntent: reference.intent, sessionID: reference.session,
							scope: FilesystemOutputNativeLockSession, failureScope: transfer.OutputFaultSession,
						},
						intentOutputFault("acquire terminal discard session lock", errOutputIntentUnsafe),
					)
				},
			)
			if layout != nil {
				layoutErr = errors.Join(layoutErr, layout.close())
			}
			if layoutErr != nil {
				return DiscardSettlement{}, intentOutputFault("validate lockless discard suffix", layoutErr)
			}
		}
	}
	if err := recoverCorruptDiscard(
		control, intentDir, sessionDir, reference, listedEntry, lock, verifyDiscardAuthority,
	); err != nil {
		return DiscardSettlement{}, err
	}
	lock, lockOwned = nil, false
	return DiscardSettlement{Kind: Discarded, RemovedBytes: preview.allocatedBytes}, nil
}

func discardOpaqueSessionEntry(
	sessionsDirectory outputV3Directory,
	intentDirectory outputV3Directory,
	reference ResumeStateRef,
	entry outputV3EntryRef,
	verifyAuthority func() error,
) (DiscardSettlement, error) {
	if entry == nil || entry.Kind() == outputV3EntryAbsent || entry.Kind() == outputV3EntryDirectory {
		return DiscardSettlement{}, intentOutputFault("classify opaque session entry", errOutputIntentUnsafe)
	}
	removedBytes, err := entry.AllocatedSize()
	if err != nil {
		return DiscardSettlement{}, intentOutputFault("preview opaque session entry", err)
	}
	if err := verifyAuthority(); err != nil {
		return DiscardSettlement{}, err
	}
	if err := intentDirectory.RemoveEntry(reference.sessionName, entry); err != nil {
		return DiscardSettlement{}, outputFault(transfer.OutputFaultSession, transfer.OutputFaultStateIO, err)
	}
	if err := removeEmptyIntentShell(
		sessionsDirectory, intentDirectory, reference.namespaceName,
	); err != nil {
		return DiscardSettlement{}, err
	}
	return DiscardSettlement{Kind: Discarded, RemovedBytes: removedBytes}, nil
}

func removeEmptyIntentShell(
	sessionsDirectory outputV3Directory,
	intentDirectory outputV3Directory,
	intentName string,
) error {
	remaining, err := intentDirectory.Names(1)
	if err != nil {
		return outputFault(transfer.OutputFaultSession, transfer.OutputFaultStateIO, err)
	}
	if len(remaining) != 0 {
		return nil
	}
	if err := verifyPinnedDirectoryEntry(sessionsDirectory, intentName, intentDirectory); err != nil {
		return intentOutputFault("verify empty explicit-discard intent", err)
	}
	if err := sessionsDirectory.RemoveDirectory(intentName, intentDirectory); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return outputFault(transfer.OutputFaultSession, transfer.OutputFaultStateIO, err)
	}
	if err := sessionsDirectory.Sync(); err != nil {
		return outputFault(transfer.OutputFaultSession, transfer.OutputFaultStateIO, err)
	}
	return nil
}

func installDiscardingHeader(
	store outputStateStore,
	control resumestate.Control,
	sessionDir outputV3Directory,
	reference ResumeStateRef,
	lockPresent bool,
	verifyAuthority func() error,
) (resumestate.SessionNamespaceAuthority, bool, bool, error) {
	if reference.kind == ResumeStateOpaqueUnsafe {
		// The fixed directory identity authorizes explicit cleanup, but an opaque
		// namespace cannot authorize interpreting or replacing any contained header.
		return resumestate.SessionNamespaceAuthority{}, false, false, nil
	}
	encoded, err := readStateRecord(sessionDir, resumestate.HeaderRecordName, resumestate.MaxSessionHeaderBytes)
	if err != nil {
		// A corrupt/missing header is itself the listed attention state. Explicit
		// discard authorizes the fixed session directory without inventing claims.
		if isMissing(err) || errors.Is(err, errOutputV3Unsafe) {
			return resumestate.SessionNamespaceAuthority{}, false, !isMissing(err), nil
		}
		return resumestate.SessionNamespaceAuthority{}, false, false, intentOutputFault("read discard header", err)
	}
	header, err := resumestate.DecodeHeader(encoded)
	if err != nil {
		return resumestate.SessionNamespaceAuthority{}, false, true, nil
	}
	namespace, err := resumestate.BindSessionNamespaceAuthority(
		control, header, reference.namespaceName, reference.sessionName,
	)
	if err != nil || header.ResumeIntent() != reference.intent || header.SessionID() != reference.session {
		// A decodable envelope that does not bind this fixed namespace is still
		// session-local corruption. It grants no transition authority, but the
		// explicit live-pin capability may remove it after every child and lock.
		return resumestate.SessionNamespaceAuthority{}, false, true, nil
	}
	if err := reconcileHeaderRecordTemporaries(sessionDir, namespace, verifyAuthority); err != nil {
		return resumestate.SessionNamespaceAuthority{}, false, false, intentOutputFault("reconcile discard-header update", err)
	}
	if !lockPresent {
		// Once a live process has removed the lock, only a bound terminal suffix may
		// authorize cleanup. Never synthesize a new transition in that suffix.
		if namespace.Header().Lifecycle() != resumestate.SessionCompleting &&
			namespace.Header().Lifecycle() != resumestate.SessionDiscarding {
			return resumestate.SessionNamespaceAuthority{}, false, false, nil
		}
		return namespace, true, false, nil
	}
	for namespace.Header().Lifecycle() != resumestate.SessionDiscarding {
		next := resumestate.SessionDiscarding
		if namespace.Header().Lifecycle() == resumestate.SessionPausing {
			next = resumestate.SessionPaused
		}
		updated, err := namespace.WithLifecycle(next)
		if err != nil {
			return resumestate.SessionNamespaceAuthority{}, false, false, intentOutputFault("transition discard header", err)
		}
		currentEncoded, err := resumestate.EncodeHeader(namespace.Header())
		if err != nil {
			return resumestate.SessionNamespaceAuthority{}, false, false, outputFault(transfer.OutputFaultSession, transfer.OutputFaultContract, err)
		}
		nextEncoded, err := resumestate.EncodeHeader(updated.Header())
		if err != nil {
			return resumestate.SessionNamespaceAuthority{}, false, false, outputFault(transfer.OutputFaultSession, transfer.OutputFaultContract, err)
		}
		if err := verifyAuthority(); err != nil {
			return resumestate.SessionNamespaceAuthority{}, false, false, err
		}
		outcome, replaceErr := store.replaceRecord(
			sessionDir,
			resumestate.HeaderRecordName,
			outputStateRecordImage{encoded: currentEncoded, generation: namespace.Header().StateGeneration()},
			outputStateRecordImage{encoded: nextEncoded, generation: updated.Header().StateGeneration()},
			resumestate.MaxSessionHeaderBytes,
		)
		switch outcome {
		case outputStateReplaceAdopted:
			namespace = updated
			if replaceErr != nil {
				// Child deletion must not begin while the owner that adopted this
				// header still has unresolved cleanup. A retry will reopen the exact
				// adopted generation and continue the terminal suffix.
				return resumestate.SessionNamespaceAuthority{}, false, false,
					outputFault(transfer.OutputFaultSession, transfer.OutputFaultStateIO, replaceErr)
			}
		case outputStateReplaceUnchanged:
			if replaceErr == nil {
				replaceErr = errOutputV3Unsafe
			}
			return resumestate.SessionNamespaceAuthority{}, false, false,
				outputFault(transfer.OutputFaultSession, transfer.OutputFaultStateIO, replaceErr)
		case outputStateReplaceUncertain:
			return resumestate.SessionNamespaceAuthority{}, false, false, intentOutputFault(
				"replace discard header with uncertain authority", errors.Join(errOutputV3Unsafe, replaceErr),
			)
		default:
			return resumestate.SessionNamespaceAuthority{}, false, false,
				outputFault(transfer.OutputFaultSession, transfer.OutputFaultContract, resumestate.ErrInvalidState)
		}
	}
	return namespace, true, false, nil
}

func recoverCorruptDiscard(
	control *outputControlNamespace,
	intentDir outputV3Directory,
	sessionDir outputV3Directory,
	reference ResumeStateRef,
	sessionEntry outputV3EntryRef,
	lock outputV3Lock,
	verifyParents func() error,
) error {
	if err := verifyParents(); err != nil {
		return err
	}
	for _, name := range []string{
		resumestate.StagesDirectoryName, resumestate.AnchorsDirectoryName, resumestate.FilesDirectoryName,
	} {
		if err := removePrivateEntry(sessionDir, name, 0, verifyParents); err != nil {
			return outputFault(transfer.OutputFaultSession, transfer.OutputFaultStateIO, err)
		}
	}

	names, err := sessionDir.Names(resumeDirectoryChildLimit)
	if err != nil {
		return outputFault(transfer.OutputFaultSession, transfer.OutputFaultStateIO, err)
	}
	slices.Sort(names)
	for _, name := range names {
		if name == resumestate.HeaderRecordName || name == resumestate.SessionLockName {
			continue
		}
		if err := verifyParents(); err != nil {
			return err
		}
		if err := removePrivateEntry(sessionDir, name, 0, verifyParents); err != nil {
			return outputFault(transfer.OutputFaultSession, transfer.OutputFaultStateIO, err)
		}
	}

	if lock != nil {
		if err := verifyParents(); err != nil {
			return err
		}
		lockFile := lock.File()
		if lockFile == nil {
			return intentOutputFault("remove corrupt-discard lock", errOutputIntentUnsafe)
		}
		if err := sessionDir.RemoveFile(resumestate.SessionLockName, lockFile); err != nil {
			return outputFault(transfer.OutputFaultSession, transfer.OutputFaultStateIO, err)
		}
		if err := sessionDir.Sync(); err != nil {
			return outputFault(transfer.OutputFaultSession, transfer.OutputFaultStateIO, err)
		}
		if err := lock.Close(); err != nil {
			return outputFault(transfer.OutputFaultSession, transfer.OutputFaultStateIO, err)
		}
	} else {
		if err := removePrivateEntry(
			sessionDir, resumestate.SessionLockName, 0, verifyParents,
		); err != nil {
			return outputFault(transfer.OutputFaultSession, transfer.OutputFaultStateIO, err)
		}
	}
	// The envelope is removed last, after every data-bearing child and the lock.
	// Keeping it until this cut makes explicit-discard restart diagnosis stable.
	if err := removePrivateEntry(
		sessionDir, resumestate.HeaderRecordName, 0, verifyParents,
	); err != nil {
		return outputFault(transfer.OutputFaultSession, transfer.OutputFaultStateIO, err)
	}
	return removeEmptyPinnedSessionShell(
		control.sessions, intentDir, sessionDir, reference.namespaceName, reference.sessionName,
		sessionEntry, verifyParents,
	)
}

func removeEmptyPinnedSessionShell(
	sessionsDirectory outputV3Directory,
	intentDirectory outputV3Directory,
	sessionDirectory outputV3Directory,
	intentName string,
	sessionName string,
	sessionEntry outputV3EntryRef,
	verifyAuthority func() error,
) error {
	remaining, err := sessionDirectory.Names(1)
	if err != nil || len(remaining) != 0 {
		return intentOutputFault("verify explicit-discard session shell", errors.Join(errOutputIntentUnsafe, err))
	}
	if err := verifyAuthority(); err != nil {
		return err
	}
	if err := intentDirectory.RemoveEntry(sessionName, sessionEntry); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return outputFault(transfer.OutputFaultSession, transfer.OutputFaultStateIO, err)
	}
	return removeEmptyIntentShell(sessionsDirectory, intentDirectory, intentName)
}

func removePrivateDirectoryContents(
	directory outputV3Directory,
	depth int,
	verifyAuthority func() error,
) error {
	if depth > resumestate.MaxStateNestingDepth {
		return errOutputInspectionLimit
	}
	names, err := directory.Names(resumeDirectoryChildLimit)
	if err != nil {
		return err
	}
	slices.Sort(names)
	for _, name := range names {
		if err := removePrivateEntry(directory, name, depth, verifyAuthority); err != nil {
			return err
		}
	}
	return nil
}

func removePrivateEntry(
	parent outputV3Directory,
	name string,
	depth int,
	verifyAuthority func() error,
) error {
	entry, err := parent.OpenEntry(name)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	defer entry.Close()
	switch entry.Kind() {
	case outputV3EntryDirectory:
		child, err := parent.OpenPinnedDirectory(entry, false)
		if err != nil {
			return err
		}
		verifyChildAuthority := func() error {
			if err := verifyAuthority(); err != nil {
				return err
			}
			matches, err := parent.EntryMatches(name, entry)
			if err != nil || !matches {
				return errors.Join(errOutputV3Unsafe, err)
			}
			return nil
		}
		if err := verifyChildAuthority(); err != nil {
			_ = child.Close()
			return err
		}
		if err := removePrivateDirectoryContents(child, depth+1, verifyChildAuthority); err != nil {
			_ = child.Close()
			return err
		}
		if err := verifyChildAuthority(); err != nil {
			_ = child.Close()
			return err
		}
		removeErr := parent.RemoveEntry(name, entry)
		return errors.Join(removeErr, child.Close())
	case outputV3EntryAbsent:
		return nil
	case outputV3EntryRegularFile, outputV3EntryOther:
		if err := verifyAuthority(); err != nil {
			return err
		}
		return parent.RemoveEntry(name, entry)
	default:
		return errOutputV3Unsafe
	}
}

func discardLegacyState(root outputV3Directory, reference ResumeStateRef) (DiscardSettlement, error) {
	if !reference.legacyRemovable || strings.HasPrefix(reference.legacyName, legacyOutputStagePrefix) {
		return DiscardSettlement{}, rootOutputFault("validate legacy discard state", errLegacyOutputState)
	}
	if root == nil {
		return DiscardSettlement{}, transfer.ErrInvalidOutputBinding
	}
	fixedRoot := reference.legacyRoot.fixedDirectory()
	if fixedRoot == nil {
		return DiscardSettlement{}, transfer.ErrInvalidOutputBinding
	}
	sameRoot, err := fixedRoot.SameDirectory(root)
	if err != nil || !sameRoot {
		return DiscardSettlement{}, rootOutputFault(
			"bind legacy discard root", errors.Join(errOutputRootUnsafe, err),
		)
	}
	entry := reference.legacyPin.take()
	if entry == nil {
		return DiscardSettlement{}, transfer.ErrInvalidOutputBinding
	}
	if entry.Kind() != outputV3EntryRegularFile {
		_ = entry.Close()
		return DiscardSettlement{}, rootOutputFault("validate pinned legacy discard state", errLegacyOutputState)
	}
	matches, matchErr := root.EntryMatches(reference.legacyName, entry)
	if matchErr != nil || !matches {
		_ = entry.Close()
		return DiscardSettlement{}, rootOutputFault(
			"bind pinned legacy discard state", errors.Join(errOutputRootUnsafe, matchErr),
		)
	}
	file, openErr := root.OpenFile(reference.legacyName, false, false)
	var encoded []byte
	var readErr, fileCloseErr error
	if openErr == nil {
		encoded, readErr = readStateFile(file, maxLegacyOutputJournalBytes)
		fileCloseErr = file.Close()
	}
	stable, stableErr := root.EntryMatches(reference.legacyName, entry)
	digest := sha256.Sum256(encoded)
	if openErr != nil || readErr != nil || fileCloseErr != nil || stableErr != nil || !stable ||
		uint64(len(encoded)) != reference.legacySize || digest != reference.legacyDigest {
		_ = entry.Close()
		return DiscardSettlement{}, rootOutputFault(
			"verify fixed legacy discard state",
			errors.Join(errOutputRootUnsafe, openErr, readErr, fileCloseErr, stableErr),
		)
	}
	allocated, allocationErr := entry.AllocatedSize()
	if allocationErr != nil {
		_ = entry.Close()
		return DiscardSettlement{}, rootOutputFault("preview legacy discard state", allocationErr)
	}
	removeErr := root.RemoveEntry(reference.legacyName, entry)
	syncErr := root.Sync()
	closeErr := entry.Close()
	if err := errors.Join(removeErr, syncErr, closeErr); err != nil {
		return DiscardSettlement{}, rootOutputFault("remove legacy discard state", err)
	}
	return DiscardSettlement{Kind: Discarded, RemovedBytes: allocated}, nil
}

var _ transfer.OutputAuthority = (*FilesystemOutputAuthority)(nil)
