package liveshare

import (
	"bytes"
	"context"
	"errors"
	mathrand "math/rand"
	"os"
	"path/filepath"
	"testing"

	"github.com/windshare/windshare/core/catalog"
	"github.com/windshare/windshare/core/content"
)

type preparationRevisionTracer struct{}

func (*preparationRevisionTracer) TraceRevision(content.RevisionTrace) {}

func TestPrepareSenderWiresSharedCacheAndRevisionIdentityAuthorities(t *testing.T) {
	filename := filepath.Join(t.TempDir(), "selected.bin")
	if err := os.WriteFile(filename, []byte("revision-cache-wiring"), 0o600); err != nil {
		t.Fatal(err)
	}
	dependencies := productionSenderPreparationDependencies()
	newRevisionDeriver := dependencies.newRevisionDeriver
	newRevisionStore := dependencies.newRevisionStore
	var capturedKey content.RevisionIdentityKey
	var deriverConstructions int
	var capturedDeriver *observedRevisionDeriver
	dependencies.newRevisionDeriver = func(
		key content.RevisionIdentityKey,
	) (senderRevisionIdentityDeriver, error) {
		capturedKey = key
		deriverConstructions++
		delegate, err := newRevisionDeriver(key)
		if err != nil {
			return nil, err
		}
		hmacDeriver, ok := delegate.(*content.HMACRevisionIdentityDeriver)
		if !ok {
			t.Fatalf("production revision deriver type = %T", delegate)
		}
		capturedDeriver = &observedRevisionDeriver{
			delegate:  hmacDeriver,
			destroyed: make(chan struct{}),
		}
		return capturedDeriver, nil
	}
	var capturedStoreConfig content.RevisionStoreConfig
	dependencies.newRevisionStore = func(
		config content.RevisionStoreConfig,
	) (*content.RevisionStore, error) {
		capturedStoreConfig = config
		return newRevisionStore(config)
	}
	tracer := &preparationRevisionTracer{}
	sender, err := PrepareSender(context.Background(), SenderConfig{
		RevisionCapacity: newTestRevisionCapacity(t),
		Paths:            []string{filename},
		Relays:           []string{"ws://127.0.0.1:8484"},
		ChunkSize:        catalog.MinChunkSize,
		Random:           mathrand.New(mathrand.NewSource(71)),
		RevisionTracer:   tracer,
		preparation:      dependencies,
	})
	if err != nil {
		t.Fatal(err)
	}
	if capturedDeriver == nil || deriverConstructions != 1 {
		t.Fatalf("revision deriver constructions = %d, deriver = %v", deriverConstructions, capturedDeriver)
	}
	if capturedStoreConfig.RevisionDeriver != capturedDeriver {
		t.Fatal("revision store did not receive the live-share-owned deriver")
	}
	if capturedStoreConfig.MetadataBudget == nil {
		t.Fatal("revision store did not receive a share metadata budget")
	}
	if capturedStoreConfig.CacheInvalidator != sender.cache {
		t.Fatal("revision store and sender services did not share one cache authority")
	}
	if capturedStoreConfig.Tracer != tracer {
		t.Fatal("revision store did not receive the configured revision tracer")
	}
	if capturedKey == (content.RevisionIdentityKey{}) {
		t.Fatal("sender preparation accepted a zero revision identity key")
	}
	if bytes.Equal(capturedKey[:], sender.capability.ReadSecret) {
		t.Fatal("revision identity reused the capability read secret")
	}
	if bytes.Equal(capturedKey[:], sender.sessionAuthKey) {
		t.Fatal("revision identity reused protocol session authority")
	}

	if err := sender.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case <-capturedDeriver.destroyed:
	default:
		t.Fatal("sender close retained the revision identity key")
	}
}

func TestPrepareSenderRollbackDestroysRevisionIdentityAuthority(t *testing.T) {
	filename := filepath.Join(t.TempDir(), "selected.bin")
	if err := os.WriteFile(filename, []byte("revision-rollback"), 0o600); err != nil {
		t.Fatal(err)
	}
	dependencies := productionSenderPreparationDependencies()
	newRevisionDeriver := dependencies.newRevisionDeriver
	var capturedDeriver *observedRevisionDeriver
	dependencies.newRevisionDeriver = func(
		key content.RevisionIdentityKey,
	) (senderRevisionIdentityDeriver, error) {
		delegate, err := newRevisionDeriver(key)
		if err != nil {
			return nil, err
		}
		hmacDeriver, ok := delegate.(*content.HMACRevisionIdentityDeriver)
		if !ok {
			t.Fatalf("production revision deriver type = %T", delegate)
		}
		capturedDeriver = &observedRevisionDeriver{
			delegate:  hmacDeriver,
			destroyed: make(chan struct{}),
		}
		return capturedDeriver, nil
	}
	lateFailure := errors.New("forced late revision rollback")
	dependencies.sessionAuthKey = func(*content.KeyTree) (content.DerivedKey, error) {
		return content.DerivedKey{}, lateFailure
	}

	sender, err := PrepareSender(context.Background(), SenderConfig{
		RevisionCapacity: newTestRevisionCapacity(t),
		Paths:            []string{filename},
		Relays:           []string{"ws://127.0.0.1:8484"},
		ChunkSize:        catalog.MinChunkSize,
		Random:           mathrand.New(mathrand.NewSource(73)),
		preparation:      dependencies,
	})
	if sender != nil || !errors.Is(err, lateFailure) {
		t.Fatalf("late preparation result = %v, %v", sender, err)
	}
	if capturedDeriver == nil {
		t.Fatal("late failure occurred before revision identity ownership transferred")
	}
	select {
	case <-capturedDeriver.destroyed:
	default:
		t.Fatal("preparation rollback retained the revision identity key")
	}
}
