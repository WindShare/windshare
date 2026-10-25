package transfer

import (
	"math"
	"testing"
	"time"

	"github.com/windshare/windshare/core/diagnostics"
	"github.com/windshare/windshare/core/session/protocolsession"
	"github.com/windshare/windshare/core/transfer/receivecontract"
	"github.com/windshare/windshare/core/transfer/revisionwait"
)

func TestDiagnosticCapacityWaitKeepsJobAndProtocolOwnership(t *testing.T) {
	source := TransferLifecycleTrace{Stage: TransferCapacityRetryScheduled, ReceiveOperationID: receivecontract.OperationID{1},
		ProtocolSessionID: protocolsession.ProtocolSessionID{2}, TransferJobID: TransferJobID{3},
		CapacityWaitID: revisionwait.WaitID{4}, CapacityGeneration: revisionwait.GenerationToken{5}, CapacityOperationID: protocolsession.OperationID{6},
		CapacityAttempt: 3, CapacityHint: 250 * time.Millisecond, CapacityJitter: 7 * time.Millisecond, CapacityDelay: 257 * time.Millisecond, CapacityAccumulated: 700 * time.Millisecond, CapacityActiveWaiters: 2,
		Progress: ReceiveProgressSnapshot{Discovery: DiscoveryComplete, CountersExact: true, PreviouslyPublishedBytes: math.MaxUint64, FileOutcomes: FileOutcomeSummary{PreviouslyPublishedFiles: 1}}}
	record := source.DiagnosticRecord()
	p := record.Payload()
	c := p.Get("capacity")
	if c.Get("wait_id").Text() != diagnostics.Identity(source.CapacityWaitID.Bytes()).Text() || c.Get("delay_ms").Uint() != 257 || c.Get("accumulated_wait_ms").Uint() != 700 ||
		record.Correlation().Get("protocol_operation_id").Text() != diagnostics.Identity(source.CapacityOperationID[:]).Text() ||
		record.Correlation().Get("transfer_job_id").Text() != diagnostics.Identity(source.TransferJobID.Bytes()).Text() {
		t.Fatal("capacity context lost")
	}
	progress := p.Get("progress")
	if progress.Get("previously_published_bytes").Uint() != math.MaxUint64 || progress.Get("verified_bytes").Uint() != 0 ||
		progress.Get("file_outcomes").Get("resumed_files").Uint() != 0 || progress.Get("file_outcomes").Get("previously_published_files").Uint() != 1 {
		t.Fatal("historical delivery was reclassified")
	}
	source.Progress.PreviouslyPublishedBytes = 0
	if progress.Get("previously_published_bytes").Uint() != math.MaxUint64 {
		t.Fatal("diagnostic borrowed mutable progress")
	}
}
