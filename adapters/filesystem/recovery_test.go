package filesystem

import (
	"context"
	"encoding/hex"
	"errors"
	"strings"
	"testing"

	"github.com/windshare/windshare/core/transfer/receivecontract"
	"github.com/windshare/windshare/engine/recovery"
)

func TestResumeInventoryRejectsAmbiguousOrdinalsAndInvalidAttention(t *testing.T) {
	operation := testResumeOperation("1", recovery.OperationIncomplete)
	if _, err := recovery.NewSnapshot([]recovery.Operation{operation, operation}, false); !errors.Is(err, recovery.ErrContract) {
		t.Fatalf("duplicate error=%v", err)
	}
	invalidAttention := testResumeOperation("2", recovery.OperationNeedsAttention)
	invalidAttention.Attention = "cleanup-uncertain"
	if _, err := recovery.NewSnapshot([]recovery.Operation{invalidAttention}, false); !errors.Is(err, recovery.ErrContract) {
		t.Fatalf("Attention error=%v", err)
	}
	unsorted := recovery.Snapshot{Operations: []recovery.Operation{
		testResumeOperation("2", recovery.OperationIncomplete),
		testResumeOperation("1", recovery.OperationIncomplete),
	}}
	if unsorted.Valid() {
		t.Fatal("unsorted ordinal snapshot was accepted")
	}
}

func TestInventoryAndDiscardFailClosedForDetachedValues(t *testing.T) {
	var detached *recovery.Inventory
	if _, err := detached.Snapshot(); !errors.Is(err, recovery.ErrContract) {
		t.Fatalf("detached snapshot error=%v", err)
	}
	result := detached.Discard(context.Background(), receivecontract.OperationID{})
	if !errors.Is(result.Err, recovery.ErrContract) || result.Successful() {
		t.Fatalf("detached discard=%+v", result)
	}
}

func testResumeOperation(fill string, state recovery.OperationState) recovery.Operation {
	decoded, _ := hex.DecodeString(strings.Repeat(fill, 32))
	id, _ := receivecontract.OperationIDFromBytes(decoded)
	return recovery.Operation{ID: id, State: state}
}
