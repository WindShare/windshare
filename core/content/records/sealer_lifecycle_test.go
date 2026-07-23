package records

import (
	"bytes"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/windshare/windshare/core/catalog"
)

func TestSealerDestroyClearsOwnedSecretsAndPreservesBorrowedKeyTree(t *testing.T) {
	fixture := newRecordFixture(t, uint64(catalog.MinChunkSize))
	t.Cleanup(fixture.keys.Destroy)
	signingSource := bytes.Clone(fixture.privateKey)
	wantSigningKey := bytes.Clone(signingSource)
	sealer, err := NewSealer(SealerConfig{
		ShareInstance: fixture.share,
		Keys:          fixture.keys,
		SigningKey:    signingSource,
		NonceSource:   bytes.NewReader(bytes.Repeat([]byte{0x71}, ObjectNonceBytes*2)),
	})
	if err != nil {
		t.Fatal(err)
	}
	ownedSigningKey := sealer.signingKey
	clear(signingSource)
	if !bytes.Equal(ownedSigningKey, wantSigningKey) {
		t.Fatal("sealer retained the caller's mutable signing-key storage")
	}

	revisionObject, err := sealer.SealRevision(fixture.descriptor)
	if err != nil {
		t.Fatal(err)
	}
	record, err := NewBlockRecord(
		fixture.descriptor,
		0,
		bytes.Repeat([]byte{0x33}, catalog.MinChunkSize),
	)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := sealer.SealBlock(record); err != nil {
		t.Fatal(err)
	}
	cacheKey := revisionCacheKey{file: fixture.file, revision: fixture.revision}
	cachedObject := sealer.revisions[cacheKey].object
	if len(cachedObject) == 0 || !bytes.Equal(cachedObject, revisionObject) {
		t.Fatal("revision cache was not populated before destruction")
	}

	sealer.Destroy()
	sealer.Destroy()
	if !allSealerBytesZero(ownedSigningKey) {
		t.Fatal("owned signing key was not cleared")
	}
	if !allSealerBytesZero(cachedObject) {
		t.Fatal("destroyed revision cache retained object bytes")
	}
	if sealer.signingKey != nil || sealer.keys != nil || sealer.nonceSource != nil ||
		sealer.revisions != nil || sealer.fileSealUses != nil || sealer.segmentSealUses != nil {
		t.Fatal("destroyed sealer retained owned state or borrowed references")
	}
	borrowedKey, err := fixture.keys.CatalogKey()
	if err != nil {
		t.Fatalf("sealer destroyed its borrowed KeyTree: %v", err)
	}
	borrowedKey.Destroy()
	if _, err := sealer.SealRevision(fixture.descriptor); !errors.Is(err, ErrSealerDestroyed) {
		t.Fatalf("post-destroy revision seal = %v", err)
	}
	if _, err := sealer.SealBlock(record); !errors.Is(err, ErrSealerDestroyed) {
		t.Fatalf("post-destroy block seal = %v", err)
	}

	var nilSealer *Sealer
	nilSealer.Stop()
	nilSealer.Destroy()
	if _, err := nilSealer.SealRevision(fixture.descriptor); !errors.Is(err, ErrSealerDestroyed) {
		t.Fatalf("nil sealer revision = %v", err)
	}
}

func TestSealerReentrantStopRejectsNewWorkAndDestroyDrainsActiveSeal(t *testing.T) {
	fixture := newRecordFixture(t, 17)
	t.Cleanup(fixture.keys.Destroy)
	reader := &reentrantStopNonceReader{
		started:      make(chan struct{}),
		stopReturned: make(chan struct{}),
		release:      make(chan struct{}),
	}
	sealer, err := NewSealer(SealerConfig{
		ShareInstance: fixture.share,
		Keys:          fixture.keys,
		SigningKey:    fixture.privateKey,
		NonceSource:   reader,
	})
	if err != nil {
		t.Fatal(err)
	}
	reader.sealer = sealer
	ownedSigningKey := sealer.signingKey
	sealResult := make(chan error, 1)
	go func() {
		_, sealErr := sealer.SealRevision(fixture.descriptor)
		sealResult <- sealErr
	}()

	awaitSealerSignal(t, reader.started, "nonce callback")
	awaitSealerSignal(t, reader.stopReturned, "reentrant Stop")
	sealer.Stop()
	if _, err := sealer.SealRevision(fixture.descriptor); !errors.Is(err, ErrSealerDestroyed) {
		t.Fatalf("seal admitted after Stop = %v", err)
	}
	sealer.lifecycleMu.Lock()
	active := sealer.activeSeals
	drained := sealer.drained
	sealer.lifecycleMu.Unlock()
	if active != 1 {
		t.Fatalf("active seals = %d, want 1", active)
	}
	select {
	case <-drained:
		t.Fatal("sealer drained before the active nonce callback returned")
	default:
	}

	destroyed := make(chan struct{})
	go func() {
		sealer.Destroy()
		close(destroyed)
	}()
	close(reader.release)
	if err := awaitSealerError(t, sealResult, "active seal"); err != nil {
		t.Fatalf("active seal after reentrant Stop = %v", err)
	}
	awaitSealerSignal(t, destroyed, "Destroy drain")
	if !allSealerBytesZero(ownedSigningKey) {
		t.Fatal("Destroy returned before clearing the signing key")
	}
}

type reentrantStopNonceReader struct {
	sealer       *Sealer
	started      chan struct{}
	stopReturned chan struct{}
	release      chan struct{}
	once         sync.Once
}

func (reader *reentrantStopNonceReader) Read(destination []byte) (int, error) {
	reader.once.Do(func() {
		close(reader.started)
		reader.sealer.Stop()
		close(reader.stopReturned)
		<-reader.release
	})
	for index := range destination {
		destination[index] = byte(index + 1)
	}
	return len(destination), nil
}

func allSealerBytesZero(value []byte) bool {
	for _, item := range value {
		if item != 0 {
			return false
		}
	}
	return true
}

func awaitSealerSignal(t *testing.T, signal <-chan struct{}, label string) {
	t.Helper()
	select {
	case <-signal:
	case <-time.After(2 * time.Second):
		t.Fatalf("timed out waiting for %s", label)
	}
}

func awaitSealerError(t *testing.T, result <-chan error, label string) error {
	t.Helper()
	select {
	case err := <-result:
		return err
	case <-time.After(2 * time.Second):
		t.Fatalf("timed out waiting for %s", label)
		return nil
	}
}
