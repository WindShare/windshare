package commandprojection

import (
	"context"
	"fmt"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/windshare/windshare/cmd/wind/internal/clievent"
	"github.com/windshare/windshare/core/transfer"
	transferfault "github.com/windshare/windshare/core/transfer/fault"
	"github.com/windshare/windshare/core/transfer/revisionwait"
)

func TestProgressProjectionPreservesAuthenticatedCounters(t *testing.T) {
	value := transfer.ReceiveProgressSnapshot{
		DiscoveredFiles: 5, DiscoveredBytes: 500,
		PublishedFiles: 2, PublishedBytes: 200,
		VerifiedBytes: 350, NewlyVerifiedBytes: 300,
		FileOutcomes: transfer.FileOutcomeSummary{
			DownloadedFiles: 1, ResumedFiles: 1, PausedFiles: 1,
			CollisionFiles: 1, ItemBlockedFiles: 3, FailedFiles: 1,
			RevisionConflictFiles: 1, CheckpointInvalidFiles: 1, OwnedObjectUnknownFiles: 1,
			ModifiedTimeWarnings: 2,
		},
		Discovery: transfer.DiscoveryComplete, CountersExact: true,
	}
	projected, err := ProjectProgress(value, false)
	if err != nil {
		t.Fatal(err)
	}
	files := projected.FileOutcomes()
	if projected.DiscoveredFiles() != 5 || projected.DiscoveredBytes() != 500 ||
		projected.PublishedFiles() != 2 || projected.PublishedBytes() != 200 ||
		projected.VerifiedBytes() != 350 || projected.NewlyVerifiedBytes() != 300 ||
		projected.Discovery() != clievent.DiscoveryComplete || !projected.CountersExact() ||
		files.DownloadedFiles != 1 || files.ResumedFiles != 1 || files.PausedFiles != 1 ||
		files.CollisionFiles != 1 || files.ItemBlockedFiles != 3 || files.FailedFiles != 1 ||
		files.RevisionConflictFiles != 1 || files.CheckpointInvalidFiles != 1 ||
		files.OwnedObjectUnknownFiles != 1 ||
		files.ModifiedTimeWarnings != 2 {
		t.Fatalf("progress projection lost facts: %+v files=%+v", projected, files)
	}
	value.Discovery = transfer.DiscoveryStatus(255)
	if _, err := ProjectProgress(value, false); err == nil {
		t.Fatal("projected unknown discovery enum")
	}
	value.Discovery = transfer.DiscoveryComplete
	value.VerifiedBytes = value.DiscoveredBytes + 1
	if _, err := ProjectProgress(value, false); err == nil {
		t.Fatal("projected contradictory exact progress")
	}
}

func TestProgressProjectionKeepsCapacityWaitSeparateFromFailureAccounting(t *testing.T) {
	value := transfer.ReceiveProgressSnapshot{
		Discovery: transfer.DiscoveryComplete, CountersExact: true,
		CapacityWait: revisionwait.Snapshot{
			ActiveWaiters: 1, AccumulatedWait: 750 * time.Millisecond, Attempts: 3,
		},
	}
	projected, err := ProjectProgress(value, true)
	if err != nil {
		t.Fatal(err)
	}
	if projected.CapacityActiveWaiters() != 1 || projected.CapacityAccumulatedWait() != 750*time.Millisecond ||
		projected.CapacityWaitAttempts() != 3 || !projected.CapacityWaitVisible() {
		t.Fatalf("capacity wait projection lost facts: %+v", projected)
	}
	if outcomes := projected.FileOutcomes(); outcomes.FailedFiles != 0 ||
		outcomes.PausedFiles != 0 || outcomes.HasNonSuccess() {
		t.Fatalf("capacity wait changed file accounting: %+v", outcomes)
	}

	value.CapacityWait.ActiveWaiters = 0
	if _, err := ProjectProgress(value, true); err == nil {
		t.Fatal("projected visible wait without an active waiter")
	}
}

