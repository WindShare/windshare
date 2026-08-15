package destinationauthority

import (
	"errors"
	"testing"

	"github.com/windshare/windshare/core/osfs/internal/checkpointmodel"
	"github.com/windshare/windshare/core/osfs/internal/outputcap"
)

type cleanupJournalFake struct {
	snapshot   LiveCleanupSnapshot
	err        error
	replaceErr error
	deleteErr  error
	closeErr   error
	replaced   [][2]checkpointmodel.LiveCleanupTicket
	deleted    []checkpointmodel.LiveCleanupTicket
}

func (journal *cleanupJournalFake) Snapshot(int) (LiveCleanupSnapshot, error) {
	return journal.snapshot, journal.err
}
func (*cleanupJournalFake) Create(checkpointmodel.LiveCleanupTicket) error { return nil }
func (journal *cleanupJournalFake) Replace(previous, next checkpointmodel.LiveCleanupTicket) error {
	journal.replaced = append(journal.replaced, [2]checkpointmodel.LiveCleanupTicket{previous, next})
	if journal.replaceErr != nil {
		return journal.replaceErr
	}
	return journal.err
}
func (journal *cleanupJournalFake) Delete(ticket checkpointmodel.LiveCleanupTicket) error {
	journal.deleted = append(journal.deleted, ticket)
	if journal.deleteErr != nil {
		return journal.deleteErr
	}
	return journal.err
}
func (journal *cleanupJournalFake) Close() error { return journal.closeErr }

type cleanupProofDirectory struct {
	outputcap.Directory
	kind        outputcap.EntryKind
	exact       bool
	file        outputcap.File
	classifyErr error
	openErr     error
	removeErr   error
	removeCalls int
}

func (directory *cleanupProofDirectory) ClassifyExactEntry(string) (outputcap.EntryKind, bool, error) {
	return directory.kind, directory.exact, directory.classifyErr
}
func (directory *cleanupProofDirectory) OpenFile(string, bool, bool) (outputcap.File, error) {
	return directory.file, directory.openErr
}
func (directory *cleanupProofDirectory) RemoveLiveCleanupStage(
	checkpointmodel.LiveCleanupTicket,
	outputcap.File,
) error {
	directory.removeCalls++
	return directory.removeErr
}

type cleanupFile struct {
	outputcap.File
	size     uint64
	sizeErr  error
	closeErr error
	closed   int
}

func (file *cleanupFile) Size() (uint64, error) { return file.size, file.sizeErr }
func (file *cleanupFile) Close() error {
	file.closed++
	return file.closeErr
}

func TestCleanupReconciliationReducerCuts(t *testing.T) {
	committed := cleanupTicket(t, checkpointmodel.LiveCleanupTicketCommitted, 1)
	created, err := checkpointmodel.ReduceLiveCleanupTicket(committed, checkpointmodel.LiveCleanupRecordStageCreated)
	if err != nil {
		t.Fatal(err)
	}
	removed, err := checkpointmodel.ReduceLiveCleanupTicket(created, checkpointmodel.LiveCleanupRecordStageRemoved)
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name         string
		ticket       checkpointmodel.LiveCleanupTicket
		kind         outputcap.EntryKind
		exact        bool
		proved       bool
		replacements int
		deletes      int
		removes      int
	}{
		{"committed-absent", committed, outputcap.EntryAbsent, true, true, 0, 1, 0},
		{"created-absent", created, outputcap.EntryAbsent, true, true, 1, 1, 0},
		{"removed-absent", removed, outputcap.EntryAbsent, true, true, 0, 1, 0},
		{"committed-present", committed, outputcap.EntryRegularFile, true, true, 2, 1, 1},
		{"created-present", created, outputcap.EntryRegularFile, true, true, 1, 1, 1},
		{"removed-present", removed, outputcap.EntryRegularFile, true, false, 0, 0, 0},
		{"wrong-kind", created, outputcap.EntryDirectory, true, false, 0, 0, 0},
		{"inexact", created, outputcap.EntryRegularFile, false, false, 0, 0, 0},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			journal := &cleanupJournalFake{}
			file := &cleanupFile{size: test.ticket.ExactSize()}
			proof := &cleanupProofDirectory{kind: test.kind, exact: test.exact, file: file}
			proved, err := reconcileLiveCleanupTicket(journal, proof, test.ticket)
			if err != nil || proved != test.proved || len(journal.replaced) != test.replacements ||
				len(journal.deleted) != test.deletes || proof.removeCalls != test.removes {
				t.Fatalf("proved=%t err=%v replaced=%d deleted=%d removed=%d", proved, err,
					len(journal.replaced), len(journal.deleted), proof.removeCalls)
			}
		})
	}
}

