package output

import (
	"context"
	"encoding/hex"
	"errors"
	"io/fs"
	"path/filepath"
	"slices"
	"sort"

	"github.com/windshare/windshare/core/transfer"
	"github.com/windshare/windshare/core/transfer/receivecontract"
)

type RecoveryListStatus uint8

const (
	RecoveryListReady RecoveryListStatus = iota + 1
	RecoveryListNeedsAttention
)

type RecoveryOperationState uint8

const (
	RecoveryOperationIncomplete RecoveryOperationState = iota + 1
	RecoveryOperationResumable
	RecoveryOperationShareStopped
	RecoveryOperationCleanupPending
	RecoveryOperationNeedsAttention
	RecoveryOperationDiscarded
)

type RecoveryItemState uint8

const (
	RecoveryItemIncomplete RecoveryItemState = iota + 1
	RecoveryItemResumable
	RecoveryItemPublished
	RecoveryItemFailed
	RecoveryItemBlocked
)

type RecoveryItemBlockReason uint8

const (
	RecoveryItemBlockNone RecoveryItemBlockReason = iota
	RecoveryItemBlockPublicationUnknown
	RecoveryItemBlockCheckpointInvalid
	RecoveryItemBlockOwnedObjectUnknown
	RecoveryItemBlockRevisionConflict
)

type RecoveryItem struct {
	canonicalPath string
	reference     string
	state         RecoveryItemState
	blockReason   RecoveryItemBlockReason
}

func (i RecoveryItem) CanonicalPath() string                { return i.canonicalPath }
func (i RecoveryItem) Reference() string                    { return i.reference }
func (i RecoveryItem) State() RecoveryItemState             { return i.state }
func (i RecoveryItem) BlockReason() RecoveryItemBlockReason { return i.blockReason }

type RecoverySummary struct {
	valid           bool
	operationID     receivecontract.OperationID
	intentDigest    transfer.ReceiveIntentDigest
	stateGeneration uint64
	state           RecoveryOperationState
	attention       AttentionReason
	busy            bool
	items           []RecoveryItem
}

func (s RecoverySummary) Valid() bool                                       { return s.valid }
func (s RecoverySummary) OperationID() receivecontract.OperationID          { return s.operationID }
func (s RecoverySummary) ReceiveIntentDigest() transfer.ReceiveIntentDigest { return s.intentDigest }
func (s RecoverySummary) StateGeneration() uint64                           { return s.stateGeneration }
func (s RecoverySummary) State() RecoveryOperationState                     { return s.state }
func (s RecoverySummary) NeedsAttentionReason() AttentionReason             { return s.attention }
func (s RecoverySummary) Busy() bool                                        { return s.busy }
func (s RecoverySummary) Items() []RecoveryItem                             { return slices.Clone(s.items) }

type RecoveryInventory struct {
	status         RecoveryListStatus
	summaries      []RecoverySummary
	unknownEntries bool
}

func (inv RecoveryInventory) Status() RecoveryListStatus   { return inv.status }
func (inv RecoveryInventory) Summaries() []RecoverySummary { return slices.Clone(inv.summaries) }
func (inv RecoveryInventory) UnknownEntries() bool         { return inv.unknownEntries }

// ListOperations inspects retained native operations under <rootPath>/.windshare-tmp
// without creating or modifying anything when no control directory exists.
func ListOperations(ctx context.Context, rootPath string) (RecoveryInventory, error) {
	if ctx == nil {
		return RecoveryInventory{}, ErrInvalidContract
	}
	if err := ctx.Err(); err != nil {
		return RecoveryInventory{}, err
	}
	if rootPath == "" || !filepath.IsAbs(rootPath) {
		return RecoveryInventory{}, transfer.ErrInvalidOutputBinding
	}
	root, _, err := OpenContainer(filepath.Clean(rootPath), false)
	if err != nil {
		return RecoveryInventory{}, DiagnoseFailure(FailureDestinationBinding, err)
	}
	defer root.Close()

	dirLock, err := acquireDirLock(root)
	if err != nil {
		return RecoveryInventory{}, DiagnoseFailure(FailureInventory, err)
	}
	defer dirLock.Close()

	ctrlDir, err := openChildDir(root, ControlDirName)
	if errors.Is(err, fs.ErrNotExist) {
		return RecoveryInventory{status: RecoveryListReady}, nil
	}
	if err != nil {
		if errors.Is(err, ErrUnsafeNamespace) {
			return RecoveryInventory{}, DiagnoseFailure(FailureDestinationBinding, err)
		}
		return RecoveryInventory{}, DiagnoseFailure(FailureInventory, err)
	}
	defer ctrlDir.Close()

	names, err := readDirNames(ctrlDir)
	if err != nil {
		return RecoveryInventory{}, DiagnoseFailure(FailureInventory, err)
	}
	sort.Strings(names)

	var summaries []RecoverySummary
	unknownEntries := false
	for _, name := range names {
		if name == authorityIDFile {
			continue
		}
		opID, ok := isValidOpDirName(name)
		if !ok {
			unknownEntries = true
			continue
		}
		opDir, err := openChildDir(ctrlDir, name)
		if err != nil {
			unknownEntries = true
			continue
		}
		summary, ok := inspectOperationDir(opID, opDir)
		_ = opDir.Close()
		if !ok {
			unknownEntries = true
			continue
		}
		summaries = append(summaries, summary)
	}

	status := RecoveryListReady
	if unknownEntries {
		status = RecoveryListNeedsAttention
	} else {
		for _, s := range summaries {
			if s.state == RecoveryOperationNeedsAttention {
				status = RecoveryListNeedsAttention
				break
			}
		}
	}
	return RecoveryInventory{
		status:         status,
		summaries:      summaries,
		unknownEntries: unknownEntries,
	}, nil
}