func TestCapacityBudgetPauseProjectsAsResumableJobWithoutFileFailure(t *testing.T) {
	settlement, err := transfer.NewDirectTreeSettlement(transfer.DirectTreeSettlementPaused)
	if err != nil {
		t.Fatal(err)
	}
	faultValue, err := transferfault.NewSession(
		transferfault.ScopeOutputPause,
		transferfault.SessionResourceBudget,
	)
	if err != nil {
		t.Fatal(err)
	}
	result, err := ProjectGetResult(GetResultInput{
		Result: transfer.JobResult{
			Outcome: transfer.DirectTreeOutcomePaused, Settlement: settlement,
			TerminationFault: faultValue,
			Progress: transfer.ReceiveProgressSnapshot{
				Discovery: transfer.DiscoveryComplete, CountersExact: true,
				CapacityWait: revisionwait.Snapshot{
					AccumulatedWait: revisionwait.DefaultWaitBudget, Attempts: 4,
				},
			},
		},
		Destination: clievent.NewDisplayPath("C:/safe"),
	})
	if err != nil {
		t.Fatal(err)
	}
	failure, present := result.Failure()
	if result.Status() != clievent.ResultPaused || !present ||
		failure.Code() != clievent.FailureSessionResourceBudget {
		t.Fatalf("capacity pause projection = %+v failure=%+v present=%t", result, failure, present)
	}
	if files := result.Files(); files.FailedFiles != 0 || files.PausedFiles != 0 || files.HasNonSuccess() {
		t.Fatalf("job-level capacity pause became a file failure: %+v", files)
	}
}

