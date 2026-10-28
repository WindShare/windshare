package destinationauthority

import (
	"errors"
	"io/fs"

	"github.com/windshare/windshare/core/transfer/ordinaryoutput"
	"github.com/windshare/windshare/osfs/internal/checkpointmodel"
	"github.com/windshare/windshare/osfs/internal/outputcap"
)

type LiveCleanupBatchState uint8

const (
	LiveCleanupBatchComplete LiveCleanupBatchState = iota + 1
	LiveCleanupBatchPartial
	LiveCleanupBatchUnverifiable
)

type LiveCleanupBatch struct {
	State   LiveCleanupBatchState
	Tickets []checkpointmodel.LiveCleanupTicket
}

// LiveCleanupProgress describes only the bounded work performed at this bind.
// More means deferred work, never a loss of cleanup ownership or capability.
type LiveCleanupProgress struct {
	InspectedTickets uint64
	ReclaimedTickets uint64
	More             bool
}

type LiveCleanupJournal interface {
	ReadBatch(maximumTickets int) (LiveCleanupBatch, error)
	Create(checkpointmodel.LiveCleanupTicket) error
	Replace(checkpointmodel.LiveCleanupTicket, checkpointmodel.LiveCleanupTicket) error
	Delete(checkpointmodel.LiveCleanupTicket) error
	Close() error
}

type LiveCleanupJournalOpener func(outputcap.Directory) (LiveCleanupJournalHandle, error)

type LiveCleanupJournalHandle struct {
	journal LiveCleanupJournal
}

func NewLiveCleanupJournalHandle(journal LiveCleanupJournal) (LiveCleanupJournalHandle, error) {
	if journal == nil {
		return LiveCleanupJournalHandle{}, ErrInvalidConfiguration
	}
	return LiveCleanupJournalHandle{journal: journal}, nil
}

func (handle LiveCleanupJournalHandle) valid() bool { return handle.journal != nil }

func (handle *LiveCleanupJournalHandle) Close() error {
	if handle == nil || handle.journal == nil {
		return nil
	}
	err := handle.journal.Close()
	handle.journal = nil
	return err
}

func reconcileLiveCleanup(
	journal LiveCleanupJournal,
	proof outputcap.Directory,
	capabilities outputcap.DestinationCapabilities,
	profile checkpointmodel.LiveCleanupNativeProfile,
) (result outputcap.DestinationCapabilities, progress LiveCleanupProgress, resultErr error) {
	result = capabilities
	if !capabilities.CrashCleanup().Supported() {
		return
	}
	snapshot, err := journal.ReadBatch(int(ordinaryoutput.MaximumLiveCleanupTicketsPerBindV1))
	if err != nil {
		return outputcap.DestinationCapabilities{}, progress, err
	}
	switch snapshot.State {
	case LiveCleanupBatchPartial:
		if len(snapshot.Tickets) != int(ordinaryoutput.MaximumLiveCleanupTicketsPerBindV1) {
			return outputcap.DestinationCapabilities{}, progress, ErrInvalidConfiguration
		}
		progress.More = true
	case LiveCleanupBatchUnverifiable:
		result, resultErr = downgradedCleanupCapabilities(capabilities, outputcap.CapabilityReasonCleanupOwnershipUnknown)
		return
	case LiveCleanupBatchComplete:
	default:
		return outputcap.DestinationCapabilities{}, progress, ErrInvalidConfiguration
	}
	if len(snapshot.Tickets) > int(ordinaryoutput.MaximumLiveCleanupTicketsPerBindV1) {
		return outputcap.DestinationCapabilities{}, progress, ErrInvalidConfiguration
	}
	for _, ticket := range snapshot.Tickets {
		progress.InspectedTickets++
		if !ticket.Valid() || ticket.Profile() != profile {
			result, resultErr = downgradedCleanupCapabilities(capabilities, outputcap.CapabilityReasonCleanupOwnershipUnknown)
			return
		}
		proved, reconcileErr := reconcileLiveCleanupTicket(journal, proof, ticket)
		if reconcileErr != nil {
			return outputcap.DestinationCapabilities{}, progress, reconcileErr
		}
		if !proved {
			result, resultErr = downgradedCleanupCapabilities(capabilities, outputcap.CapabilityReasonCleanupOwnershipUnknown)
			return
		}
		progress.ReclaimedTickets++
	}
	return
}

func (authority *BoundDestination) LiveCleanupProgress() LiveCleanupProgress {
	return authority.cleanupProgress
}

