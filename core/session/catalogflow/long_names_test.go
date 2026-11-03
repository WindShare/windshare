package catalogflow

import (
	"bytes"
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"github.com/windshare/windshare/core/catalog"
)

func TestLongNamesFitSealedPagesAndSurviveRestart(t *testing.T) {
	for _, scenario := range []struct {
		name       string
		nameBytes  int
		childCount int
	}{
		{"native-byte-limit", 255, catalog.MaxCatalogPageEntries},
		{"metadata-limit", catalog.MaxNameBytes, 32},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			ctx := context.Background()
			fixture := newCatalogObjectFixture(t)
			disk := t.TempDir()
			openStore := func() (*catalog.CatalogStore, *catalog.FileCatalogBackend) {
				backend, err := catalog.NewFileCatalogBackend(catalog.FileCatalogBackendConfig{
					Root: filepath.Join(disk, "catalog"), ShareInstance: fixture.share,
				})
				if err != nil {
					t.Fatal(err)
				}
				process, _ := catalog.NewBudgetAccount("process", catalog.DefaultProcessBudgetLimits())
				share, _ := catalog.NewBudgetAccount("share", catalog.DefaultShareBudgetLimits())
				store, err := catalog.NewCatalogStore(catalog.StoreConfig{
					ShareInstance: fixture.share, Backend: backend, ProcessBudget: process, ShareBudget: share,
					PageSealer:   newObjectStore(t, fixture, &nonceCounter{}),
					SpillFactory: catalog.NewFileSpillFactory(filepath.Join(disk, "sort")),
				})
				if err != nil {
					t.Fatal(err)
				}
				return store, backend
			}
			store, backend := openStore()
			defer store.Close()
			session, _ := catalog.NewBudgetAccount("session", catalog.DefaultSessionBudgetLimits())
			root, directory := directoryID(t, 1), directoryID(t, 2)
			commitSelectedDirectories(t, store, fixture.share, root, []catalog.DirectoryID{directory}, session)
			scanner := catalog.DirectoryScannerFunc(func(ctx context.Context, request catalog.ScanRequest) (catalog.ScanResult, error) {
				for index := range scenario.childCount {
					name := fmt.Sprintf("%04d-%s", index, strings.Repeat("n", scenario.nameBytes-5))
					locator, _ := catalog.NewSourceReference(fmt.Appendf(nil, "object:%d", index))
					identity, _ := catalog.NewSourceIdentity(locator.Bytes())
					version, _ := catalog.NewVersionCandidate([]byte("revision"))
					var idBytes [catalog.IdentityBytes]byte
					idBytes[0], idBytes[1] = byte(index), byte(index>>8)+1
					id, _ := catalog.FileIDFromBytes(idBytes[:])
					if err := request.Children.Add(ctx, catalog.ScannedChild{
						FileID: id, Name: name, SourceReference: locator,
						SourceIdentity: identity, VersionCandidate: version,
						ExpectedSize: catalog.MaxSafeInteger,
					}); err != nil {
						return catalog.ScanResult{}, err
					}
				}
				return catalog.ScanResult{}, nil
			})
			committed, err := store.ListChildren(ctx, directory, session, catalog.ScanOptions{}, scanner)
			if err != nil {
				t.Fatal(err)
			}
			if committed.EntryCount() != uint64(scenario.childCount) || committed.PageCount() < 2 {
				t.Fatalf("long-name pages did not split by byte budget: %+v", committed)
			}
			verifier, err := NewCatalogObjectVerifier(CatalogObjectVerifierConfig{
				ShareInstance: fixture.share, CatalogKey: fixture.key, Sender: checkedSender(t, fixture.publicKey),
			})
			if err != nil {
				t.Fatal(err)
			}
			generation := committed.Generation()
			before := make([][]byte, committed.PageCount())
			total := 0
			for index := range committed.PageCount() {
				object, found, err := store.PageObject(ctx, directory, generation, index)
				if err != nil || !found || len(object.Bytes()) > catalog.MaxCatalogPageObjectBytes {
					t.Fatalf("page %d exceeds the sealed-object budget: found=%v err=%v", index, found, err)
				}
				before[index] = object.Bytes()
				request, _ := NewListRequest(directory, &generation, index)
				verified, err := verifier.Verify(ctx, fixture.share, request, before[index])
				if err != nil {
					t.Fatal(err)
				}
				total += len(verified.Page.Entries())
			}
			if total != scenario.childCount {
				t.Fatalf("paged catalog lost entries: got %d", total)
			}
			// Reopen through disk metadata: variable page counts must retain the
			// exact authenticated objects without another scan or seal.
			if err := backend.Close(); err != nil {
				t.Fatal(err)
			}
			recovered, _ := openStore()
			defer recovered.Close()
			actual, found, err := recovered.Directory(ctx, directory)
			if err != nil || !found || actual != committed {
				t.Fatalf("recovered geometry changed: found=%v err=%v", found, err)
			}
			for index := range committed.PageCount() {
				object, found, err := recovered.PageObject(ctx, directory, generation, index)
				if err != nil || !found || !bytes.Equal(object.Bytes(), before[index]) {
					t.Fatalf("recovered page %d changed: found=%v err=%v", index, found, err)
				}
			}
		})
	}
}