func inspectOperationDir(opID receivecontract.OperationID, opDir *Dir) (RecoverySummary, bool) {
	busy := false
	if lock, _, lockErr := acquireLockFile(opDir, taskLockFileName, true); errors.Is(lockErr, ErrNamespaceLockBusy) {
		busy = true
	} else if lock != nil {
		_ = lock.Close()
	}

	state, _, err := readJournalState(opDir)
	if err != nil || !state.hasMeta {
		return RecoverySummary{}, false
	}
	meta := state.meta
	if meta.Intent.OperationID() != opID {
		return RecoverySummary{}, false
	}

	if meta.Lifecycle == journalLifecycleCleanupPending {
		// Once retirement has started, partial files may already have been
		// removed; omit stale per-item projections and surface cleanup-pending.
		attention := meta.Attention
		if attention != AttentionNone && attention != AttentionCleanup {
			attention = AttentionCleanup
		}
		return RecoverySummary{
			valid:           true,
			operationID:     opID,
			intentDigest:    meta.Intent.Digest(),
			stateGeneration: meta.StateGen,
			state:           RecoveryOperationCleanupPending,
			attention:       attention,
			busy:            busy,
		}, true
	}

	items := make([]RecoveryItem, 0, len(state.partials)+len(state.publishing)+len(state.delivered))
	hasResumable := len(state.delivered) > 0 || len(state.publishing) > 0
	for _, p := range state.partials {
		if p.BlockedReason != 0 {
			items = append(items, RecoveryItem{
				canonicalPath: p.Path,
				state:         RecoveryItemBlocked,
				blockReason:   projectItemBlockReason(p.BlockedReason),
			})
			continue
		}
		if len(p.Ranges) > 0 {
			hasResumable = true
			items = append(items, RecoveryItem{
				canonicalPath: p.Path,
				state:         RecoveryItemResumable,
			})
		} else {
			items = append(items, RecoveryItem{
				canonicalPath: p.Path,
				state:         RecoveryItemIncomplete,
			})
		}
	}
	for _, p := range state.publishing {
		items = append(items, RecoveryItem{
			canonicalPath: p.Path,
			state:         RecoveryItemResumable,
		})
	}
	for _, d := range state.delivered {
		items = append(items, RecoveryItem{
			canonicalPath: d.Path,
			state:         RecoveryItemPublished,
		})
	}
	sort.Slice(items, func(i, j int) bool {
		return items[i].canonicalPath < items[j].canonicalPath
	})

	opState := RecoveryOperationIncomplete
	attention := AttentionNone
	switch meta.Lifecycle {
	case journalLifecycleStopped:
		opState = RecoveryOperationShareStopped
	case journalLifecycleAttn:
		opState = RecoveryOperationNeedsAttention
		attention = meta.Attention
		if !attention.Valid() {
			attention = AttentionOperation
		}
	default:
		if hasResumable && !busy {
			opState = RecoveryOperationResumable
		}
	}

	return RecoverySummary{
		valid:           true,
		operationID:     opID,
		intentDigest:    meta.Intent.Digest(),
		stateGeneration: meta.StateGen,
		state:           opState,
		attention:       attention,
		busy:            busy,
		items:           items,
	}, true
}

