//go:build windows || linux || darwin

package osfs

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/windshare/windshare/osfs/internal/checkpointstore"
	"github.com/windshare/windshare/osfs/internal/outputcap"
)

func TestPortablePublicationCollisionRetainsDiscardAuthority(t *testing.T) {
	ctx := context.Background()
	root, intent := pausedPortableFile(t, 4)
	stage := onlyPortableStage(t, root)
	repository, policy, closeRepository := openRetainedCandidateRepository(t, root, intent)
	store, err := checkpointstore.NewFileExecutionStoreWithStagePolicy(repository, policy)
	if err != nil {
		t.Fatal(err)
	}
	slots, attention, err := store.Inventory()
	if err != nil || len(slots) != 1 || len(attention) != 0 {
		t.Fatalf("checkpoint inventory: %v %v %v", slots, attention, err)
	}
	record, exact := slots[0].Record()
	if !exact {
		t.Fatal("checkpoint has no exact lineage")
	}
	final := filepath.Join(nativeDirectTreeResultRoot(t, root, intent), "file.bin")
	if err := os.WriteFile(final, []byte("user"), 0o600); err != nil {
		t.Fatal(err)
	}
	platform, err := openNativeOutputPlatform(filepath.Dir(final), false)
	if err != nil {
		t.Fatal(err)
	}
	published, err := store.PublishOwnedNoReplace(ctx, record.OwnedObjectID(), 4, platform.Root(), "file.bin")
	if published != nil || !errors.Is(err, outputcap.ErrNamespaceCollision) {
		t.Fatalf("publication collision: %v %v", published, err)
	}
	if err := platform.Close(); err != nil {
		t.Fatal(err)
	}
	closeRepository()

	// A definite no-replace collision must restore ownership before the live
	// handle closes, so a later discard can still remove only the staged data.
	recovery, err := NewFilesystemResumeStateAuthority(FilesystemResumeRoot{RootPath: root})
	if err != nil {
		t.Fatal(err)
	}
	result, err := recovery.Discard(ctx, intent.OperationID())
	if err != nil || result.State() != ResumeOperationDiscarded {
		t.Fatalf("discard after publication collision: %v %v", result.State(), err)
	}
	if _, err := os.Stat(stage); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("owned partial remains after discard: %v", err)
	}
	if actual, err := os.ReadFile(final); err != nil || string(actual) != "user" {
		t.Fatalf("discard changed collision: %q %v", actual, err)
	}
}