func TestCleanupProofNegativeDowngradesOnlyCrashCleanup(t *testing.T) {
	supported := outputcap.SupportedCapability()
	capabilities, _ := outputcap.NewDestinationCapabilities(supported, supported, supported, supported)
	for _, test := range []struct {
		state  LiveCleanupScanState
		reason outputcap.CapabilityReason
	}{
		{LiveCleanupScanOverflow, outputcap.CapabilityReasonCleanupJournalOverflow},
		{LiveCleanupScanUnknown, outputcap.CapabilityReasonCleanupOwnershipUnknown},
	} {
		journal := &cleanupJournalFake{snapshot: LiveCleanupSnapshot{State: test.state}}
		result, err := reconcileLiveCleanup(
			journal, &cleanupProofDirectory{}, capabilities, checkpointmodel.LiveCleanupWindowsNTFSV1,
		)
		if err != nil || result.SafePublish() != supported || result.OperationRecovery() != supported ||
			result.RangeRecovery() != supported || result.CrashCleanup().Reason() != test.reason {
			t.Fatalf("result=%+v err=%v", result, err)
		}
		if _, err := outputcap.SelectExecutionMode(result); !errors.Is(err, outputcap.ErrOrdinaryOutputUnsupported) {
			t.Fatalf("mode error=%v", err)
		}
	}
}

type nonRemovingCleanupProof struct {
	outputcap.Directory
	kind  outputcap.EntryKind
	exact bool
	file  outputcap.File
}

func (proof nonRemovingCleanupProof) ClassifyExactEntry(string) (outputcap.EntryKind, bool, error) {
	return proof.kind, proof.exact, nil
}

func (proof nonRemovingCleanupProof) OpenFile(string, bool, bool) (outputcap.File, error) {
	return proof.file, nil
}

