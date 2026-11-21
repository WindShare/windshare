package contentflow

import (
	"encoding/binary"
	"errors"
	"testing"
	"time"

	"github.com/windshare/windshare/core/content/records"
	"github.com/windshare/windshare/core/session/protocolsession"
)

// A healthy transfer completes and retires one block operation after another.
// Each must leave bounded late-fragment protection, but only as much as the
// table can sustain for the whole tombstone lifetime.
func TestAssemblerSustainsBlocksBeyondTheTombstoneTableWhenOperationsRetire(t *testing.T) {
	hierarchy, _ := reassemblyHierarchy(t, ReassemblyLimits{Bytes: 1 << 20, Records: 4})
	now := time.Unix(4_000, 0)
	assembler, err := NewAssembler(flowID[protocolsession.ProtocolSessionID](1), hierarchy, func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(assembler.Close)
	// The completions stay inside one tombstone lifetime, so none may expire.
	const blocks = MaxFragmentTombstones - 1
	step := FragmentTombstone / (blocks + 1)
	var lastOperation protocolsession.OperationID
	var lastFragment []byte
	for index := range blocks {
		var raw [16]byte
		binary.BigEndian.PutUint64(raw[8:], uint64(index)+1)
		operation, err := protocolsession.OperationIDFromBytes(raw[:])
		if err != nil {
			t.Fatal(err)
		}
		fragments, err := FragmentRecord(operation, binary.BigEndian.AppendUint64(nil, uint64(index)+1))
		if err != nil {
			t.Fatal(err)
		}
		result, err := assembler.AcceptAuthenticated(flowMessagePlaintext(t, fragments[0]))
		if err != nil || result.Status != RecordComplete {
			t.Fatalf("block %d of %d: status=%v err=%v", index, blocks, result.Status, err)
		}
		if err := assembler.CompleteOperation(operation); err != nil {
			t.Fatalf("block %d of %d: retiring its operation: %v", index, blocks, err)
		}
		lastOperation, lastFragment = operation, flowMessagePlaintext(t, fragments[0])
		now = now.Add(step)
	}
	late, err := assembler.AcceptAuthenticated(lastFragment)
	if err != nil || late.Status != FragmentTombstoned || late.OperationID != lastOperation {
		t.Fatalf("late fragment of a retired operation: %+v err=%v", late, err)
	}
}

func TestAssemblerRetiringAnOperationReleasesItsRecordTombstones(t *testing.T) {
	hierarchy, _ := reassemblyHierarchy(t, ReassemblyLimits{Bytes: 1 << 20, Records: 4})
	assembler, err := NewAssembler(flowID[protocolsession.ProtocolSessionID](1), hierarchy, func() time.Time { return time.Unix(4_000, 0) })
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(assembler.Close)
	operation := flowID[protocolsession.OperationID](201)
	fragments, err := FragmentRecord(operation, []byte("block"))
	if err != nil {
		t.Fatal(err)
	}
	fragment := flowMessagePlaintext(t, fragments[0])
	if result, err := assembler.AcceptAuthenticated(fragment); err != nil || result.Status != RecordComplete {
		t.Fatalf("status=%v err=%v", result.Status, err)
	}
	if len(assembler.recordTombstones) != 1 {
		t.Fatalf("completed record tombstones=%d, want 1", len(assembler.recordTombstones))
	}
	if err := assembler.CompleteOperation(operation); err != nil {
		t.Fatal(err)
	}
	if len(assembler.recordTombstones) != 0 || len(assembler.operationTombs) != 1 {
		t.Fatalf("record tombstones=%d operation tombstones=%d", len(assembler.recordTombstones), len(assembler.operationTombs))
	}
	late, err := assembler.AcceptAuthenticated(fragment)
	if err != nil || late.Status != FragmentTombstoned || late.OperationID != operation {
		t.Fatalf("late fragment: %+v err=%v", late, err)
	}
}

func TestAssemblerKeepsRecordTombstonesWhenTheTableCannotTakeTheOperationTombstone(t *testing.T) {
	hierarchy, _ := reassemblyHierarchy(t, ReassemblyLimits{Bytes: 1 << 20, Records: 4})
	now := time.Unix(4_000, 0)
	assembler, err := NewAssembler(flowID[protocolsession.ProtocolSessionID](1), hierarchy, func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(assembler.Close)
	operation := flowID[protocolsession.OperationID](200)
	fragments, err := FragmentRecord(operation, []byte("block"))
	if err != nil {
		t.Fatal(err)
	}
	if result, err := assembler.AcceptAuthenticated(flowMessagePlaintext(t, fragments[0])); err != nil || result.Status != RecordComplete {
		t.Fatalf("status=%v err=%v", result.Status, err)
	}
	// Other operations' tombstones fill the table even without this one's.
	for index := range MaxFragmentTombstones {
		assembler.recordTombstones[assemblyKey{
			operation: flowID[protocolsession.OperationID](byte(index%100 + 1)),
			record:    records.RecordID{byte(index / 250), byte(index%250 + 1)},
		}] = now.Add(time.Hour)
	}
	owned := len(assembler.recordTombstones)
	if err := assembler.CompleteOperation(operation); !errors.Is(err, ErrReassemblyBudget) {
		t.Fatalf("retire error=%v", err)
	}
	if len(assembler.recordTombstones) != owned || len(assembler.operationTombs) != 0 {
		t.Fatalf("a refused retire dropped protection: record tombstones=%d (was %d) operation tombstones=%d", len(assembler.recordTombstones), owned, len(assembler.operationTombs))
	}
	late, err := assembler.AcceptAuthenticated(flowMessagePlaintext(t, fragments[0]))
	if err != nil || late.Status != FragmentTombstoned {
		t.Fatalf("late fragment after refused retire: %+v err=%v", late, err)
	}
}
