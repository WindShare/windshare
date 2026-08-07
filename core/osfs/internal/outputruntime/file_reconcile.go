package outputruntime

import (
	"errors"
	"fmt"

	"github.com/windshare/windshare/core/osfs/internal/outputcap"
	"github.com/windshare/windshare/core/osfs/internal/outputfault"
	"github.com/windshare/windshare/core/osfs/internal/outputnamespace"
	"github.com/windshare/windshare/core/osfs/internal/resumestate"
	"github.com/windshare/windshare/core/transfer"
)

func (session *Session) claimFileStart(digest resumestate.LocatorDigest) error {
	session.mu.Lock()
	defer session.mu.Unlock()
	if session.closed || session.settling || session.poisoned {
		return outputfault.New(transfer.OutputFaultSession, transfer.OutputFaultOwnership, outputfault.ErrSessionClosed)
	}
	if _, exists := session.active[digest]; exists {
		return outputfault.New(transfer.OutputFaultFile, transfer.OutputFaultOwnership, outputfault.ErrFileActive)
	}
	if _, exists := session.beginning[digest]; exists {
		return outputfault.New(transfer.OutputFaultFile, transfer.OutputFaultOwnership, outputfault.ErrFileActive)
	}
	if len(session.active)+len(session.beginning) >= maxFilesystemOutputTransactions {
		return outputfault.New(transfer.OutputFaultSession, transfer.OutputFaultOwnership, outputfault.ErrTransactionLimit)
	}
	session.beginning[digest] = struct{}{}
	session.beginWG.Add(1)
	return nil
}

func (session *Session) releaseFileStart(digest resumestate.LocatorDigest) {
	session.mu.Lock()
	delete(session.beginning, digest)
	session.mu.Unlock()
	session.beginWG.Done()
}

func (session *Session) allocateOutputObjectID(
	digest resumestate.LocatorDigest,
) (resumestate.OutputObjectID, error) {
	for range outputnamespace.AllocationAttempts {
		objectID, err := session.owner.objectIDs.NewOutputObjectID()
		if err != nil {
			return resumestate.OutputObjectID{}, err
		}
		if objectID.IsZero() {
			continue
		}
		session.mu.Lock()
		_, claimed := session.objectClaims[objectID]
		if !claimed {
			session.objectClaims[objectID] = digest
		}
		session.mu.Unlock()
		if claimed {
			continue
		}
		occupied, err := session.outputObjectNameOccupied(objectID)
		if err != nil {
			session.releaseOutputObjectClaim(objectID, digest)
			return resumestate.OutputObjectID{}, err
		}
		if !occupied {
			return objectID, nil
		}
		session.releaseOutputObjectClaim(objectID, digest)
	}
	return resumestate.OutputObjectID{}, fmt.Errorf("%w: allocate unique output object", outputcap.ErrUnsafeNamespace)
}

func (session *Session) releaseOutputObjectClaim(
	objectID resumestate.OutputObjectID,
	digest resumestate.LocatorDigest,
) {
	session.mu.Lock()
	if session.objectClaims[objectID] == digest {
		delete(session.objectClaims, objectID)
	}
	session.mu.Unlock()
}

func (session *Session) outputObjectNameOccupied(id resumestate.OutputObjectID) (bool, error) {
	for _, candidate := range []struct {
		parent outputcap.Directory
		name   resumestate.ShardedName
	}{
		{session.anchorsDir, resumestate.AnchorName(id)},
		{session.stagesDir, resumestate.StageName(id)},
	} {
		shard, present, err := openOutputShard(candidate.parent, candidate.name.Shard(), false)
		if err != nil {
			return false, err
		}
		if !present {
			continue
		}
		kind, observeErr := shard.ObserveEntry(candidate.name.Name())
		closeErr := shard.Close()
		if observeErr != nil || closeErr != nil {
			return false, errors.Join(observeErr, closeErr)
		}
		if kind != outputcap.EntryAbsent {
			return true, nil
		}
	}
	return false, nil
}

func closeOutputFile(file outputcap.File) error {
	if file == nil {
		return nil
	}
	return file.Close()
}

func fileOutputFault(operation string, cause error) error {
	code := transfer.OutputFaultStateIO
	if errors.Is(cause, outputcap.ErrUnsafeNamespace) {
		code = transfer.OutputFaultOwnership
	}
	return outputfault.New(transfer.OutputFaultFile, code, fmt.Errorf("%s: %w", operation, cause))
}

func finalParentSyncObservation(
	record resumestate.CheckpointRuntimeState,
	parentSynced bool,
) resumestate.FinalParentObservation {
	if parentSynced || record.Phase() == resumestate.CheckpointRuntimePublished || record.Phase() == resumestate.CheckpointRuntimeRetiring {
		return resumestate.FinalParentSynced
	}
	return resumestate.FinalParentSyncRequired
}

func internalCleanupNeedsAttentionFault(operation string) error {
	return pauseRequiredFileOutputFault(fileOutputFault(
		operation,
		errors.Join(outputcap.ErrUnsafeNamespace, errNativeInternalCleanupNeedsAttention),
	))
}

func directoryOutputFault(operation string, cause error) error {
	return outputfault.New(
		transfer.OutputFaultSession,
		transfer.OutputFaultStateIO,
		fmt.Errorf("%s: %w", operation, cause),
	)
}

var _ transfer.FileTransaction = (*FileTransaction)(nil)
