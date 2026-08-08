package checkpointmodel

import "github.com/windshare/windshare/core/transfer"

// Binding is the repository capability for one transfer intent. It combines
// the certified checkpoint root with the intent digest so records cannot cross
// either repository boundary even when their filenames are attacker-controlled.
type Binding struct {
	ownership Ownership
	intent    transfer.TransferIntentDigest
}

func NewBinding(ownership Ownership, intent transfer.TransferIntentDigest) (Binding, error) {
	if !ownership.Valid() || intent.IsZero() {
		return Binding{}, ErrRecordBinding
	}
	return Binding{ownership: ownership, intent: intent}, nil
}

func (binding Binding) Ownership() Ownership {
	return binding.ownership
}

func (binding Binding) TransferIntentDigest() transfer.TransferIntentDigest {
	return binding.intent
}

func (binding Binding) Valid() bool {
	return binding.ownership.Valid() && !binding.intent.IsZero()
}

func (binding Binding) Matches(record Record, expectedID RecordID) bool {
	if !binding.Valid() || !record.Valid() || expectedID.IsZero() {
		return false
	}
	return record.TransferIntentDigest() == binding.intent &&
		record.BackendID() == binding.ownership.Backend() &&
		record.RootIdentity() == binding.ownership.RootIdentity() &&
		record.RecordID() == expectedID
}
