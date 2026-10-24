package liveshare

import (
	"errors"

	"github.com/windshare/windshare/core/session/contentflow"
	"github.com/windshare/windshare/core/transfer"
)

type ReceiveCapacityLimits struct {
	Reassembly     contentflow.ReassemblyLimits
	PlaintextBytes uint64
}

// ReceiveCapacity belongs to the application owner. Receivers borrow it across
// connection generations and retire only their own reservations and caches.
type ReceiveCapacity struct {
	reassembly *contentflow.ReassemblyAccount
	plaintext  *transfer.PlaintextBudget
}

func NewReceiveCapacity(limits ReceiveCapacityLimits) (*ReceiveCapacity, error) {
	if limits.Reassembly == (contentflow.ReassemblyLimits{}) {
		limits.Reassembly = contentflow.ReassemblyLimits{
			Bytes: contentflow.DefaultProcessReassemblyBytes, Records: contentflow.DefaultProcessReassemblyRecords,
		}
	}
	if limits.PlaintextBytes == 0 {
		limits.PlaintextBytes = transfer.DefaultProcessPlaintextBytes
	}
	reassembly, err := contentflow.NewReassemblyAccount("application-receive", limits.Reassembly)
	if err != nil {
		return nil, err
	}
	plaintext, err := transfer.NewPlaintextBudget(limits.PlaintextBytes)
	if err != nil {
		return nil, err
	}
	return &ReceiveCapacity{reassembly: reassembly, plaintext: plaintext}, nil
}

func (capacity *ReceiveCapacity) Close() error {
	if capacity == nil {
		return nil
	}
	return errors.Join(capacity.reassembly.Close(), capacity.plaintext.Close())
}
