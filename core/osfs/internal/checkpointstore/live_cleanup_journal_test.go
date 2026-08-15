package checkpointstore

import (
	"bytes"
	"errors"
	"testing"

	"github.com/windshare/windshare/core/osfs/internal/checkpointmodel"
	"github.com/windshare/windshare/core/osfs/internal/destinationauthority"
	"github.com/windshare/windshare/core/transfer"
)

func newTestLiveCleanupTicket(nonce byte, gen uint64, state checkpointmodel.LiveCleanupTicketState) checkpointmodel.LiveCleanupTicket {
	ticket, err := checkpointmodel.NewLiveCleanupTicket(checkpointmodel.LiveCleanupTicketSpec{
		Nonce:      bytes.Repeat([]byte{nonce}, checkpointmodel.LiveCleanupNonceBytesV1),
		ExactSize:  128,
		Profile:    checkpointmodel.LiveCleanupWindowsNTFSV1,
		Generation: gen,
		State:      state,
	})
	if err != nil {
		panic(err)
	}
	return ticket
}

func TestLiveCleanupJournalLifecycleAndOperations(t *testing.T) {
	// Nil control directory returns ErrInvalidOutputBinding
	if _, err := OpenLiveCleanupJournal(nil); !errors.Is(err, transfer.ErrInvalidOutputBinding) {
		t.Fatalf("nil control open error = %v, want ErrInvalidOutputBinding", err)
	}

	control := newMemoryDirectory()
	journal, err := OpenLiveCleanupJournal(control)
	if err != nil {
		t.Fatalf("open live cleanup journal: %v", err)
	}

	// Nil receiver methods fail safely
	var nilJournal *LiveCleanupJournal
	if err := nilJournal.Close(); err != nil {
		t.Fatalf("close nil journal: %v", err)
	}
	if _, err := nilJournal.Snapshot(10); !errors.Is(err, transfer.ErrInvalidOutputBinding) {
		t.Fatalf("snapshot nil journal error = %v", err)
	}
	if err := nilJournal.Create(newTestLiveCleanupTicket(1, 1, checkpointmodel.LiveCleanupTicketCommitted)); !errors.Is(err, transfer.ErrInvalidOutputBinding) {
		t.Fatalf("create nil journal error = %v", err)
	}
	if err := nilJournal.Replace(checkpointmodel.LiveCleanupTicket{}, checkpointmodel.LiveCleanupTicket{}); !errors.Is(err, transfer.ErrInvalidOutputBinding) {
		t.Fatalf("replace nil journal error = %v", err)
	}
	if err := nilJournal.Delete(checkpointmodel.LiveCleanupTicket{}); !errors.Is(err, transfer.ErrInvalidOutputBinding) {
		t.Fatalf("delete nil journal error = %v", err)
	}

	// Create valid ticket
	ticket1 := newTestLiveCleanupTicket(0x11, 1, checkpointmodel.LiveCleanupTicketCommitted)
	if err := journal.Create(ticket1); err != nil {
		t.Fatalf("create ticket1: %v", err)
	}
	// Create invalid ticket (generation != 1, invalid state, or zero ticket)
	if err := journal.Create(checkpointmodel.LiveCleanupTicket{}); !errors.Is(err, transfer.ErrInvalidOutputBinding) {
		t.Fatalf("create zero ticket error = %v", err)
	}
	if err := journal.Create(newTestLiveCleanupTicket(0x12, 2, checkpointmodel.LiveCleanupTicketCommitted)); !errors.Is(err, transfer.ErrInvalidOutputBinding) {
		t.Fatalf("create ticket with gen!=1 error = %v", err)
	}

	// Snapshot returns ticket1
	snapshot, err := journal.Snapshot(10)
	if err != nil || snapshot.State != destinationauthority.LiveCleanupScanComplete || len(snapshot.Tickets) != 1 {
		t.Fatalf("snapshot = (%+v, %v)", snapshot, err)
	}

	// Replace ticket1 -> stage created (generation 2)
	ticket1StageCreated := newTestLiveCleanupTicket(0x11, 2, checkpointmodel.LiveCleanupStageCreated)
	if err := journal.Replace(ticket1, ticket1StageCreated); err != nil {
		t.Fatalf("replace ticket1: %v", err)
	}

	// Replace invalid transition
	if err := journal.Replace(ticket1, ticket1StageCreated); err == nil {
		t.Fatal("replace with stale previous state succeeded")
	}
	if err := journal.Replace(ticket1StageCreated, newTestLiveCleanupTicket(0x11, 2, checkpointmodel.LiveCleanupStageCreated)); !errors.Is(err, transfer.ErrInvalidOutputBinding) {
		t.Fatal("replace with same generation succeeded")
	}

	// Snapshot reflects replaced ticket
	snapshot, err = journal.Snapshot(10)
	if err != nil || snapshot.State != destinationauthority.LiveCleanupScanComplete || len(snapshot.Tickets) != 1 ||
		snapshot.Tickets[0].State() != checkpointmodel.LiveCleanupStageCreated {
		t.Fatalf("snapshot after replace = (%+v, %v)", snapshot, err)
	}

	// Delete ticket1
	if err := journal.Delete(ticket1StageCreated); err != nil {
		t.Fatalf("delete ticket1: %v", err)
	}
	// Delete already removed ticket returns nil
	if err := journal.Delete(ticket1StageCreated); err != nil {
		t.Fatalf("repeat delete error = %v", err)
	}

	// Snapshot is now empty
	snapshot, err = journal.Snapshot(10)
	if err != nil || snapshot.State != destinationauthority.LiveCleanupScanComplete || len(snapshot.Tickets) != 0 {
		t.Fatalf("empty snapshot = (%+v, %v)", snapshot, err)
	}

	if err := journal.Close(); err != nil {
		t.Fatalf("close journal: %v", err)
	}
}