func reconcileLiveCleanupTicket(
	journal LiveCleanupJournal,
	proof outputcap.Directory,
	ticket checkpointmodel.LiveCleanupTicket,
) (bool, error) {
	kind, exact, err := proof.ClassifyExactEntry(ticket.StageName())
	if err != nil {
		return false, err
	}
	if !exact {
		return false, nil
	}
	if kind == outputcap.EntryAbsent {
		return reconcileMissingLiveCleanupStage(journal, ticket)
	}
	if kind != outputcap.EntryRegularFile || ticket.State() == checkpointmodel.LiveCleanupStageRemoved {
		return false, nil
	}
	return reconcilePresentLiveCleanupStage(journal, proof, ticket)
}

func reconcileMissingLiveCleanupStage(
	journal LiveCleanupJournal,
	ticket checkpointmodel.LiveCleanupTicket,
) (bool, error) {
	switch ticket.State() {
	case checkpointmodel.LiveCleanupTicketCommitted, checkpointmodel.LiveCleanupStageRemoved:
		return true, journal.Delete(ticket)
	case checkpointmodel.LiveCleanupStageCreated:
		next, err := checkpointmodel.ReduceLiveCleanupTicket(
			ticket, checkpointmodel.LiveCleanupRecordStageRemoved,
		)
		if err != nil {
			return false, err
		}
		if err := journal.Replace(ticket, next); err != nil {
			return false, err
		}
		return true, journal.Delete(next)
	default:
		return false, nil
	}
}

func reconcilePresentLiveCleanupStage(
	journal LiveCleanupJournal,
	proof outputcap.Directory,
	ticket checkpointmodel.LiveCleanupTicket,
) (bool, error) {
	current, err := promoteLiveCleanupStageTicket(journal, ticket)
	if err != nil {
		return false, err
	}
	stage, proved, err := openExactLiveCleanupStage(proof, current)
	if err != nil || !proved {
		return false, err
	}
	remover, ok := proof.(liveCleanupStageRemover)
	if !ok {
		return false, errors.Join(ErrInvalidConfiguration, stage.Close())
	}
	if err := remover.RemoveLiveCleanupStage(current, stage); err != nil {
		if errors.Is(err, outputcap.ErrUnsafeNamespace) {
			return false, stage.Close()
		}
		return false, errors.Join(err, stage.Close())
	}
	if err := stage.Close(); err != nil {
		return false, err
	}
	removed, err := checkpointmodel.ReduceLiveCleanupTicket(
		current, checkpointmodel.LiveCleanupRecordStageRemoved,
	)
	if err != nil {
		return false, err
	}
	if err := journal.Replace(current, removed); err != nil {
		return false, err
	}
	return true, journal.Delete(removed)
}

func promoteLiveCleanupStageTicket(
	journal LiveCleanupJournal,
	ticket checkpointmodel.LiveCleanupTicket,
) (checkpointmodel.LiveCleanupTicket, error) {
	if ticket.State() != checkpointmodel.LiveCleanupTicketCommitted {
		return ticket, nil
	}
	next, err := checkpointmodel.ReduceLiveCleanupTicket(
		ticket, checkpointmodel.LiveCleanupRecordStageCreated,
	)
	if err != nil {
		return checkpointmodel.LiveCleanupTicket{}, err
	}
	if err := journal.Replace(ticket, next); err != nil {
		return checkpointmodel.LiveCleanupTicket{}, err
	}
	return next, nil
}

func openExactLiveCleanupStage(
	proof outputcap.Directory,
	ticket checkpointmodel.LiveCleanupTicket,
) (outputcap.MutableFile, bool, error) {
	stage, err := proof.OpenMutableFile(ticket.StageName(), false)
	if err != nil || stage == nil {
		if errors.Is(err, outputcap.ErrUnsafeNamespace) || errors.Is(err, fs.ErrNotExist) {
			return nil, false, closeFile(stage)
		}
		return nil, false, errors.Join(err, closeFile(stage))
	}
	size, err := stage.Size()
	if err != nil {
		if errors.Is(err, outputcap.ErrUnsafeNamespace) {
			return nil, false, stage.Close()
		}
		return nil, false, errors.Join(err, stage.Close())
	}
	if size != ticket.ExactSize() {
		return nil, false, stage.Close()
	}
	return stage, true, nil
}

func closeFile(file outputcap.FileIdentity) error {
	if file == nil {
		return nil
	}
	return file.Close()
}
