package transfer

import "github.com/windshare/windshare/core/catalog"

type TransferLifecycleStage uint8

const (
	TransferDiscoveryStarted TransferLifecycleStage = iota + 1
	TransferDiscoveryCompleted
	TransferSelectionFrozen
	TransferAdmissionStarted
	TransferAdmissionCompleted
	TransferFileStarted
	TransferFileSettled
	TransferJobSettled
)

// TransferLifecycleTrace is deliberately text-free. Stable typed decisions can
// be aggregated safely while the session and selection IDs reconstruct one run.
type TransferLifecycleTrace struct {
	Stage             TransferLifecycleStage
	OutputSessionID   OutputSessionID
	ResumeIntent      ResumeIntent
	SelectionIdentity SelectionIdentity
	FileID            catalog.FileID
	FileSettlement    FileSettlementKind
	JobSettlement     JobSettlementKind
	Failed            bool
}

type TransferLifecycleTracer interface {
	TraceTransferLifecycle(TransferLifecycleTrace)
}

type TransferLifecycleTraceFunc func(TransferLifecycleTrace)

func (function TransferLifecycleTraceFunc) TraceTransferLifecycle(event TransferLifecycleTrace) {
	if function != nil {
		function(event)
	}
}

func (j *TransferJob) trace(event TransferLifecycleTrace) {
	if j.tracer != nil {
		j.tracer.TraceTransferLifecycle(event)
	}
}
