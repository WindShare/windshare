package resumestate

import (
	"errors"
	"strings"
	"testing"
)

func TestFileStateNamespaceBudgetCountsEveryShardEntryClass(t *testing.T) {
	if MaxFileStateShardDirectories != 256 ||
		MaxUpdateTemporariesPerSession != MaxFilesPerSession ||
		MaxFileStateEntriesPerSession != 2*MaxFilesPerSession {
		t.Fatal("file-state enumeration constants no longer match the two-link crash bound")
	}
	budget, err := NewFileStateNamespaceBudget(2)
	if err != nil {
		t.Fatal(err)
	}
	digest := identity32[LocatorDigest](0x12)
	recordName := FileRecordName(digest)
	secondRecordName := FileRecordName(identity32[LocatorDigest](0x34))
	temporaryName := UpdateTemporaryName(digest, identity32[UpdateNonce](0xab))
	entries := []ClassifiedFileShardEntry{
		ClassifyFileShardEntry(recordName.Shard(), recordName.Name()),
		ClassifyFileShardEntry(secondRecordName.Shard(), secondRecordName.Name()),
		ClassifyFileShardEntry(temporaryName.Shard(), temporaryName.Name()),
		ClassifyFileShardEntry(recordName.Shard(), strings.ToUpper(recordName.Name())),
	}
	for _, entry := range entries {
		if err := budget.ObserveEntry(entry); err != nil {
			t.Fatalf("observe classification %d: %v", entry.Classification(), err)
		}
	}
	if budget.SelectedFiles() != 2 || budget.Records() != 2 || budget.AuxiliaryEntries() != 2 ||
		budget.TotalEntries() != 4 {
		t.Fatalf("budget counts = %+v", budget)
	}
	thirdRecordName := FileRecordName(identity32[LocatorDigest](0x56))
	for _, overflow := range []ClassifiedFileShardEntry{
		ClassifyFileShardEntry("zz", "opaque"),
		ClassifyFileShardEntry(thirdRecordName.Shard(), thirdRecordName.Name()),
	} {
		if err := budget.ObserveEntry(overflow); !errors.Is(err, ErrFileStateNamespaceLimit) {
			t.Fatalf("overflow classification %d error = %v", overflow.Classification(), err)
		}
	}
	if budget.TotalEntries() != 4 {
		t.Fatal("failed observation consumed namespace budget")
	}
}

func TestFileStateNamespaceBudgetUsesSelectionCountAndCountsUnknownShards(t *testing.T) {
	budget, err := NewFileStateNamespaceBudget(1)
	if err != nil {
		t.Fatal(err)
	}
	digest := identity32[LocatorDigest](0x12)
	recordName := FileRecordName(digest)
	temporaryName := UpdateTemporaryName(digest, identity32[UpdateNonce](0xab))
	if err := budget.ObserveEntry(ClassifyFileShardEntry(recordName.Shard(), recordName.Name())); err != nil {
		t.Fatal(err)
	}
	if err := budget.ObserveEntry(ClassifyFileShardEntry(temporaryName.Shard(), temporaryName.Name())); err != nil {
		t.Fatal(err)
	}
	if err := budget.ObserveEntry(ClassifyFileShardEntry("zz", "opaque")); !errors.Is(err, ErrFileStateNamespaceLimit) {
		t.Fatalf("selection-scoped auxiliary overflow error = %v", err)
	}

	budget.shards = MaxFileStateShardDirectories - 1
	if err := budget.ObserveShard(); err != nil || budget.Shards() != MaxFileStateShardDirectories {
		t.Fatalf("exact shard bound = %d, %v", budget.Shards(), err)
	}
	if err := budget.ObserveShard(); !errors.Is(err, ErrFileStateNamespaceLimit) {
		t.Fatalf("shard bound + 1 error = %v", err)
	}
	if _, err := NewFileStateNamespaceBudget(MaxFilesPerSession + 1); !errors.Is(err, ErrInvalidState) {
		t.Fatalf("oversized selection error = %v", err)
	}
	if err := (*FileStateNamespaceBudget)(nil).ObserveShard(); !errors.Is(err, ErrInvalidState) {
		t.Fatalf("nil budget error = %v", err)
	}
	if err := (&FileStateNamespaceBudget{}).ObserveEntry(ClassifiedFileShardEntry{}); !errors.Is(err, ErrInvalidState) {
		t.Fatalf("zero budget error = %v", err)
	}
	valid, err := NewFileStateNamespaceBudget(1)
	if err != nil {
		t.Fatal(err)
	}
	if err := valid.ObserveEntry(ClassifiedFileShardEntry{}); !errors.Is(err, ErrInvalidState) {
		t.Fatalf("zero classification error = %v", err)
	}
}
