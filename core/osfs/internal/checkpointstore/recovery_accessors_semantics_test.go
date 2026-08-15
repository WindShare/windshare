package checkpointstore

import (
	"testing"

	"github.com/windshare/windshare/core/osfs/internal/checkpointmodel"
	"github.com/windshare/windshare/core/osfs/internal/fileexecution"
)

func TestRecoveryAttentionAndCheckpointMatchingRemainClosed(t *testing.T) {
	codes := []AttentionCode{
		AttentionUnknownShard,
		AttentionUnknownEntry,
		AttentionCorruptRecord,
		AttentionInvalidBinding,
		AttentionUncommittedRecord,
		AttentionInvalidCandidate,
		AttentionOrphanedCandidate,
		AttentionConflictingCandidate,
	}
	for _, code := range codes {
		if !code.Valid() {
			t.Fatalf("closed attention code %q rejected", code)
		}
	}
	if AttentionCode("").Valid() || AttentionCode("future").Valid() {
		t.Fatal("attention code union is open")
	}
	attention := newAttention(AttentionInvalidBinding, "aa", "record")
	if attention.Code() != AttentionInvalidBinding || attention.Reference() == "" {
		t.Fatalf("attention = %+v", attention)
	}
	if checkpointKeyMatchesRecord(fileexecution.CheckpointKey{}, checkpointmodel.Record{}) {
		t.Fatal("zero checkpoint coordinates matched")
	}
}
