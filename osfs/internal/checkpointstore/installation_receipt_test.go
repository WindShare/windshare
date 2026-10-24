package checkpointstore

import (
	"bytes"
	"errors"
	"testing"

	"github.com/windshare/windshare/osfs/internal/checkpointmodel"
)

func TestInstallationReceiptRequiresNamespaceDurabilityEvenForExactRetry(t *testing.T) {
	for _, operation := range []string{"replace", "retry-replace", "retry-create"} {
		t.Run(operation, func(t *testing.T) {
			_, registry, lease, repository, ownership, intent := openRepositoryFixture(t, 0xe1)
			t.Cleanup(func() {
				_ = repository.Close()
				_ = lease.Close()
				_ = registry.Close()
			})
			initial := checkpointRecordFixture(t, ownership, intent, 0xe2)
			if _, err := repository.Create(initial); err != nil {
				t.Fatal(err)
			}
			next, err := checkpointmodel.PromoteInitialCandidate(initial)
			if err != nil {
				t.Fatal(err)
			}
			if operation != "replace" {
				if _, err := repository.Replace(initial, next); err != nil {
					t.Fatal(err)
				}
			}
			injected := errors.New("namespace durability unavailable")
			repository.records = observeCleanupBarriers(repository.records, func() error { return injected })
			var receipt checkpointmodel.Record
			if operation == "retry-create" {
				receipt, err = repository.Create(next)
			} else {
				receipt, err = repository.Replace(initial, next)
			}
			if !errors.Is(err, injected) || receipt.Valid() {
				t.Fatalf("failed installation produced receipt: valid=%v, err=%v", receipt.Valid(), err)
			}
			observed, err := repository.Reopen(next.RecordID())
			if err != nil || !bytes.Equal(observed.CanonicalBytes(), next.CanonicalBytes()) {
				t.Fatalf("failed-sync fixture did not leave the exact readable image: %v", err)
			}
		})
	}
}
