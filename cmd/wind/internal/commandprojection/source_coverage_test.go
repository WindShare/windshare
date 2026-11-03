package commandprojection

import (
	"testing"

	"github.com/windshare/windshare/cmd/wind/internal/clievent"
	"github.com/windshare/windshare/core/transfer"
	"github.com/windshare/windshare/engine"
	appobs "github.com/windshare/windshare/engine/observation"
)

func TestSourceOmissionsProduceMachineReadablePartialWithoutUndoingPublication(t *testing.T) {
	job := successfulJobResult(t)
	job.Progress.SourceOmittedEntries = 3
	completion, err := engine.SettleReceive(engine.ReceiveSettlementInput{Result: job, Destination: "downloads"})
	if err != nil {
		t.Fatal(err)
	}
	result, err := ProjectReceiveResult(completion)
	if err != nil {
		t.Fatal(err)
	}
	failure, present := result.Failure()
	if result.Status() != clievent.ResultPartial || result.ExitCode() != clievent.ExitFailure ||
		result.SourceOmittedEntries() != 3 || !present || failure.Code() != appobs.FailureSourceOmitted {
		t.Fatalf("incomplete selection claimed success: %+v", result)
	}
	if completion.Value.Transfer.Settlement.Kind() != transfer.DirectTreeSettlementSuccess ||
		completion.Value.Recovery.Retention != engine.ReceiveRetentionNone ||
		completion.Value.Recovery.Reason != engine.ReceiveRecoverySourceOmitted ||
		result.PublishedBytes() != job.Progress.PublishedBytes {
		t.Fatalf("source coverage changed durable output: %+v", completion)
	}
	if _, err := clievent.NewTransferResult(clievent.TransferResultSpec{
		Status: clievent.ResultSuccess, ExitCode: clievent.ExitSuccess,
		Destination: appobs.NewDisplayPath("downloads"), SourceOmittedEntries: 1,
	}); err == nil {
		t.Fatal("result contract accepted success with an incomplete selection")
	}
}