func projectItemBlockReason(reason transfer.ItemBlockReason) RecoveryItemBlockReason {
	switch reason {
	case transfer.ItemBlockRevisionConflict:
		return RecoveryItemBlockRevisionConflict
	case transfer.ItemBlockPublicationAmbiguous:
		return RecoveryItemBlockPublicationUnknown
	case transfer.ItemBlockOwnedObjectUnknown, transfer.ItemBlockOwnershipUnknown:
		return RecoveryItemBlockOwnedObjectUnknown
	default:
		return RecoveryItemBlockCheckpointInvalid
	}
}

// DiscardOperation deletes WindShare's private scratch directory for opID while
// preserving every published and foreign file in the user's destination.
func DiscardOperation(
	ctx context.Context,
	rootPath string,
	opID receivecontract.OperationID,
) (RecoverySummary, error) {
	if ctx == nil || opID.IsZero() {
		return RecoverySummary{}, ErrInvalidContract
	}
	if err := ctx.Err(); err != nil {
		return RecoverySummary{}, err
	}
	if rootPath == "" || !filepath.IsAbs(rootPath) {
		return RecoverySummary{}, transfer.ErrInvalidOutputBinding
	}
	root, _, err := OpenContainer(filepath.Clean(rootPath), false)
	if err != nil {
		return RecoverySummary{}, DiagnoseFailure(FailureDestinationBinding, err)
	}
	defer root.Close()

	// Hold the directory lock across control-dir lookup, task-lock
	// verification, handle release, and directory removal so no concurrent
	// get or resume retry can reopen opID or observe a half-removed
	// .windshare-tmp directory.
	dirLock, lockErr := acquireDirLock(root)
	if lockErr != nil {
		return RecoverySummary{}, DiagnoseFailure(FailureOpen, lockErr)
	}
	ctrlDir, err := openChildDir(root, ControlDirName)
	if err != nil {
		_ = dirLock.Close()
		return RecoverySummary{}, DiagnoseFailure(FailureLookup, err)
	}

	opDirName := hex.EncodeToString(opID.Bytes())
	opDir, err := openChildDir(ctrlDir, opDirName)
	if err != nil {
		_ = errors.Join(ctrlDir.Close(), dirLock.Close())
		return RecoverySummary{}, DiagnoseFailure(FailureLookup, err)
	}

	// Probe an existing task lock without creating one under dirLock: every active
	// receiver acquires task.lock while holding dirLock, so a missing lock file
	// proves no active owner and avoids mutating a replaced directory on refusal.
	taskLock, _, lockErr := acquireLockFile(opDir, taskLockFileName, true)
	if errors.Is(lockErr, ErrNamespaceLockBusy) {
		_ = errors.Join(opDir.Close(), ctrlDir.Close(), dirLock.Close())
		return RecoverySummary{}, DiagnoseFailure(FailureOpen, ErrBusy)
	}
	if lockErr != nil && !errors.Is(lockErr, fs.ErrNotExist) {
		_ = errors.Join(opDir.Close(), ctrlDir.Close(), dirLock.Close())
		return RecoverySummary{}, DiagnoseFailure(FailureOpen, lockErr)
	}

	state, _, readErr := readJournalState(opDir)
	// Confirm the locked directory still belongs to opID before deleting it;
	// confirmation prompts run unlocked, while corrupt headers without a verified
	// identity remain discardable inside WindShare's private scratch area.
	if state.hasMeta && state.meta.Intent.OperationID() != opID {
		closeErr := errors.Join(taskLock.Close(), opDir.Close(), ctrlDir.Close(), dirLock.Close())
		return RecoverySummary{}, freezeFailure(DiagnoseFailure(FailureLookup, ErrOperationChanged), closeErr)
	}
	releaseErr := errors.Join(taskLock.Close(), opDir.Close())
	if releaseErr != nil {
		releaseErr = errors.Join(releaseErr, ctrlDir.Close(), dirLock.Close())
		return RecoverySummary{}, DiagnoseFailure(FailureClose, releaseErr)
	}

	removeErr, closeErr := removeOperationLocked(root, ctrlDir, dirLock, opDirName)
	summary := RecoverySummary{
		valid:           true,
		operationID:     opID,
		stateGeneration: max(state.meta.StateGen, 1),
		state:           RecoveryOperationDiscarded,
		attention:       AttentionNone,
	}
	if readErr == nil && state.hasMeta {
		summary.intentDigest = state.meta.Intent.Digest()
	}
	if removeErr != nil {
		summary.state = RecoveryOperationCleanupPending
		summary.attention = AttentionCleanup
		// Keep scratch removal failure separate from FailureClose so clean
		// lease release is not misclassified as an unclosed destination handle.
		return summary, freezeFailure(DiagnoseFailure(FailureDurability, removeErr), closeErr)
	}
	if closeErr != nil {
		return summary, DiagnoseFailure(FailureClose, closeErr)
	}
	return summary, nil
}
