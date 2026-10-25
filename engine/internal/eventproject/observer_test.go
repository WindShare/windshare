package eventproject

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/windshare/windshare/core/catalog"
	"github.com/windshare/windshare/core/content/revisioncapacity"
	"github.com/windshare/windshare/core/liveshare"
	"github.com/windshare/windshare/core/session/contentflow"
	appobs "github.com/windshare/windshare/engine/observation"
	"github.com/windshare/windshare/osfs"
)

func TestSharingSubjectProjectionUsesFrozenHumanDisplayFact(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "selected.bin")
	if err := os.WriteFile(path, []byte("selected-content"), 0o600); err != nil {
		t.Fatal(err)
	}
	capacityOwner, err := revisioncapacity.NewProcessOwner(revisioncapacity.DefaultProcessConfig())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := capacityOwner.Close(); err != nil {
			t.Errorf("close revision capacity owner: %v", err)
		}
	})
	sender, err := liveshare.PrepareSender(context.Background(), sourceBoundarySenderConfig([]string{path}, liveshare.SenderConfig{
		Relays:           []string{"ws://127.0.0.1:8484"},
		ChunkSize:        catalog.MinChunkSize,
		RevisionCapacity: capacityOwner.Coordinator(),
		CatalogStorage: liveshare.CatalogStorageFactoryFunc(func(context.Context, catalog.ShareInstance) (catalog.CatalogBackend, error) {
			return catalog.NewMemoryCatalogBackend(), nil
		}),
	}))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sender.Close() })

	projected, err := ProjectSharingSubject(sender.SelectedRootSummary())
	if err != nil {
		t.Fatal(err)
	}
	subject := projected.Subject()
	if subject.Kind() != appobs.SharingFile || subject.Name().Text() != filepath.Base(path) {
		t.Fatalf("sharing subject = %#v", subject)
	}
	if size := subject.FileBytes(); size != uint64(len("selected-content")) {
		t.Fatalf("sharing file size = %d", size)
	}
	if _, err := ProjectSharingSubject(liveshare.SelectedRootSummary{}); err == nil {
		t.Fatal("empty selected-root summary was projected")
	}
}

func sourceBoundarySenderConfig(paths []string, config liveshare.SenderConfig) liveshare.SenderConfig {
	selected := append([]string(nil), paths...)
	config.Source = liveshare.FileSourceFactoryFunc(func(ctx context.Context, source liveshare.FileSourceContext) (liveshare.FileSource, error) {
		return osfs.NewSelectedFileSource(ctx, osfs.SelectedCatalogSourceConfig{Paths: selected, SyntheticRoot: source.SyntheticRoot, Identities: osfs.CatalogIdentitySourceFunc(source.NewIdentity)})
	})
	var err error
	config.CatalogBudget, err = catalog.NewBudgetAccount("test-process", catalog.DefaultProcessBudgetLimits())
	if err != nil {
		panic(err)
	}
	config.CacheBudget, err = contentflow.NewProcessCacheBudget(uint64(64) << 20)
	if err != nil {
		panic(err)
	}
	return config
}
