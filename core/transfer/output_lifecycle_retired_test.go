package transfer

import (
	"testing"

	"github.com/windshare/windshare/core/catalog"
	"github.com/windshare/windshare/core/content"
)

func TestFileStartAcceptsDeterministicallyRecoveredRetirement(t *testing.T) {
	binding := retiredStartTestBinding(t)
	settlement, err := NewRetiredFileSettlement(binding)
	if err != nil {
		t.Fatal(err)
	}
	start, err := NewFileSettlementStart(settlement)
	if err != nil {
		t.Fatal(err)
	}
	if transaction, _, ok := start.Transaction(); ok || transaction != nil {
		t.Fatal("recovered retirement exposed a second content transaction")
	}
	immediate, ok := start.ImmediateSettlement()
	actualBinding, bound := immediate.MaterializedBinding()
	if !ok || immediate.Kind() != FileRetired || !bound || actualBinding != binding {
		t.Fatalf("immediate settlement = (%v, %v), want target-bound FileRetired", immediate.Kind(), ok)
	}
}

func retiredStartTestBinding(t *testing.T) MaterializedFileBinding {
	t.Helper()
	identity16 := func(value byte) [catalog.IdentityBytes]byte {
		var identity [catalog.IdentityBytes]byte
		for index := range identity {
			identity[index] = value
		}
		return identity
	}
	geometry, err := content.NewFileGeometry(1, catalog.DefaultChunkSize)
	if err != nil {
		t.Fatal(err)
	}
	descriptor, err := content.NewFileRevisionDescriptor(
		catalog.ShareInstance(identity16(1)), catalog.FileID(identity16(2)),
		content.FileRevision(identity16(3)), geometry, catalog.ModifiedTime{},
	)
	if err != nil {
		t.Fatal(err)
	}
	locator, err := NewPathMaterializationLocator("file.bin")
	if err != nil {
		t.Fatal(err)
	}
	var session OutputSessionID
	var object OwnedObjectID
	for index := range session {
		session[index] = 4
	}
	for index := range object {
		object[index] = 5
	}
	binding, err := NewMaterializedFileBinding(session, descriptor, locator, object)
	if err != nil {
		t.Fatal(err)
	}
	return binding
}