func TestGetResultProjectionFreezesStatusAndExitPrecedence(t *testing.T) {
	destination := clievent.NewDisplayPath("C:/downloads/result")
	success := successfulJobResult(t)
	sourceDrift, err := transferfault.NewSource(transferfault.ScopeFileLocal, transferfault.SourceRevisionChanged)
	if err != nil {
		t.Fatal(err)
	}
	sessionTerminal, err := transferfault.NewSession(transferfault.ScopeSessionTerminal, transferfault.SessionTransport)
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name       string
		mutate     func(*GetResultInput)
		wantStatus clievent.ResultStatus
		wantExit   clievent.ExitCode
		wantDrift  clievent.DriftReason
	}{
		{"success", func(*GetResultInput) {}, clievent.ResultSuccess, clievent.ExitSuccess, clievent.DriftNone},
		{"nominal success invariant failure", func(input *GetResultInput) {
			input.Result.Progress.PublishedBytes--
		}, clievent.ResultFailed, clievent.ExitFailure, clievent.DriftNone},
		{"partial missing selection", func(input *GetResultInput) {
			input.Result.Outcome = transfer.DirectTreeOutcomePartial
			input.Result.SelectionResolutionFailure = fmt.Errorf("selection wrapper: %w", transfer.ErrSelectionTargetMissing)
		}, clievent.ResultPartial, clievent.ExitUsage, clievent.DriftNone},
		{"exact missing selection outranks caller cancel", func(input *GetResultInput) {
			input.Result.Outcome = transfer.DirectTreeOutcomePartial
			input.Result.SelectionResolutionFailure = transfer.ErrSelectionTargetMissing
			input.ContextError = context.Canceled
		}, clievent.ResultPartial, clievent.ExitUsage, clievent.DriftNone},
		{"inexact complete discovery does not prove missing selection", func(input *GetResultInput) {
			input.Result.Outcome = transfer.DirectTreeOutcomePartial
			input.Result.SelectionResolutionFailure = fmt.Errorf("selection wrapper: %w", transfer.ErrSelectionTargetMissing)
			input.Result.Progress.CountersExact = false
		}, clievent.ResultPartial, clievent.ExitFailure, clievent.DriftNone},
		{"failed discovery does not prove missing selection", func(input *GetResultInput) {
			input.Result.Outcome = transfer.DirectTreeOutcomePartial
			input.Result.SelectionResolutionFailure = transfer.ErrSelectionTargetMissing
			input.Result.Progress.Discovery = transfer.DiscoveryFailed
		}, clievent.ResultPartial, clievent.ExitFailure, clievent.DriftNone},
		{"open discovery does not prove missing selection", func(input *GetResultInput) {
			input.Result.Outcome = transfer.DirectTreeOutcomePartial
			input.Result.SelectionResolutionFailure = transfer.ErrSelectionTargetMissing
			input.Result.Progress.Discovery = transfer.DiscoveryOpen
		}, clievent.ResultPartial, clievent.ExitFailure, clievent.DriftNone},
		{"drift outranks exact missing selection network and cancel", func(input *GetResultInput) {
			input.Result.Outcome = transfer.DirectTreeOutcomePartial
			input.Result.SelectionResolutionFailure = transfer.ErrSelectionTargetMissing
			input.Result.SourceDriftFault = sourceDrift
			input.RuntimeError = opaqueCanaryError{"relay-token-canary"}
			input.ContextError = context.Canceled
		}, clievent.ResultPartial, clievent.ExitDrift, clievent.DriftSource},
		{"drift outranks network and cancel", func(input *GetResultInput) {
			input.Result.Outcome = transfer.DirectTreeOutcomePaused
			input.Result.SourceDriftFault = sourceDrift
			input.Result.SourceDriftFailure = opaqueCanaryError{"catalog/path/canary"}
			input.RuntimeError = opaqueCanaryError{"relay-token-canary"}
			input.ContextError = context.Canceled
		}, clievent.ResultPaused, clievent.ExitDrift, clievent.DriftSource},
		{"session fault is network", func(input *GetResultInput) {
			input.Result.Outcome = transfer.DirectTreeOutcomePaused
			input.Result.TerminationFault = sessionTerminal
		}, clievent.ResultPaused, clievent.ExitNetwork, clievent.DriftNone},
		{"runtime failure outranks cancel", func(input *GetResultInput) {
			input.Result.Outcome = transfer.DirectTreeOutcomeFailed
			input.RuntimeError = opaqueCanaryError{"provider-url-canary"}
			input.ContextError = context.Canceled
		}, clievent.ResultFailed, clievent.ExitNetwork, clievent.DriftNone},
		{"caller cancel is local exit", func(input *GetResultInput) {
			input.Result.Outcome = transfer.DirectTreeOutcomePaused
			input.ContextError = context.Canceled
		}, clievent.ResultPaused, clievent.ExitFailure, clievent.DriftNone},
		{"ordinary partial remains local", func(input *GetResultInput) {
			input.Result.Outcome = transfer.DirectTreeOutcomePartial
		}, clievent.ResultPartial, clievent.ExitFailure, clievent.DriftNone},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			input := GetResultInput{Result: success, Destination: destination, Elapsed: 3 * time.Second}
			test.mutate(&input)
			result, err := ProjectGetResult(input)
			if err != nil {
				t.Fatal(err)
			}
			if result.Status() != test.wantStatus || result.ExitCode() != test.wantExit || result.Drift() != test.wantDrift {
				t.Fatalf("result status/exit/drift = %d/%d/%d want %d/%d/%d", result.Status(), result.ExitCode(), result.Drift(), test.wantStatus, test.wantExit, test.wantDrift)
			}
			if test.wantStatus == clievent.ResultSuccess {
				if _, present := result.Failure(); present {
					t.Fatal("successful result retained failure")
				}
			} else if failure, present := result.Failure(); !present || !failure.Valid() {
				t.Fatalf("unsuccessful result missing safe failure: %+v,%t", failure, present)
			}
		})
	}
}

