package transfer

import "testing"

func TestJobRunAggregatesSettlementOwnedFileOutcomes(t *testing.T) {
	binding, checkpoint := outputLifecycleFixture(t)
	downloaded, err := NewVerifiedFileSettlement(FilePublished, checkpoint)
	if err != nil {
		t.Fatal(err)
	}
	downloaded, err = downloaded.WithMetadataWarnings([]FileMetadataWarning{FileMetadataModifiedTime})
	if err != nil {
		t.Fatal(err)
	}
	resumed, err := downloaded.WithPublicationProvenance(FileResumed)
	if err != nil {
		t.Fatal(err)
	}
	paused, err := NewVerifiedFileSettlement(FilePaused, checkpoint)
	if err != nil {
		t.Fatal(err)
	}
	collision, err := NewTransactionCollisionFileSettlement(binding)
	if err != nil {
		t.Fatal(err)
	}
	reference, err := NewMaterializationStateRef(binding.OutputSessionID(), binding.Locator().Digest())
	if err != nil {
		t.Fatal(err)
	}
	blocked, err := NewTransactionItemBlockedFileSettlement(binding, reference, ItemBlockOwnershipUnknown)
	if err != nil {
		t.Fatal(err)
	}
	failed, err := NewFailedFileSettlement(binding)
	if err != nil {
		t.Fatal(err)
	}

	run := &jobRun{}
	for _, settlement := range []FileSettlement{downloaded, resumed, paused, collision, blocked, failed} {
		run.acceptFileSettlement(settlement)
	}
	want := FileOutcomeSummary{
		DownloadedFiles: 1, ResumedFiles: 1, PausedFiles: 1,
		CollisionFiles: 1, FailedFiles: 1, ItemBlockedFiles: 1,
		ModifiedTimeWarnings: 2,
	}
	if run.fileOutcomes != want || run.fileOutcomes.PublishedFiles() != 2 {
		t.Fatalf("summary = %+v, want %+v", run.fileOutcomes, want)
	}
}
