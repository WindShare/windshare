package commandprojection

import (
	"github.com/windshare/windshare/cmd/wind/internal/clievent"
	"github.com/windshare/windshare/core/transfer"
	"github.com/windshare/windshare/engine"
	"time"
)

type GetResultInput struct {
	Result              transfer.JobResult
	AdmissionError      error
	RuntimeError        error
	ConnectionError     error
	ContextError        error
	Elapsed             time.Duration
	Destination         clievent.DisplayPath
	DestinationAdjusted bool
}

func ProjectGetResult(input GetResultInput) (clievent.TransferResult, error) {
	result, err := engine.SettleReceive(engine.ReceiveSettlementInput{Result: input.Result, AdmissionError: input.AdmissionError, RuntimeError: input.RuntimeError, ConnectionError: input.ConnectionError, ContextError: input.ContextError, Elapsed: input.Elapsed, Destination: input.Destination.Text(), DestinationAdjusted: input.DestinationAdjusted})
	if err != nil {
		return clievent.TransferResult{}, ErrInvalidProjection
	}
	return ProjectReceiveResult(result)
}