func TestGetResultProjectsClosedCallerCancellationBeforeCleanupDiagnostics(t *testing.T) {
	result := successfulJobResult(t)
	result.Outcome = transfer.DirectTreeOutcomePaused
	result.TerminationCause = opaqueCanaryError{"private cancellation carrier"}
	result.TerminationInterruption = transfer.TransferInterruptionCanceled
	projected, err := ProjectGetResult(GetResultInput{
		Result: result, RuntimeError: opaqueCanaryError{"late peer cleanup residue"},
		ContextError: context.Canceled, Destination: clievent.NewDisplayPath("C:/safe"),
	})
	if err != nil {
		t.Fatal(err)
	}
	failure, present := projected.Failure()
	if !present || failure.Code() != clievent.FailureCanceled || projected.ExitCode() != clievent.ExitFailure {
		t.Fatalf("canceled result=%+v failure=%+v present=%t", projected, failure, present)
	}
}

func TestGetResultProjectsClosedSettlementDeadline(t *testing.T) {
	result := successfulJobResult(t)
	result.Outcome = transfer.DirectTreeOutcomePaused
	result.SettlementFailure = opaqueCanaryError{"private settlement carrier"}
	result.SettlementInterruption = transfer.TransferInterruptionDeadline
	projected, err := ProjectGetResult(GetResultInput{
		Result: result, Destination: clievent.NewDisplayPath("C:/safe"),
	})
	if err != nil {
		t.Fatal(err)
	}
	failure, present := projected.Failure()
	if !present || failure.Code() != clievent.FailureDeadline || projected.ExitCode() != clievent.ExitFailure {
		t.Fatalf("deadline result=%+v failure=%+v present=%t", projected, failure, present)
	}
}

func TestGetResultProjectsAuthoritativeLocalOutcomesWithoutUnexpectedFallback(t *testing.T) {
	tests := []struct {
		name     string
		outcomes transfer.FileOutcomeSummary
		want     clievent.FailureCode
	}{
		{"revision conflict", transfer.FileOutcomeSummary{ItemBlockedFiles: 1, RevisionConflictFiles: 1}, clievent.FailureCheckpointRevisionConflict},
		{"invalid checkpoint", transfer.FileOutcomeSummary{ItemBlockedFiles: 1, CheckpointInvalidFiles: 1}, clievent.FailureCheckpointInvalid},
		{"owned object conflict", transfer.FileOutcomeSummary{ItemBlockedFiles: 1, OwnedObjectUnknownFiles: 1}, clievent.FailureOwnedObjectUnknown},
		{"destination collision", transfer.FileOutcomeSummary{CollisionFiles: 1}, clievent.FailureDestinationCollision},
		{
			"mixed local priority",
			transfer.FileOutcomeSummary{
				ItemBlockedFiles: 3, RevisionConflictFiles: 1,
				CheckpointInvalidFiles: 1, OwnedObjectUnknownFiles: 1, CollisionFiles: 1,
			},
			clievent.FailureCheckpointRevisionConflict,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			job := successfulJobResult(t)
			job.Outcome = transfer.DirectTreeOutcomePartial
			job.Progress.PublishedFiles = 0
			job.Progress.PublishedBytes = 0
			job.Progress.VerifiedBytes = 0
			job.Progress.FileOutcomes = test.outcomes
			job.SucceededFiles = 0
			projected, err := ProjectGetResult(GetResultInput{
				Result: job, Destination: clievent.NewDisplayPath("C:/safe"),
			})
			if err != nil {
				t.Fatal(err)
			}
			failure, ok := projected.Failure()
			if !ok || failure.Code() != test.want || failure.Code() == clievent.FailureUnexpected {
				t.Fatalf("failure = %+v, present=%t, want %v", failure, ok, test.want)
			}
		})
	}
}

