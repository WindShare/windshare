package checkpointmodel

import (
	"github.com/windshare/windshare/core/transfer"
	"github.com/windshare/windshare/core/transfer/receivecontract"
)

// Binding is the repository capability for one durable receive operation. All
// four identities are required because filesystem ownership alone cannot prove
// that a checkpoint belongs to a particular intent or materialization plan.
type Binding struct {
	ownership       Ownership
	operation       receivecontract.OperationID
	receiveIntent   transfer.ReceiveIntentDigest
	materialization receivecontract.BindingDigest
}

func NewBinding(
	ownership Ownership,
	operation receivecontract.OperationID,
	receiveIntent transfer.ReceiveIntentDigest,
	materialization receivecontract.BindingDigest,
) (Binding, error) {
	if !ownership.Valid() || operation.IsZero() || receiveIntent.IsZero() || materialization.IsZero() {
		return Binding{}, ErrRecordBinding
	}
	return Binding{
		ownership: ownership, operation: operation,
		receiveIntent: receiveIntent, materialization: materialization,
	}, nil
}

func (binding Binding) Ownership() Ownership                     { return binding.ownership }
func (binding Binding) OperationID() receivecontract.OperationID { return binding.operation }
func (binding Binding) ReceiveIntentDigest() transfer.ReceiveIntentDigest {
	return binding.receiveIntent
}
func (binding Binding) MaterializationBindingDigest() receivecontract.BindingDigest {
	return binding.materialization
}

func (binding Binding) Valid() bool {
	return binding.ownership.Valid() && !binding.operation.IsZero() &&
		!binding.receiveIntent.IsZero() && !binding.materialization.IsZero()
}

func (binding Binding) Matches(record Record, expectedID RecordID) bool {
	if !binding.Valid() || !record.Valid() || expectedID.IsZero() {
		return false
	}
	return record.OperationID() == binding.operation &&
		record.ReceiveIntentDigest() == binding.receiveIntent &&
		record.MaterializationBindingDigest() == binding.materialization &&
		record.MaterializerKind() == binding.ownership.MaterializerKind() &&
		record.AuthorityRef() == binding.ownership.AuthorityRef() &&
		record.RecordID() == expectedID
}
