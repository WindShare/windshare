package clievent

import (
	"testing"
	"time"
)

func TestProgressSnapshotEnforcesExactCounterRelationships(t *testing.T) {
	valid := ProgressSpec{
		DiscoveredFiles: 3, DiscoveredBytes: 100,
		PublishedFiles: 2, PublishedBytes: 70,
		VerifiedBytes: 90, NewlyVerifiedBytes: 60,
		FileOutcomes: FileOutcomes{DownloadedFiles: 1, ResumedFiles: 1},
		Discovery:    DiscoveryComplete, CountersExact: true,
	}
	snapshot, err := NewProgressSnapshot(valid)
	if err != nil || !snapshot.Valid() || snapshot.VerifiedBytes() != 90 || snapshot.PublishedBytes() != 70 {
		t.Fatalf("valid progress rejected: %+v err=%v", snapshot, err)
	}
	tests := []struct {
		name   string
		mutate func(*ProgressSpec)
	}{
		{"new exceeds verified", func(value *ProgressSpec) { value.NewlyVerifiedBytes = 91 }},
		{"verified exceeds discovered", func(value *ProgressSpec) { value.VerifiedBytes = 101 }},
		{"published exceeds verified", func(value *ProgressSpec) { value.PublishedBytes = 91 }},
		{"published files exceed discovered", func(value *ProgressSpec) { value.PublishedFiles = 4 }},
		{"settlement count disagrees", func(value *ProgressSpec) { value.FileOutcomes.ResumedFiles = 0 }},
		{"classified item blocks exceed total", func(value *ProgressSpec) {
			value.FileOutcomes.RevisionConflictFiles = 1
		}},
		{"invalid discovery", func(value *ProgressSpec) { value.Discovery = 0 }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			candidate := valid
			test.mutate(&candidate)
			if _, err := NewProgressSnapshot(candidate); err == nil {
				t.Fatalf("accepted invalid exact progress: %+v", candidate)
			}
		})
	}
	inexact := valid
	inexact.CountersExact = false
	inexact.VerifiedBytes = 101
	if _, err := NewProgressSnapshot(inexact); err != nil {
		t.Fatalf("inexact saturated snapshot rejected: %v", err)
	}
}

func TestProgressSnapshotKeepsCapacityWaitOutsideFileOutcomes(t *testing.T) {
	snapshot, err := NewProgressSnapshot(ProgressSpec{
		Discovery: DiscoveryComplete, CountersExact: true,
		CapacityActiveWaiters: 1, CapacityAccumulatedWait: 750 * time.Millisecond,
		CapacityWaitAttempts: 2, CapacityWaitVisible: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.CapacityActiveWaiters() != 1 || snapshot.CapacityAccumulatedWait() != 750*time.Millisecond ||
		snapshot.CapacityWaitAttempts() != 2 || !snapshot.CapacityWaitVisible() {
		t.Fatalf("capacity wait facts changed: %+v", snapshot)
	}
	if snapshot.FileOutcomes().HasNonSuccess() {
		t.Fatalf("capacity wait became a file outcome: %+v", snapshot.FileOutcomes())
	}

	invalid := []ProgressSpec{
		{Discovery: DiscoveryComplete, CapacityAccumulatedWait: -time.Millisecond},
		{Discovery: DiscoveryComplete, CapacityWaitVisible: true},
	}
	for _, spec := range invalid {
		if _, err := NewProgressSnapshot(spec); err == nil {
			t.Fatalf("accepted invalid capacity wait: %+v", spec)
		}
	}
}

func TestResultConstructorsFreezeExitAndDriftSemantics(t *testing.T) {
	destination := NewDisplayPath("C:/receiver/result")
	success, err := NewTransferResult(TransferResultSpec{
		Status: ResultSuccess, ExitCode: ExitSuccess, Drift: DriftNone,
		Elapsed: time.Second, Destination: destination, CountersExact: true,
	})
	if err != nil || !success.Valid() {
		t.Fatalf("valid success rejected: %+v err=%v", success, err)
	}
	driftFailure, _ := NewFailure(FailureSourceRevisionChanged)
	drifted, err := NewTransferResult(TransferResultSpec{
		Status: ResultPartial, ExitCode: ExitDrift, Drift: DriftSource,
		Elapsed: time.Second, Destination: destination, Failure: driftFailure,
	})
	if err != nil || !drifted.Valid() || drifted.ExitCode() != ExitDrift || drifted.Drift() != DriftSource {
		t.Fatalf("valid drift result rejected: %+v err=%v", drifted, err)
	}
	invalid := []TransferResultSpec{
		{Status: ResultSuccess, ExitCode: ExitFailure, Drift: DriftNone, Destination: destination},
		{Status: ResultFailed, ExitCode: ExitSuccess, Drift: DriftNone, Destination: destination},
		{Status: ResultPartial, ExitCode: ExitFailure, Drift: DriftSource, Destination: destination},
		{Status: ResultPartial, ExitCode: ExitDrift, Drift: DriftNone, Destination: destination},
		{Status: ResultPartial, ExitCode: ExitFailure, Drift: DriftNone, Destination: destination},
		{Status: ResultPartial, ExitCode: ExitFailure, Drift: DriftNone},
	}
	for _, candidate := range invalid {
		if _, err := NewTransferResult(candidate); err == nil {
			t.Fatalf("accepted contradictory transfer result: %+v", candidate)
		}
	}
}
