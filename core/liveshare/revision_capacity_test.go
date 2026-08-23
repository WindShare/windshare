package liveshare

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/windshare/windshare/core/catalog"
	"github.com/windshare/windshare/core/content/records"
	"github.com/windshare/windshare/core/content/revisioncapacity"
)

func newTestRevisionCapacity(t testing.TB) *revisioncapacity.Coordinator {
	t.Helper()
	owner, err := revisioncapacity.NewProcessOwner(revisioncapacity.DefaultProcessConfig())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := owner.Close(); err != nil {
			t.Errorf("close test revision capacity owner: %v", err)
		}
	})
	return owner.Coordinator()
}

func TestPreparedSendersShareApplicationRevisionCapacityUntilEveryStoreCloses(t *testing.T) {
	owner, err := revisioncapacity.NewProcessOwner(revisioncapacity.DefaultProcessConfig())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := owner.Close(); err != nil {
			t.Errorf("close application revision capacity owner: %v", err)
		}
	})
	coordinator := owner.Coordinator()
	prepare := func(name string) *PreparedSender {
		t.Helper()
		path := filepath.Join(t.TempDir(), name)
		if err := os.WriteFile(path, []byte(name), 0o600); err != nil {
			t.Fatal(err)
		}
		sender, err := PrepareSender(context.Background(), SenderConfig{
			Paths: []string{path}, Relays: []string{"ws://127.0.0.1:8484"}, ChunkSize: catalog.MinChunkSize,
			RevisionCapacity: coordinator,
		})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			if err := sender.Close(); err != nil {
				t.Errorf("close prepared sender: %v", err)
			}
		})
		return sender
	}

	first := prepare("first.bin")
	second := prepare("second.bin")
	firstCapacity := first.revisionStore.CapacitySnapshot()
	secondCapacity := second.revisionStore.CapacitySnapshot()
	if firstCapacity.Process().Identity() != secondCapacity.Process().Identity() ||
		firstCapacity.Process().Limits() != revisioncapacity.DefaultProcessLimits() ||
		secondCapacity.Process().Limits() != revisioncapacity.DefaultProcessLimits() {
		t.Fatalf("prepared senders did not retain one process capacity authority: first=%+v second=%+v", firstCapacity.Process(), secondCapacity.Process())
	}
	if firstCapacity.Share().Identity() == secondCapacity.Share().Identity() ||
		firstCapacity.Share().Limits() != revisioncapacity.DefaultShareLimits() ||
		secondCapacity.Share().Limits() != revisioncapacity.DefaultShareLimits() {
		t.Fatalf("prepared senders did not retain distinct default-limited share registrations: first=%+v second=%+v", firstCapacity.Share(), secondCapacity.Share())
	}
	assertLiveStores := func(want int) {
		t.Helper()
		var live *revisioncapacity.LiveStoreRegistrationsError
		if err := owner.Close(); !errors.As(err, &live) || live.Count() != want {
			t.Fatalf("owner close with live stores = %v, want %d registrations", err, want)
		}
	}
	assertLiveStores(2)
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	assertLiveStores(1)
	if err := second.Close(); err != nil {
		t.Fatal(err)
	}
	if err := owner.Close(); err != nil {
		t.Fatalf("owner did not close after both prepared senders: %v", err)
	}
}

func TestPrepareSenderFailureUnregistersStoreBeforeReturning(t *testing.T) {
	owner, err := revisioncapacity.NewProcessOwner(revisioncapacity.DefaultProcessConfig())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := owner.Close(); err != nil {
			t.Errorf("close application revision capacity owner: %v", err)
		}
	})
	path := filepath.Join(t.TempDir(), "selected.bin")
	if err := os.WriteFile(path, []byte("partial preparation"), 0o600); err != nil {
		t.Fatal(err)
	}
	injected := errors.New("record sealer construction failed")
	dependencies := productionSenderPreparationDependencies()
	dependencies.newRecordSealer = func(records.SealerConfig) (*records.Sealer, error) {
		return nil, injected
	}

	sender, err := PrepareSender(context.Background(), SenderConfig{
		Paths: []string{path}, Relays: []string{"ws://127.0.0.1:8484"}, ChunkSize: catalog.MinChunkSize,
		RevisionCapacity: owner.Coordinator(), preparation: dependencies,
	})
	if sender != nil || !errors.Is(err, injected) {
		t.Fatalf("partial preparation result = %v, %v", sender, err)
	}
	if err := owner.Close(); err != nil {
		t.Fatalf("partial preparation returned before unregistering its store: %v", err)
	}
}

func TestPrepareSenderRejectsMissingApplicationRevisionCapacity(t *testing.T) {
	path := filepath.Join(t.TempDir(), "selected.bin")
	if err := os.WriteFile(path, []byte("explicit owner"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := PrepareSender(context.Background(), SenderConfig{
		Paths: []string{path}, Relays: []string{"ws://127.0.0.1:8484"}, ChunkSize: catalog.MinChunkSize,
	}); err == nil {
		t.Fatal("sender preparation accepted a missing application revision capacity owner")
	}
}