func TestCleanupJournalLifecycleAndCapabilityReducerFailures(t *testing.T) {
	if _, err := NewLiveCleanupJournalHandle(nil); !errors.Is(err, ErrInvalidConfiguration) {
		t.Fatalf("nil journal handle = %v", err)
	}
	var nilHandle *LiveCleanupJournalHandle
	if err := nilHandle.Close(); err != nil {
		t.Fatalf("nil journal close = %v", err)
	}
	closeFailure := errors.New("journal close failed")
	journal := &cleanupJournalFake{closeErr: closeFailure}
	handle, err := NewLiveCleanupJournalHandle(journal)
	if err != nil || !handle.valid() {
		t.Fatalf("journal handle = (%t, %v)", handle.valid(), err)
	}
	if err := handle.Close(); !errors.Is(err, closeFailure) || handle.valid() {
		t.Fatalf("journal close = (%t, %v)", handle.valid(), err)
	}
	if err := handle.Close(); err != nil {
		t.Fatalf("idempotent journal close = %v", err)
	}

	supported := outputcap.SupportedCapability()
	unsupported, err := outputcap.UnsupportedCapability(outputcap.CapabilityReasonUnverifiableCrashCleanup)
	if err != nil {
		t.Fatal(err)
	}
	noCleanup, err := outputcap.NewDestinationCapabilities(
		supported, supported, supported, unsupported,
	)
	if err != nil {
		t.Fatal(err)
	}
	if got, err := reconcileLiveCleanup(
		&cleanupJournalFake{err: errors.New("must not scan")},
		&cleanupProofDirectory{}, noCleanup, checkpointmodel.LiveCleanupWindowsNTFSV1,
	); err != nil || got != noCleanup {
		t.Fatalf("unsupported cleanup reconciliation = (%+v, %v)", got, err)
	}

	capabilities, _ := outputcap.NewDestinationCapabilities(supported, supported, supported, supported)
	snapshotFailure := errors.New("snapshot failed")
	if _, err := reconcileLiveCleanup(
		&cleanupJournalFake{err: snapshotFailure},
		&cleanupProofDirectory{}, capabilities, checkpointmodel.LiveCleanupWindowsNTFSV1,
	); !errors.Is(err, snapshotFailure) {
		t.Fatalf("snapshot failure = %v", err)
	}
	if _, err := reconcileLiveCleanup(
		&cleanupJournalFake{snapshot: LiveCleanupSnapshot{State: LiveCleanupScanState(0xff)}},
		&cleanupProofDirectory{}, capabilities, checkpointmodel.LiveCleanupWindowsNTFSV1,
	); !errors.Is(err, ErrInvalidConfiguration) {
		t.Fatalf("unknown scan state = %v", err)
	}
	for name, tickets := range map[string][]checkpointmodel.LiveCleanupTicket{
		"invalid-ticket": {{}},
		"wrong-profile":  {cleanupTicket(t, checkpointmodel.LiveCleanupTicketCommitted, 1)},
	} {
		t.Run(name, func(t *testing.T) {
			profile := checkpointmodel.LiveCleanupWindowsNTFSV1
			if name == "wrong-profile" {
				profile = checkpointmodel.LiveCleanupLinuxExt4V1
			}
			got, err := reconcileLiveCleanup(
				&cleanupJournalFake{snapshot: LiveCleanupSnapshot{
					State: LiveCleanupScanComplete, Tickets: tickets,
				}},
				&cleanupProofDirectory{}, capabilities, profile,
			)
			if err != nil || got.CrashCleanup().Reason() != outputcap.CapabilityReasonCleanupOwnershipUnknown {
				t.Fatalf("unsafe ticket = (%+v, %v)", got, err)
			}
		})
	}
	created := cleanupTicket(t, checkpointmodel.LiveCleanupStageCreated, 2)
	classifyFailure := errors.New("classification failed")
	if _, err := reconcileLiveCleanup(
		&cleanupJournalFake{snapshot: LiveCleanupSnapshot{State: LiveCleanupScanComplete, Tickets: []checkpointmodel.LiveCleanupTicket{created}}},
		&cleanupProofDirectory{classifyErr: classifyFailure},
		capabilities, checkpointmodel.LiveCleanupWindowsNTFSV1,
	); !errors.Is(err, classifyFailure) {
		t.Fatalf("ticket reconciliation failure = %v", err)
	}
	got, err := reconcileLiveCleanup(
		&cleanupJournalFake{snapshot: LiveCleanupSnapshot{State: LiveCleanupScanComplete, Tickets: []checkpointmodel.LiveCleanupTicket{created}}},
		&cleanupProofDirectory{kind: outputcap.EntryRegularFile, exact: false},
		capabilities, checkpointmodel.LiveCleanupWindowsNTFSV1,
	)
	if err != nil || got.CrashCleanup().Reason() != outputcap.CapabilityReasonCleanupOwnershipUnknown {
		t.Fatalf("unproved ticket = (%+v, %v)", got, err)
	}
}

