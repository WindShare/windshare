package resumeauthority

import (
	"context"
	"errors"
	"io/fs"
	"testing"

	"github.com/windshare/windshare/core/osfs/internal/checkpointmodel"
	"github.com/windshare/windshare/core/osfs/internal/checkpointstore"
	"github.com/windshare/windshare/core/osfs/internal/outputcap"
)

func TestResumeAdapterSettlesAbsentCutsButStopsAtRecordReplacement(t *testing.T) {
	t.Run("already removed crash cuts", func(t *testing.T) {
		fixture := newResumeAdapterFixture(t, 0xc1)
		inventory, leased, snapshot := observeResumeFixture(t, fixture)
		actions := discardPlanForSnapshot(t, snapshot).Actions()

		removeMemoryFile(fixture.stageShard, fixture.stageName)
		removeMemoryFile(fixture.anchorShard, fixture.anchorName)
		removeMemoryFile(fixture.recordShard, fixture.recordName)
		want := []ApplyStatus{
			ApplyAlreadySatisfied,
			ApplyCompleted,
			ApplyAlreadySatisfied,
			ApplyCompleted,
			ApplyAlreadySatisfied,
			ApplyCompleted,
		}
		for index, action := range actions {
			result, err := leased.Apply(context.Background(), action)
			if err != nil || result.Status() != want[index] {
				t.Fatalf("action %d (%v) settlement = %v, %v", index, action.Kind(), result.Status(), err)
			}
		}
		assertResumeArtifactsAbsent(t, fixture)
		closeResumeFixture(t, inventory, leased)
	})

	t.Run("canonical record replacement", func(t *testing.T) {
		fixture := newResumeAdapterFixture(t, 0xc2)
		inventory, leased, snapshot := observeResumeFixture(t, fixture)
		actions := discardPlanForSnapshot(t, snapshot).Actions()
		applyActions(t, leased, actions[:4])

		replacement := &memoryFileData{bytes: []byte("foreign canonical replacement")}
		fixture.recordShard.mu.Lock()
		fixture.recordShard.files[fixture.recordName] = replacement
		fixture.recordShard.mu.Unlock()

		for attempt := range 2 {
			result, err := leased.Apply(context.Background(), actions[4])
			if err != nil || result.Status() != ApplyNeedsAttention ||
				len(result.Attention()) != 1 ||
				result.Attention()[0].Reason() != AttentionReplacement {
				t.Fatalf("replacement attempt %d = %+v, %v", attempt, result, err)
			}
		}
		fixture.recordShard.mu.Lock()
		retained := fixture.recordShard.files[fixture.recordName] == replacement
		fixture.recordShard.mu.Unlock()
		if !retained {
			t.Fatal("same-name canonical replacement was removed")
		}
		closeResumeFixture(t, inventory, leased)
	})
}

