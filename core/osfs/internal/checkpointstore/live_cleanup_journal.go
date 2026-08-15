package checkpointstore

import (
	"encoding/hex"
	"errors"
	"io/fs"
	"slices"
	"strings"

	"github.com/windshare/windshare/core/osfs/internal/checkpointmodel"
	"github.com/windshare/windshare/core/osfs/internal/destinationauthority"
	"github.com/windshare/windshare/core/osfs/internal/outputcap"
	"github.com/windshare/windshare/core/transfer"
)

const liveCleanupTicketsDirectory = "tickets"

const liveCleanupTicketNamePrefix = "ticket-"

type LiveCleanupJournal struct {
	tickets outputcap.Directory
}

// OpenLiveCleanupJournal owns control/cleanup-v1/tickets. The cleanup-v1 parent
// remains the proof namespace for stage objects; separating its ticket child is
// why Snapshot never mistakes an ordinary-profile stage for unknown metadata.
func OpenLiveCleanupJournal(control outputcap.Directory) (journal LiveCleanupJournal, resultErr error) {
	if control == nil {
		return LiveCleanupJournal{}, transfer.ErrInvalidOutputBinding
	}
	proof, err := openOrCreateDirectory(control, checkpointmodel.LiveCleanupNamespaceV1)
	if err != nil {
		return LiveCleanupJournal{}, repositoryError("open live cleanup proof namespace", err)
	}
	defer func() {
		if resultErr != nil {
			resultErr = errors.Join(resultErr, proof.Close())
		}
	}()
	tickets, err := openOrCreateDirectory(proof, liveCleanupTicketsDirectory)
	if err != nil {
		return LiveCleanupJournal{}, repositoryError("open live cleanup ticket journal", err)
	}
	if err := proof.Close(); err != nil {
		return LiveCleanupJournal{}, repositoryError("close live cleanup proof namespace", errors.Join(err, tickets.Close()))
	}
	return LiveCleanupJournal{tickets: tickets}, nil
}

func (journal *LiveCleanupJournal) Close() error {
	if journal == nil {
		return nil
	}
	err := closeDirectory(journal.tickets)
	*journal = LiveCleanupJournal{}
	return repositoryError("close live cleanup journal", err)
}

func (journal *LiveCleanupJournal) Snapshot(
	maximumTickets int,
) (destinationauthority.LiveCleanupSnapshot, error) {
	if journal == nil || journal.tickets == nil || maximumTickets <= 0 {
		return destinationauthority.LiveCleanupSnapshot{}, transfer.ErrInvalidOutputBinding
	}
	// The maximum+1 request is the overflow witness. The capability reports a
	// bounded-namespace refusal as unsafe; other failures remain operational
	// errors because calling them overflow would hide a broken proof store.
	names, err := journal.tickets.Names(maximumTickets + 1)
	if err != nil {
		if errors.Is(err, outputcap.ErrUnsafeNamespace) {
			return destinationauthority.LiveCleanupSnapshot{State: destinationauthority.LiveCleanupScanOverflow}, nil
		}
		return destinationauthority.LiveCleanupSnapshot{}, repositoryError("snapshot live cleanup journal", err)
	}
	if len(names) > maximumTickets {
		return destinationauthority.LiveCleanupSnapshot{State: destinationauthority.LiveCleanupScanOverflow}, nil
	}
	slices.Sort(names)
	snapshot := destinationauthority.LiveCleanupSnapshot{State: destinationauthority.LiveCleanupScanComplete}
	seen := make(map[string]struct{}, len(names))
	for _, name := range names {
		if !validLiveCleanupTicketName(name) {
			return destinationauthority.LiveCleanupSnapshot{State: destinationauthority.LiveCleanupScanUnknown}, nil
		}
		encoded, readErr := ReadFile(journal.tickets, name)
		ticket, decodeErr := checkpointmodel.DecodeLiveCleanupTicket(encoded)
		if readErr != nil || decodeErr != nil || liveCleanupTicketName(ticket) != name {
			return destinationauthority.LiveCleanupSnapshot{State: destinationauthority.LiveCleanupScanUnknown}, nil
		}
		nonceKey := string(ticket.Nonce())
		if _, duplicate := seen[nonceKey]; duplicate {
			return destinationauthority.LiveCleanupSnapshot{State: destinationauthority.LiveCleanupScanUnknown}, nil
		}
		seen[nonceKey] = struct{}{}
		snapshot.Tickets = append(snapshot.Tickets, ticket)
	}
	return snapshot, nil
}

