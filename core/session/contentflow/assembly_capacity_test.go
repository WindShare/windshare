package contentflow

import (
	"bytes"
	"errors"
	"testing"

	"github.com/windshare/windshare/core/session/protocolsession"
)

func TestReservedAssemblyKeepsSharedCapacityThroughAuthentication(t *testing.T) {
	const size = 128
	firstHierarchy, _ := reassemblyHierarchy(t, ReassemblyLimits{Bytes: size, Records: 1})
	secondHierarchy, _ := reassemblyHierarchy(t, ReassemblyLimits{Bytes: size, Records: 1})
	secondHierarchy.Process = firstHierarchy.Process
	first, _ := NewAssembler(flowID[protocolsession.ProtocolSessionID](1), firstHierarchy, nil)
	second, _ := NewAssembler(flowID[protocolsession.ProtocolSessionID](2), secondHierarchy, nil)
	defer first.Close()
	defer second.Close()

	capacity, _, err := first.TryReserve(size)
	if err != nil || capacity == nil {
		t.Fatalf("first admission: %v", err)
	}
	fragments, _ := FragmentRecord(flowID[protocolsession.OperationID](3), bytes.Repeat([]byte{7}, size))
	result, err := capacity.AcceptAuthenticated(fragments[0].Body())
	if err != nil || result.Status != RecordComplete || len(result.Object) != size {
		t.Fatalf("assembly: %+v %v", result, err)
	}
	waiting, changed, err := second.TryReserve(size)
	if waiting != nil || changed == nil || err != nil {
		t.Fatalf("shared contention did not wait: %v %v", waiting, err)
	}
	if err := first.CompleteOperation(result.OperationID); err != nil {
		t.Fatal(err)
	}
	first.Close()
	select {
	case <-changed:
		t.Fatal("receipt or receiver close released another owner's authentication buffer")
	default:
	}
	if firstHierarchy.Process.Usage().Bytes != size {
		t.Fatal("complete object was no longer charged")
	}
	capacity.Release()
	capacity.Release()
	select {
	case <-changed:
	default:
		t.Fatal("physical settlement did not wake another receiver")
	}
	waiting, _, err = second.TryReserve(size)
	if err != nil || waiting == nil {
		t.Fatalf("sibling admission after close: %v", err)
	}
	waiting.Release()
	for _, account := range []*ReassemblyAccount{firstHierarchy.Process, firstHierarchy.Share, firstHierarchy.Session, secondHierarchy.Share, secondHierarchy.Session} {
		if err := account.Close(); err != nil {
			t.Error(err)
		}
	}
}

func TestReservedAssemblyRejectsCapacityAndRecordSubstitution(t *testing.T) {
	hierarchy, _ := reassemblyHierarchy(t, ReassemblyLimits{Bytes: 128, Records: 1})
	assembler, _ := NewAssembler(flowID[protocolsession.ProtocolSessionID](1), hierarchy, nil)
	defer assembler.Close()
	if held, changed, err := assembler.TryReserve(129); held != nil || changed != nil || !errors.Is(err, ErrReassemblyBudget) {
		t.Fatalf("impossible admission: %v", err)
	}
	capacity, _, _ := assembler.TryReserve(64)
	oversized, _ := FragmentRecord(flowID[protocolsession.OperationID](2), make([]byte, 65))
	if _, err := capacity.AcceptAuthenticated(oversized[0].Body()); !errors.Is(err, ErrReassemblyBudget) {
		t.Fatalf("oversized: %v", err)
	}
	fragments, _ := FragmentRecord(flowID[protocolsession.OperationID](2), make([]byte, 64))
	if _, err := capacity.AcceptAuthenticated(fragments[0].Body()); err != nil {
		t.Fatal(err)
	}
	other, _ := FragmentRecord(flowID[protocolsession.OperationID](3), make([]byte, 64))
	if _, err := capacity.AcceptAuthenticated(other[0].Body()); !errors.Is(err, ErrFragmentConflict) {
		t.Fatalf("substitution: %v", err)
	}
	if err := hierarchy.Process.Close(); err == nil {
		t.Fatal("owner shutdown hid retained response")
	}
	capacity.Release()
	if _, _, err := assembler.TryReserve(64); !errors.Is(err, ErrReassemblyClosed) {
		t.Fatalf("closed capacity: %v", err)
	}
}
