package liveshare

import (
	"context"
	"errors"
	mathrand "math/rand"
	"os"
	"path/filepath"
	"testing"

	"github.com/windshare/windshare/core/catalog"
	"github.com/windshare/windshare/core/content"
	"github.com/windshare/windshare/core/content/revisioncapacity"
)

type preparationRevisionTracer struct{}

func (*preparationRevisionTracer) TraceRevision(content.RevisionTrace) {}

func TestPrepareSenderWiresSharedCacheAndRevisionIDs(t *testing.T) {
	filename := filepath.Join(t.TempDir(), "selected.bin")
	if err := os.WriteFile(filename, []byte("revision-cache-wiring"), 0o600); err != nil {
		t.Fatal(err)
	}
	dependencies := productionSenderPreparationDependencies()
	newRevisionStore := dependencies.newRevisionStore
	var capturedStoreConfig content.RevisionStoreConfig
	dependencies.newRevisionStore = func(
		config content.RevisionStoreConfig,
	) (*content.RevisionStore, error) {
		capturedStoreConfig = config
		return newRevisionStore(config)
	}
	tracer := &preparationRevisionTracer{}
	sender, err := PrepareSender(context.Background(), SenderConfig{
		CatalogBudget:    testCatalogBudget(),
		CacheBudget:      testCacheBudget(),
		RevisionCapacity: newTestRevisionCapacity(t),
		Source:           testFileSource([]string{filename}),
		Relays:           []string{"ws://127.0.0.1:8484"},
		ChunkSize:        catalog.MinChunkSize,
		Random:           mathrand.New(mathrand.NewSource(71)),
		RevisionTracer:   tracer,
		preparation:      dependencies,
	})
	if err != nil {
		t.Fatal(err)
	}
	if capturedStoreConfig.RevisionIDs == nil {
		t.Fatal("revision store did not receive a per-open revision ID generator")
	}
	firstRevision, err := capturedStoreConfig.RevisionIDs.NewFileRevision()
	if err != nil || firstRevision.IsZero() {
		t.Fatalf("first revision ID = %x, err = %v", firstRevision, err)
	}
	secondRevision, err := capturedStoreConfig.RevisionIDs.NewFileRevision()
	if err != nil || secondRevision.IsZero() || secondRevision == firstRevision {
		t.Fatalf("second revision ID = %x (first=%x), err = %v", secondRevision, firstRevision, err)
	}
	if capturedStoreConfig.CacheInvalidator != sender.cache {
		t.Fatal("revision store and sender services did not share one cache authority")
	}
	if capturedStoreConfig.Tracer != tracer {
		t.Fatal("revision store did not receive the configured revision tracer")
	}
	if err := sender.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestPrepareSenderRollbackClosesRevisionStoreAndCache(t *testing.T) {
	filename := filepath.Join(t.TempDir(), "selected.bin")
	if err := os.WriteFile(filename, []byte("revision-rollback"), 0o600); err != nil {
		t.Fatal(err)
	}
	dependencies := productionSenderPreparationDependencies()
	newRevisionStore := dependencies.newRevisionStore
	var capturedStore *content.RevisionStore
	dependencies.newRevisionStore = func(
		config content.RevisionStoreConfig,
	) (*content.RevisionStore, error) {
		store, err := newRevisionStore(config)
		capturedStore = store
		return store, err
	}
	lateFailure := errors.New("forced late revision rollback")
	dependencies.sessionAuthKey = func(*content.KeyTree) (content.DerivedKey, error) {
		return content.DerivedKey{}, lateFailure
	}

	sender, err := PrepareSender(context.Background(), SenderConfig{
		CatalogBudget:    testCatalogBudget(),
		CacheBudget:      testCacheBudget(),
		RevisionCapacity: newTestRevisionCapacity(t),
		Source:           testFileSource([]string{filename}),
		Relays:           []string{"ws://127.0.0.1:8484"},
		ChunkSize:        catalog.MinChunkSize,
		Random:           mathrand.New(mathrand.NewSource(73)),
		preparation:      dependencies,
	})
	if sender != nil || !errors.Is(err, lateFailure) {
		t.Fatalf("late preparation result = %v, %v", sender, err)
	}
	if capturedStore == nil {
		t.Fatal("late failure occurred before revision store ownership transferred")
	}
	if _, err := capturedStore.RegisterSession(revisioncapacity.SessionConfig{
		SessionID: "rollback-check",
		Limits:    revisioncapacity.DefaultSessionLimits(),
	}); !errors.Is(err, content.ErrRevisionStoreClosed) {
		t.Fatalf("rolled-back revision store accepted session registration: %v", err)
	}
}