func (journal *LiveCleanupJournal) Create(ticket checkpointmodel.LiveCleanupTicket) error {
	if journal == nil || journal.tickets == nil || !ticket.Valid() ||
		ticket.Generation() != 1 || ticket.State() != checkpointmodel.LiveCleanupTicketCommitted {
		return transfer.ErrInvalidOutputBinding
	}
	encoded, err := checkpointmodel.EncodeLiveCleanupTicket(ticket)
	if err != nil {
		return codedError(ErrorCorruptRecord, "encode live cleanup ticket", err)
	}
	return repositoryError("create live cleanup ticket", InstallCreate(journal.tickets, liveCleanupTicketName(ticket), encoded))
}

func (journal *LiveCleanupJournal) Replace(
	previous checkpointmodel.LiveCleanupTicket,
	next checkpointmodel.LiveCleanupTicket,
) error {
	if journal == nil || journal.tickets == nil || !previous.Valid() || !next.Valid() ||
		!sameLiveCleanupIdentity(previous, next) || next.Generation() != previous.Generation()+1 {
		return transfer.ErrInvalidOutputBinding
	}
	expected, err := checkpointmodel.ReduceLiveCleanupTicket(previous, liveCleanupEventFor(next.State()))
	if err != nil || !sameLiveCleanupTicket(expected, next) {
		return codedError(ErrorUnsafeInstall, "validate live cleanup ticket transition", errors.Join(err, checkpointmodel.ErrInvalidLiveCleanupTicket))
	}
	previousBytes, previousErr := checkpointmodel.EncodeLiveCleanupTicket(previous)
	nextBytes, nextErr := checkpointmodel.EncodeLiveCleanupTicket(next)
	if previousErr != nil || nextErr != nil {
		return codedError(ErrorCorruptRecord, "encode live cleanup replacement", errors.Join(previousErr, nextErr))
	}
	return repositoryError("replace live cleanup ticket", InstallReplaceStrict(journal.tickets, liveCleanupTicketName(previous), previousBytes, nextBytes))
}

func (journal *LiveCleanupJournal) Delete(ticket checkpointmodel.LiveCleanupTicket) error {
	if journal == nil || journal.tickets == nil || !ticket.Valid() {
		return transfer.ErrInvalidOutputBinding
	}
	encoded, err := checkpointmodel.EncodeLiveCleanupTicket(ticket)
	if err != nil {
		return codedError(ErrorCorruptRecord, "encode deleted live cleanup ticket", err)
	}
	removeErr, closeErr := RemoveExact(journal.tickets, liveCleanupTicketName(ticket), encoded)
	if errors.Is(removeErr, fs.ErrNotExist) {
		removeErr = nil
	}
	return repositoryError("delete live cleanup ticket", errors.Join(removeErr, closeErr))
}

func liveCleanupTicketName(ticket checkpointmodel.LiveCleanupTicket) string {
	if !ticket.Valid() {
		return ""
	}
	return liveCleanupTicketNamePrefix + hex.EncodeToString(ticket.Nonce())
}

func validLiveCleanupTicketName(name string) bool {
	if len(name) != len(liveCleanupTicketNamePrefix)+checkpointmodel.LiveCleanupNonceBytesV1*2 ||
		!strings.HasPrefix(name, liveCleanupTicketNamePrefix) || name != strings.ToLower(name) {
		return false
	}
	_, err := hex.DecodeString(name[len(liveCleanupTicketNamePrefix):])
	return err == nil
}

func sameLiveCleanupIdentity(left, right checkpointmodel.LiveCleanupTicket) bool {
	return left.Valid() && right.Valid() && slices.Equal(left.Nonce(), right.Nonce()) &&
		left.ExactSize() == right.ExactSize() && left.Profile() == right.Profile()
}

func sameLiveCleanupTicket(left, right checkpointmodel.LiveCleanupTicket) bool {
	leftBytes, leftErr := checkpointmodel.EncodeLiveCleanupTicket(left)
	rightBytes, rightErr := checkpointmodel.EncodeLiveCleanupTicket(right)
	return leftErr == nil && rightErr == nil && slices.Equal(leftBytes, rightBytes)
}

func liveCleanupEventFor(state checkpointmodel.LiveCleanupTicketState) checkpointmodel.LiveCleanupEvent {
	switch state {
	case checkpointmodel.LiveCleanupStageCreated:
		return checkpointmodel.LiveCleanupRecordStageCreated
	case checkpointmodel.LiveCleanupStageRemoved:
		return checkpointmodel.LiveCleanupRecordStageRemoved
	default:
		return 0
	}
}

var _ destinationauthority.LiveCleanupJournal = (*LiveCleanupJournal)(nil)