func TestCleanupTicketFailuresNeverClaimOrDeleteForeignStage(t *testing.T) {
	created := cleanupTicket(t, checkpointmodel.LiveCleanupStageCreated, 2)
	failure := errors.New("native cleanup failed")
	cases := []struct {
		name    string
		journal *cleanupJournalFake
		proof   outputcap.Directory
	}{
		{"classify", &cleanupJournalFake{}, &cleanupProofDirectory{classifyErr: failure}},
		{"open-unsafe", &cleanupJournalFake{}, &cleanupProofDirectory{
			kind: outputcap.EntryRegularFile, exact: true, openErr: outputcap.ErrUnsafeNamespace,
		}},
		{"open-failure", &cleanupJournalFake{}, &cleanupProofDirectory{
			kind: outputcap.EntryRegularFile, exact: true, openErr: failure,
		}},
		{"size-unsafe", &cleanupJournalFake{}, &cleanupProofDirectory{
			kind: outputcap.EntryRegularFile, exact: true,
			file: &cleanupFile{sizeErr: outputcap.ErrUnsafeNamespace},
		}},
		{"size-failure", &cleanupJournalFake{}, &cleanupProofDirectory{
			kind: outputcap.EntryRegularFile, exact: true,
			file: &cleanupFile{sizeErr: failure},
		}},
		{"wrong-size", &cleanupJournalFake{}, &cleanupProofDirectory{
			kind: outputcap.EntryRegularFile, exact: true,
			file: &cleanupFile{size: created.ExactSize() + 1},
		}},
		{"missing-remover", &cleanupJournalFake{}, nonRemovingCleanupProof{
			kind: outputcap.EntryRegularFile, exact: true,
			file: &cleanupFile{size: created.ExactSize()},
		}},
		{"remove-unsafe", &cleanupJournalFake{}, &cleanupProofDirectory{
			kind: outputcap.EntryRegularFile, exact: true, removeErr: outputcap.ErrUnsafeNamespace,
			file: &cleanupFile{size: created.ExactSize()},
		}},
		{"remove-failure", &cleanupJournalFake{}, &cleanupProofDirectory{
			kind: outputcap.EntryRegularFile, exact: true, removeErr: failure,
			file: &cleanupFile{size: created.ExactSize()},
		}},
		{"stage-close", &cleanupJournalFake{}, &cleanupProofDirectory{
			kind: outputcap.EntryRegularFile, exact: true,
			file: &cleanupFile{size: created.ExactSize(), closeErr: failure},
		}},
		{"replace-removed", &cleanupJournalFake{replaceErr: failure}, &cleanupProofDirectory{
			kind: outputcap.EntryRegularFile, exact: true,
			file: &cleanupFile{size: created.ExactSize()},
		}},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			proved, err := reconcileLiveCleanupTicket(test.journal, test.proof, created)
			switch test.name {
			case "wrong-size", "open-unsafe", "size-unsafe", "remove-unsafe":
				if proved || err != nil {
					t.Fatalf("unowned stage = (%t, %v)", proved, err)
				}
			default:
				if proved || err == nil {
					t.Fatalf("unsafe cleanup = (%t, %v)", proved, err)
				}
			}
		})
	}
	deleteFailure := errors.New("journal delete failed")
	if proved, err := reconcileLiveCleanupTicket(
		&cleanupJournalFake{deleteErr: deleteFailure},
		&cleanupProofDirectory{kind: outputcap.EntryAbsent, exact: true},
		cleanupTicket(t, checkpointmodel.LiveCleanupTicketCommitted, 1),
	); !proved || !errors.Is(err, deleteFailure) {
		t.Fatalf("delete failure = (%t, %v)", proved, err)
	}
}

func cleanupTicket(t *testing.T, state checkpointmodel.LiveCleanupTicketState, generation uint64) checkpointmodel.LiveCleanupTicket {
	t.Helper()
	nonce := make([]byte, checkpointmodel.LiveCleanupNonceBytesV1)
	nonce[0] = 1
	ticket, err := checkpointmodel.NewLiveCleanupTicket(checkpointmodel.LiveCleanupTicketSpec{
		Nonce: nonce, ExactSize: 9, Profile: checkpointmodel.LiveCleanupWindowsNTFSV1,
		Generation: generation, State: state,
	})
	if err != nil {
		t.Fatal(err)
	}
	return ticket
}
