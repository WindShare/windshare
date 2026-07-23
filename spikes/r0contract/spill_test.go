package r0contract

import (
	"errors"
	"testing"
)

var errInjectedDiskFull = errors.New("injected metadata spill disk full")

type spillTransaction struct {
	failAt    string
	temporary bool
	published bool
}

func (transaction *spillTransaction) perform(step string) error {
	if transaction.failAt == step {
		return errInjectedDiskFull
	}
	return nil
}

func (transaction *spillTransaction) abort() {
	if !transaction.published {
		transaction.temporary = false
	}
}

func writeCatalogGeneration(transaction *spillTransaction) (err error) {
	transaction.temporary = true
	defer func() {
		if err != nil {
			transaction.abort()
		}
	}()
	for _, step := range []string{
		"pages",
		"node-records",
		"terminal",
		"budget-charge",
		"spill-flush",
		"atomic-install",
	} {
		if err = transaction.perform(step); err != nil {
			return err
		}
	}
	transaction.published = true
	transaction.temporary = false
	return nil
}

func TestCatalogDiskFullCutsCleanTemporaryState(t *testing.T) {
	steps := []string{
		"pages",
		"node-records",
		"terminal",
		"budget-charge",
		"spill-flush",
		"atomic-install",
	}
	for _, step := range steps {
		t.Run(step, func(t *testing.T) {
			transaction := &spillTransaction{failAt: step}
			if err := writeCatalogGeneration(transaction); !errors.Is(err, errInjectedDiskFull) {
				t.Fatalf("writeCatalogGeneration() = %v", err)
			}
			if transaction.temporary || transaction.published {
				t.Fatalf("failed transaction leaked temp/published = %t/%t", transaction.temporary, transaction.published)
			}
		})
	}

	transaction := &spillTransaction{}
	if err := writeCatalogGeneration(transaction); err != nil {
		t.Fatal(err)
	}
	if transaction.temporary || !transaction.published {
		t.Fatalf("committed transaction temp/published = %t/%t", transaction.temporary, transaction.published)
	}
}
