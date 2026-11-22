package contentflow

import (
	"encoding/binary"
	"errors"
	"testing"
	"time"

	"github.com/windshare/windshare/core/session/protocolsession"
)

// Sustained transfers at arbitrary throughput must never fail in Assembler
// after operations retire; cross-operation retention and backpressure belong
// solely to protocolsession.OperationTable.
func TestAssemblerSustainsBlocksBeyondOperationTableRetentionAtFrozenClock(t *testing.T) {
	hierarchy, accounts := reassemblyHierarchy(t, ReassemblyLimits{Bytes: 1 << 20, Records: 4})
	now := time.Unix(4_000, 0)
	assembler, err := NewAssembler(flowID[protocolsession.ProtocolSessionID](1), hierarchy, func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(assembler.Close)

	const blocks = 10_000
	for index := range blocks {
		operation := sequentialOperationID(t, uint64(index)+1)
		fragments, err := FragmentRecord(operation, binary.BigEndian.AppendUint64(nil, uint64(index)+1))
		if err != nil {
			t.Fatal(err)
		}
		result, err := assembler.AcceptAuthenticated(flowMessagePlaintext(t, fragments[0]))
		if err != nil || result.Status != RecordComplete {
			t.Fatalf("block %d of %d: status=%v err=%v", index, blocks, result.Status, err)
		}
		if err := assembler.RetireOperation(operation); err != nil {
			t.Fatalf("block %d of %d: retiring operation: %v", index, blocks, err)
		}
	}
	if assembler.ActiveRecords() != 0 || len(assembler.completedRecords) != 0 || len(assembler.operationBytes) != 0 {
		t.Fatalf("retained state after %d retired blocks: records=%d completed=%d bytes=%d",
			blocks, assembler.ActiveRecords(), len(assembler.completedRecords), len(assembler.operationBytes))
	}
	for _, account := range accounts {
		if usage := account.Usage(); usage != (ReassemblyUsage{}) {
			t.Fatalf("budget leaked after sustained blocks: %+v", usage)
		}
	}
}

func TestAssemblerSuppressesInOperationDuplicateUntilRetired(t *testing.T) {
	hierarchy, _ := reassemblyHierarchy(t, ReassemblyLimits{Bytes: 1 << 20, Records: 4})
	assembler, err := NewAssembler(flowID[protocolsession.ProtocolSessionID](1), hierarchy, func() time.Time { return time.Unix(4_000, 0) })
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(assembler.Close)

	operation := protocolsession.OperationID(201)
	fragments, err := FragmentRecord(operation, []byte("block"))
	if err != nil {
		t.Fatal(err)
	}
	fragment := flowMessagePlaintext(t, fragments[0])
	if result, err := assembler.AcceptAuthenticated(fragment); err != nil || result.Status != RecordComplete {
		t.Fatalf("status=%v err=%v", result.Status, err)
	}
	if len(assembler.completedRecords[operation]) != 1 {
		t.Fatalf("completed records=%d, want 1", len(assembler.completedRecords[operation]))
	}
	replay, err := assembler.AcceptAuthenticated(fragment)
	if err != nil || replay.Status != FragmentDuplicate || replay.ReceivedBytes != 0 {
		t.Fatalf("in-operation replay: %+v err=%v", replay, err)
	}
	conflicting, err := FragmentRecord(operation, []byte("different-length-block"))
	if err != nil {
		t.Fatal(err)
	}
	conflictingPlaintext := flowMessagePlaintext(t, conflicting[0])
	copy(conflictingPlaintext[12:28], fragment[12:28])
	if _, err := assembler.AcceptAuthenticated(conflictingPlaintext); !errors.Is(err, ErrFragmentConflict) {
		t.Fatalf("completed-record geometry conflict err=%v", err)
	}
	if len(assembler.completedRecords) != 0 {
		t.Fatalf("completed records retained after geometry conflict: %d", len(assembler.completedRecords))
	}
	if result, err := assembler.AcceptAuthenticated(fragment); err != nil || result.Status != RecordComplete {
		t.Fatalf("status=%v err=%v", result.Status, err)
	}
	if err := assembler.RetireOperation(operation); err != nil {
		t.Fatal(err)
	}
	if len(assembler.completedRecords) != 0 {
		t.Fatalf("completed records retained after retire: %d", len(assembler.completedRecords))
	}
}

// Reproduces the production TryReserve -> AcceptAuthenticated -> RetireOperation
// -> delayed AssemblyCapacity.Release flow past OperationTable's retention cap
// without advancing the clock, proving delayed releases cannot stall or fail
// newly admitted operations.
func TestReservedAssemblySustainsDelayedCapacityReleasePastRetentionCap(t *testing.T) {
	const blockSize = 64
	hierarchy, accounts := reassemblyHierarchy(t, ReassemblyLimits{Bytes: blockSize * 4, Records: 4})
	now := time.Unix(4_000, 0)
	assembler, err := NewAssembler(flowID[protocolsession.ProtocolSessionID](2), hierarchy, func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(assembler.Close)

	const blocks = 5_000
	var deferred *AssemblyCapacity
	for index := range blocks {
		capacity, _, err := assembler.TryReserve(blockSize)
		if err != nil || capacity == nil {
			t.Fatalf("block %d reserve: capacity=%v err=%v", index, capacity, err)
		}
		operation := sequentialOperationID(t, uint64(index)+1)
		payload := make([]byte, blockSize)
		binary.BigEndian.PutUint64(payload, uint64(index)+1)
		fragments, err := FragmentRecord(operation, payload)
		if err != nil {
			t.Fatal(err)
		}
		result, err := capacity.AcceptAuthenticated(fragments[0].Body())
		if err != nil || result.Status != RecordComplete {
			t.Fatalf("block %d accept: status=%v err=%v", index, result.Status, err)
		}
		if err := assembler.RetireOperation(operation); err != nil {
			t.Fatalf("block %d retire: %v", index, err)
		}
		// Release the previous attempt only after the next operation completes
		// and retires, matching delayed lane settlement in receiversession.
		if deferred != nil {
			deferred.Release()
		}
		deferred = capacity
	}
	if deferred != nil {
		deferred.Release()
	}
	if assembler.ActiveRecords() != 0 || len(assembler.completedRecords) != 0 {
		t.Fatalf("retained state: records=%d completed=%d", assembler.ActiveRecords(), len(assembler.completedRecords))
	}
	for _, account := range accounts {
		if usage := account.Usage(); usage != (ReassemblyUsage{}) {
			t.Fatalf("budget leaked: %+v", usage)
		}
	}
}

func sequentialOperationID(t *testing.T, sequence uint64) protocolsession.OperationID {
	t.Helper()
	id, err := protocolsession.OperationIDFromUint64(sequence)
	if err != nil {
		t.Fatal(err)
	}
	return id
}
