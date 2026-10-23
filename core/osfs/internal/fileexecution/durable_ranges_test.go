package fileexecution

import (
	"context"
	"errors"
	"testing"

	"github.com/windshare/windshare/core/osfs/internal/checkpointmodel"
)

func TestConfirmedRangesRequireDataAndRecordDurability(t *testing.T) {
	for _, failure := range []string{"data-sync", "record-install"} {
		t.Run(failure, func(t *testing.T) {
			fixture := newExecutionFixture(t, 8)
			repository := &memoryCheckpointRepository{}
			platform := newMemoryPlatform()
			destination := &memoryDestination{target: fixture.file.Target(), final: FinalAbsent}
			engine := newFixtureEngine(t, fixture, repository, platform, destination, nil)
			ctx := context.Background()
			start, err := engine.BeginFile(ctx, fixture.file, fixture.destination)
			if err != nil {
				t.Fatal(err)
			}
			transaction, _, ok := start.Transaction()
			if !ok {
				t.Fatal("no writable transaction")
			}
			strategy := transaction.(*PartialFileTransaction).strategy.(*resumablePartialFileTransaction)
			owned := strategy.file.(*memoryOwnedFile)
			if err := transaction.WriteRange(ctx, 0, []byte("abcd")); err != nil {
				t.Fatal(err)
			}
			prior := repository.record
			cuts := len(repository.stores)
			injected := errors.New("durability failed")
			if failure == "data-sync" {
				owned.syncErr = injected
			} else {
				repository.storeErr = injected
			}
			if checkpoint, err := transaction.Checkpoint(ctx); !errors.Is(err, injected) || !checkpoint.Ranges().IsEmpty() {
				t.Fatalf("failed checkpoint returned progress: %v, %v", checkpoint.Ranges(), err)
			}
			if !recordEqual(repository.record, prior) || !recordEqual(strategy.record, prior) || len(repository.stores) != cuts {
				t.Fatal("failed durability advanced checkpoint authority")
			}
			if strategy.pending.IsEmpty() {
				t.Fatal("failed checkpoint forgot unconfirmed writes")
			}
			owned.syncErr, repository.storeErr = nil, nil
			checkpoint, err := transaction.Checkpoint(ctx)
			if err != nil || checkpoint.Ranges().IsEmpty() || len(repository.stores) != cuts+1 ||
				repository.record.CommitState() != checkpointmodel.CommitVerified {
				t.Fatalf("confirmed checkpoint = %v, %v, durable cuts = %d", checkpoint.Ranges(), err, len(repository.stores)-cuts)
			}
		})
	}
}