func TestPublishedCheckpointRequiresRetainedExactAnchorWitness(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(resumeAdapterFixture)
		want   Evidence
	}{
		{name: "exact", want: EvidenceExact},
		{
			name: "missing anchor",
			mutate: func(fixture resumeAdapterFixture) {
				removeMemoryFile(fixture.anchorShard, fixture.anchorName)
			},
			want: EvidenceAmbiguous,
		},
		{
			name: "same-name anchor replacement",
			mutate: func(fixture resumeAdapterFixture) {
				fixture.anchorShard.mu.Lock()
				fixture.anchorShard.files[fixture.anchorName] = &memoryFileData{
					bytes: make([]byte, fixture.record.ExactSize()),
				}
				fixture.anchorShard.mu.Unlock()
			},
			want: EvidenceAmbiguous,
		},
		{
			name: "anchor size drift",
			mutate: func(fixture resumeAdapterFixture) {
				fixture.anchorShard.mu.Lock()
				data := fixture.anchorShard.files[fixture.anchorName]
				fixture.anchorShard.mu.Unlock()
				data.mu.Lock()
				data.bytes = data.bytes[:len(data.bytes)-1]
				data.mu.Unlock()
			},
			want: EvidenceAmbiguous,
		},
	}
	for index, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			fixture := publishedResumeFixture(t, byte(0xd1+index))
			inventory, leased, snapshot := observeResumeFixture(t, fixture)
			provider := leased.(PinnedCheckpointProvider)
			checkpoint, ok := provider.PinnedCheckpoint(snapshot.Checkpoints()[0].RecordID())
			if !ok || checkpoint.Record().Phase() != checkpointmodel.PhasePublished {
				t.Fatal("published checkpoint did not retain a canonical pin")
			}
			public, err := fixture.anchorShard.OpenFile(fixture.anchorName, true, false)
			if err != nil {
				t.Fatal(err)
			}
			if test.mutate != nil {
				test.mutate(fixture)
			}
			evidence, err := checkpoint.SameOwnedFile(context.Background(), public)
			if err != nil || evidence != test.want {
				t.Fatalf("published anchor evidence = %v, %v; want %v", evidence, err, test.want)
			}
			if err := errors.Join(public.Close(), leased.Close(), inventory.Close()); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestResumeAdapterNormalizesOnlyTheClosedFailureVocabulary(t *testing.T) {
	projections := []struct {
		name  string
		cause error
		want  RepositoryErrorCode
	}{
		{
			name:  "busy",
			cause: outputcap.ErrNamespaceLockBusy,
			want:  RepositoryBusy,
		},
		{
			name:  "corrupt record",
			cause: checkpointmodel.ErrInvalidRecord,
			want:  RepositoryCorruptRecord,
		},
		{
			name:  "unsafe install",
			cause: checkpointmodel.ErrRecordBinding,
			want:  RepositoryUnsafeInstall,
		},
		{
			name:  "ownership mismatch",
			cause: checkpointmodel.ErrInvalidOwnership,
			want:  RepositoryOwnershipMismatch,
		},
		{
			name:  "state io",
			cause: errors.New("storage failed"),
			want:  RepositoryStateIO,
		},
	}
	for _, test := range projections {
		t.Run(test.name, func(t *testing.T) {
			projected := projectResumeError("resume adapter operation", test.cause)
			var repositoryErr *RepositoryError
			if !errors.As(projected, &repositoryErr) || repositoryErr.Code() != test.want {
				t.Fatalf("projected error = %v, want %s", projected, test.want)
			}
		})
	}
	if projectResumeError("no failure", nil) != nil {
		t.Fatal("nil adapter cause became a repository failure")
	}
	if err := projectResumeError("canceled", context.Canceled); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation projection = %v", err)
	}

	attentionCases := []struct {
		cause error
		want  AttentionReason
		ok    bool
	}{
		{cause: errResumePinReplaced, want: AttentionReplacement, ok: true},
		{cause: fs.ErrNotExist, want: AttentionReplacement, ok: true},
		{cause: outputcap.ErrUnsafeNamespace, want: AttentionCorruptBinding, ok: true},
		{cause: checkpointmodel.ErrInvalidRecord, want: AttentionCorruptBinding, ok: true},
		{cause: checkpointmodel.ErrRecordBinding, want: AttentionCorruptBinding, ok: true},
		{cause: checkpointmodel.ErrRecordChecksum, want: AttentionCorruptBinding, ok: true},
		{cause: checkpointmodel.ErrRecordNonCanonical, want: AttentionCorruptBinding, ok: true},
		{cause: errors.New("unclassified dependency failure")},
	}
	for _, test := range attentionCases {
		reason, ok := resumeObservationAttention(test.cause)
		if ok != test.ok || reason != test.want {
			t.Fatalf("attention projection for %v = %q/%t, want %q/%t",
				test.cause, reason, ok, test.want, test.ok)
		}
	}
}

func removeMemoryFile(directory *memoryDirectory, name string) {
	directory.mu.Lock()
	delete(directory.files, name)
	directory.mu.Unlock()
}

func publishedResumeFixture(t *testing.T, fill byte) resumeAdapterFixture {
	t.Helper()
	fixture := newResumeAdapterFixture(t, fill)
	namespace, err := checkpointstore.OpenNamespace(fixture.config)
	if err != nil {
		t.Fatal(err)
	}
	lease, err := namespace.AcquireIntent(fixture.record.TransferIntentDigest())
	if err != nil {
		t.Fatal(err)
	}
	repository, err := lease.OpenExistingRepository()
	if err != nil {
		t.Fatal(err)
	}
	publishing, err := checkpointmodel.AdvanceState(
		fixture.record,
		fixture.record.StateGeneration()+1,
		checkpointmodel.PhasePublishing,
		checkpointmodel.CommitVerified,
		0,
		0,
		0,
	)
	if err != nil {
		t.Fatal(err)
	}
	if err := repository.Replace(fixture.record, publishing); err != nil {
		t.Fatal(err)
	}
	published, err := checkpointmodel.AdvanceState(
		publishing,
		publishing.StateGeneration()+1,
		checkpointmodel.PhasePublished,
		checkpointmodel.CommitPublished,
		0,
		0,
		0,
	)
	if err != nil {
		t.Fatal(err)
	}
	if err := repository.Replace(publishing, published); err != nil {
		t.Fatal(err)
	}
	if err := errors.Join(repository.Close(), lease.Close(), namespace.Close()); err != nil {
		t.Fatal(err)
	}
	fixture.record = published
	return fixture
}