func TestLiveCleanupJournalSnapshotEdgeCases(t *testing.T) {
	control := newMemoryDirectory()
	journal, err := OpenLiveCleanupJournal(control)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = journal.Close() }()

	// Invalid max tickets
	if _, err := journal.Snapshot(0); !errors.Is(err, transfer.ErrInvalidOutputBinding) {
		t.Fatalf("snapshot with 0 max tickets error = %v", err)
	}

	// Overflow when tickets exceed maxTickets
	t1 := newTestLiveCleanupTicket(0x01, 1, checkpointmodel.LiveCleanupTicketCommitted)
	t2 := newTestLiveCleanupTicket(0x02, 1, checkpointmodel.LiveCleanupTicketCommitted)
	if err := journal.Create(t1); err != nil {
		t.Fatal(err)
	}
	if err := journal.Create(t2); err != nil {
		t.Fatal(err)
	}
	snapshot, err := journal.Snapshot(1)
	if err != nil || snapshot.State != destinationauthority.LiveCleanupScanOverflow {
		t.Fatalf("overflow snapshot = (%+v, %v)", snapshot, err)
	}

	// Invalid ticket name in tickets directory
	ticketsDir := journal.tickets.(*memoryDirectory)
	ticketsDir.files["invalid-ticket-name"] = &memoryFileData{bytes: []byte("invalid")}
	snapshot, err = journal.Snapshot(10)
	if err != nil || snapshot.State != destinationauthority.LiveCleanupScanUnknown {
		t.Fatalf("invalid name snapshot = (%+v, %v)", snapshot, err)
	}
	delete(ticketsDir.files, "invalid-ticket-name")

	// Corrupt content in ticket file
	corruptName := liveCleanupTicketName(newTestLiveCleanupTicket(0x03, 1, checkpointmodel.LiveCleanupTicketCommitted))
	ticketsDir.files[corruptName] = &memoryFileData{bytes: []byte("corrupt-bytes")}
	snapshot, err = journal.Snapshot(10)
	if err != nil || snapshot.State != destinationauthority.LiveCleanupScanUnknown {
		t.Fatalf("corrupt content snapshot = (%+v, %v)", snapshot, err)
	}
	delete(ticketsDir.files, corruptName)

	// Mismatched nonce between filename and ticket content
	mismatchedTicket := newTestLiveCleanupTicket(0x04, 1, checkpointmodel.LiveCleanupTicketCommitted)
	encoded, err := checkpointmodel.EncodeLiveCleanupTicket(mismatchedTicket)
	if err != nil {
		t.Fatal(err)
	}
	wrongName := liveCleanupTicketName(newTestLiveCleanupTicket(0x05, 1, checkpointmodel.LiveCleanupTicketCommitted))
	ticketsDir.files[wrongName] = &memoryFileData{bytes: encoded}
	snapshot, err = journal.Snapshot(10)
	if err != nil || snapshot.State != destinationauthority.LiveCleanupScanUnknown {
		t.Fatalf("mismatched name snapshot = (%+v, %v)", snapshot, err)
	}
	delete(ticketsDir.files, wrongName)
}

func TestLiveCleanupJournalHelpersAndValidators(t *testing.T) {
	// liveCleanupTicketName with invalid ticket
	if got := liveCleanupTicketName(checkpointmodel.LiveCleanupTicket{}); got != "" {
		t.Fatalf("zero ticket name = %q, want empty", got)
	}

	// validLiveCleanupTicketName
	validName := liveCleanupTicketName(newTestLiveCleanupTicket(0x01, 1, checkpointmodel.LiveCleanupTicketCommitted))
	if !validLiveCleanupTicketName(validName) {
		t.Fatalf("valid ticket name %q was rejected", validName)
	}
	for _, invalid := range []string{
		"",
		"ticket-short",
		"ticket-" + bytes.NewBuffer(bytes.Repeat([]byte("0"), 31)).String(),
		"ticket-" + bytes.NewBuffer(bytes.Repeat([]byte("0"), 33)).String(),
		"TICKET-" + bytes.NewBuffer(bytes.Repeat([]byte("0"), 32)).String(),
		"other-" + bytes.NewBuffer(bytes.Repeat([]byte("0"), 32)).String(),
		"ticket-" + bytes.NewBuffer(bytes.Repeat([]byte("g"), 32)).String(),
	} {
		if validLiveCleanupTicketName(invalid) {
			t.Errorf("invalid ticket name %q was accepted", invalid)
		}
	}

	// liveCleanupEventFor
	if got := liveCleanupEventFor(checkpointmodel.LiveCleanupStageCreated); got != checkpointmodel.LiveCleanupRecordStageCreated {
		t.Fatalf("stage created event = %v", got)
	}
	if got := liveCleanupEventFor(checkpointmodel.LiveCleanupStageRemoved); got != checkpointmodel.LiveCleanupRecordStageRemoved {
		t.Fatalf("stage removed event = %v", got)
	}
	if got := liveCleanupEventFor(checkpointmodel.LiveCleanupTicketCommitted); got != 0 {
		t.Fatalf("committed state event = %v, want 0", got)
	}
}