func TestGetResultUsesSettlementOwnedCountsAndDropsDiagnosticPathsAndCauses(t *testing.T) {
	const catalogPath = "catalog/path/SECRET-FILENAME"
	const providerSecret = "wss://relay/private?token=RESULT-TOKEN"
	result := successfulJobResult(t)
	result.Outcome = transfer.DirectTreeOutcomePartial
	result.Progress.CountersExact = false
	result.Progress.FileOutcomes = transfer.FileOutcomeSummary{
		DownloadedFiles: 2, ResumedFiles: 3, PausedFiles: 4,
		CollisionFiles: 5, ItemBlockedFiles: 6, FailedFiles: 7,
		ModifiedTimeWarnings: 8,
	}
	result.Directories = []transfer.DirectoryJobFailure{{Path: catalogPath, Cause: opaqueCanaryError{providerSecret}}}
	result.Files = []transfer.FileJobFailure{
		{Path: catalogPath, Cause: opaqueCanaryError{providerSecret}},
		{Path: catalogPath + "/second", Cause: opaqueCanaryError{providerSecret}},
	}
	result.OmittedDirectoryFailures = math.MaxUint64
	result.OmittedFileFailures = 9
	projected, err := ProjectGetResult(GetResultInput{
		Result: result, Destination: clievent.NewDisplayPath("C:/safe-human-destination"),
	})
	if err != nil {
		t.Fatal(err)
	}
	files := projected.Files()
	if files.DownloadedFiles != 2 || files.ResumedFiles != 3 || files.PausedFiles != 4 ||
		files.CollisionFiles != 5 || files.ItemBlockedFiles != 6 || files.FailedFiles != 7 ||
		files.ModifiedTimeWarnings != 8 {
		t.Fatalf("settlement-owned outcomes changed: %+v", files)
	}
	if projected.DirectoryFailures() != math.MaxUint64 || projected.OmittedDiagnostics() != math.MaxUint64 {
		t.Fatalf("bounded counts wrapped: directories=%d omitted=%d", projected.DirectoryFailures(), projected.OmittedDiagnostics())
	}
	encoded := fmt.Sprintf("%#v", projected)
	for _, forbidden := range []string{catalogPath, "SECRET-FILENAME", providerSecret, "RESULT-TOKEN"} {
		if strings.Contains(encoded, forbidden) {
			t.Fatalf("projected result retained forbidden value %q: %s", forbidden, encoded)
		}
	}
}

func TestShareAndCommandFailureProjectionNeverDeriveExitFromRenderer(t *testing.T) {
	clean, err := ProjectShareResult(ShareResultInput{Clean: true, Elapsed: time.Second})
	if err != nil || clean.ExitCode() != clievent.ExitSuccess || !clean.StoppedCleanly() {
		t.Fatalf("clean share result = %+v err=%v", clean, err)
	}
	failed, err := ProjectShareResult(ShareResultInput{
		Failure: opaqueCanaryError{"provider-secret"}, FailureClass: ShareFailureNetwork,
	})
	if err != nil || failed.ExitCode() != clievent.ExitNetwork || failed.StoppedCleanly() {
		t.Fatalf("failed share result = %+v err=%v", failed, err)
	}
	command, err := ProjectCommandFailure(clievent.CommandShare, clievent.ExitFailure, nil)
	if err != nil || command.ExitCode() != clievent.ExitFailure || command.Failure().Code() != clievent.FailureUnexpected {
		t.Fatalf("command failure = %+v err=%v", command, err)
	}
}

func successfulJobResult(t *testing.T) transfer.JobResult {
	t.Helper()
	settlement, err := transfer.NewDirectTreeSettlement(transfer.DirectTreeSettlementSuccess)
	if err != nil {
		t.Fatal(err)
	}
	return transfer.JobResult{
		Outcome: transfer.DirectTreeOutcomeSuccess, Settlement: settlement,
		SucceededFiles: 2,
		Progress: transfer.ReceiveProgressSnapshot{
			DiscoveredFiles: 2, DiscoveredBytes: 100,
			PublishedFiles: 2, PublishedBytes: 100,
			VerifiedBytes: 100, NewlyVerifiedBytes: 60,
			FileOutcomes: transfer.FileOutcomeSummary{DownloadedFiles: 1, ResumedFiles: 1},
			Discovery:    transfer.DiscoveryComplete, CountersExact: true,
		},
	}
}
