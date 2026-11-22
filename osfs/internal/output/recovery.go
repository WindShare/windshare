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
		if name == dirLockFileName || name == authorityIDFile {
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
	if lock, _, lockErr := acquireLockFile(opDir, taskLockFileName, true, false); errors.Is(lockErr, ErrNamespaceLockBusy) {
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

	items := make([]RecoveryItem, 0, len(state.partials)+len(state.delivered))
	hasResumable := len(state.delivered) > 0
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

	ctrlDir, err := openChildDir(root, ControlDirName)
	if err != nil {
		return RecoverySummary{}, DiagnoseFailure(FailureLookup, err)
	}
	// Hold the directory lock across task-lock verification, handle release, and
	// directory removal so no concurrent get or resume retry can reopen opID
	// after task.lock closes (required before deletion on Windows).
	dirLock, _, lockErr := acquireLockFile(ctrlDir, dirLockFileName, false, true)
	if lockErr != nil {
		_ = ctrlDir.Close()
		return RecoverySummary{}, DiagnoseFailure(FailureOpen, lockErr)
	}

	opDirName := hex.EncodeToString(opID.Bytes())
	opDir, err := openChildDir(ctrlDir, opDirName)
	if err != nil {
		_ = errors.Join(dirLock.Close(), ctrlDir.Close())
		return RecoverySummary{}, DiagnoseFailure(FailureLookup, err)
	}

	taskLock, _, lockErr := acquireLockFile(opDir, taskLockFileName, false, false)
	if errors.Is(lockErr, ErrNamespaceLockBusy) {
		_ = errors.Join(opDir.Close(), dirLock.Close(), ctrlDir.Close())
		return RecoverySummary{}, DiagnoseFailure(FailureOpen, ErrBusy)
	}
	if lockErr != nil {
		_ = errors.Join(opDir.Close(), dirLock.Close(), ctrlDir.Close())
		return RecoverySummary{}, DiagnoseFailure(FailureOpen, lockErr)
	}

	state, _, readErr := readJournalState(opDir)
	releaseErr := errors.Join(taskLock.Close(), opDir.Close())
	if releaseErr != nil {
		releaseErr = errors.Join(releaseErr, dirLock.Close(), ctrlDir.Close())
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
		return summary, DiagnoseFailure(FailureClose, errors.Join(removeErr, closeErr))
	}
	if closeErr != nil {
		return summary, DiagnoseFailure(FailureClose, closeErr)
	}
	return summary, nil
}
