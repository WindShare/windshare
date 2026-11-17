package eventproject

import (
	"errors"
	"fmt"
	"testing"

	transferfault "github.com/windshare/windshare/core/transfer/fault"
	"github.com/windshare/windshare/engine/internal/receive"
	appobs "github.com/windshare/windshare/engine/observation"
)

func TestOutputContentionDoesNotInventAnotherDownload(t *testing.T) {
	checkpoint, err := transferfault.NewCheckpoint(transferfault.ScopeOutputPause, transferfault.CheckpointBusy)
	if err != nil {
		t.Fatal(err)
	}
	file, err := transferfault.NewOutput(transferfault.ScopeFileLocal, transferfault.OutputFileAlreadyActive)
	if err != nil {
		t.Fatal(err)
	}
	storageBusy := transferfault.Wrap(checkpoint, errors.New("file is held by another program"))
	for _, test := range []struct {
		name  string
		cause error
		code  appobs.FailureCode
		key   appobs.SafeMessageKey
	}{
		{"matching operation", receive.Failure{Code: receive.FailureOutputOperationAlreadyRunning, Cause: storageBusy},
			appobs.FailureOutputOperationAlreadyRunning, appobs.MessageOutputOperationAlreadyRunning},
		{"checkpoint contention", receive.Failure{Cause: storageBusy},
			appobs.FailureCheckpointBusy, appobs.MessageOutputBusy},
		{"file transaction contention", receive.Failure{Fault: file},
			appobs.FailureOutputFileAlreadyActive, appobs.MessageOutputBusy},
	} {
		t.Run(test.name, func(t *testing.T) {
			for _, cause := range []error{test.cause, fmt.Errorf("receive admission: %w", test.cause), errors.Join(test.cause)} {
				projected, present := ClassifyError(cause)
				key, known := projected.MessageKey()
				if !present || !known || projected.Code() != test.code || key != test.key {
					t.Fatalf("contention lost its cause: %+v present=%t key=%v; want %v/%v", projected, present, key, test.code, test.key)
				}
			}
		})
	}
}

func TestMissingResumeOperationKeepsItsKnownFailureAcrossProjection(t *testing.T) {
	failure := receive.Failure{Code: receive.FailureRetainedOutputMissing, Cause: receive.ErrRetainedOutputMissing}
	for _, cause := range []error{
		failure,
		fmt.Errorf("receive admission: %w", failure),
		errors.Join(failure, receive.ErrRetainedOutputMissing),
	} {
		projected, present := ClassifyError(cause)
		key, known := projected.MessageKey()
		if !present || !known || projected.Code() != appobs.FailureRetainedOutputMissing || key != appobs.MessageRetainedOutputMissing {
			t.Fatalf("missing resume operation became an unknown error: %+v present=%t", projected, present)
		}
	}
}
