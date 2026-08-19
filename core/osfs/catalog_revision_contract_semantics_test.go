package osfs

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/windshare/windshare/core/catalog"
	"github.com/windshare/windshare/core/content"
)

func deterministicCatalogIdentities() CatalogIdentitySource {
	var next byte = 20
	return CatalogIdentitySourceFunc(func() ([catalog.IdentityBytes]byte, error) {
		var identity [catalog.IdentityBytes]byte
		identity[0] = next
		next++
		return identity, nil
	})
}

func selectedDirectorySource(t *testing.T, root string, syntheticByte byte) (*SelectedCatalogSource, catalog.NodeRecord) {
	t.Helper()
	synthetic, err := catalog.DirectoryIDFromBytes(identity16(syntheticByte))
	if err != nil {
		t.Fatal(err)
	}
	source, err := NewSelectedCatalogSource(SelectedCatalogSourceConfig{
		Paths: []string{root}, SyntheticRoot: synthetic, Identities: deterministicCatalogIdentities(),
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = source.Close() })
	return source, source.SelectedRoots()[0]
}

func TestSelectedCatalogSourcePreservesAuthorityAcrossNestedScans(t *testing.T) {
	var nilSource *SelectedCatalogSource
	if err := nilSource.Close(); err != nil {
		t.Fatal(err)
	}

	root := t.TempDir()
	childPath := filepath.Join(root, "child")
	if err := os.Mkdir(childPath, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(childPath, "nested.bin"), []byte("nested"), 0o600); err != nil {
		t.Fatal(err)
	}
	source, selected := selectedDirectorySource(t, root, 31)
	owned := source.SelectedRoots()
	owned[0] = catalog.NodeRecord{}
	if source.SelectedRoots()[0].NodeID().IsZero() {
		t.Fatal("caller mutation changed selected-root authority")
	}

	children := &collectingScanChildren{}
	if _, err := source.ScanDirectory(context.Background(), catalog.ScanRequest{
		Directory: selected, Work: &countingScanWork{}, Children: children,
	}); err != nil {
		t.Fatal(err)
	}
	if len(children.items) != 1 || children.items[0].DirectoryID.IsZero() {
		t.Fatalf("first generation children = %+v", children.items)
	}
	parent, _ := selected.DirectoryID()
	child := children.items[0]
	childRecord, err := catalog.NewDirectoryNodeRecord(
		child.DirectoryID, parent, child.Name, child.Locator, child.SourceIdentity, child.ModifiedTime,
	)
	if err != nil {
		t.Fatal(err)
	}
	nested := &collectingScanChildren{}
	if _, err := source.ScanDirectory(context.Background(), catalog.ScanRequest{
		Directory: childRecord, Work: &countingScanWork{}, Children: nested,
	}); err != nil {
		t.Fatal(err)
	}
	if len(nested.items) != 1 || nested.items[0].Locator.RelativePath() != "child/nested.bin" {
		t.Fatalf("nested locator authority = %+v", nested.items)
	}

	foreignRoot := t.TempDir()
	foreign, foreignRecord := selectedDirectorySource(t, foreignRoot, 32)
	defer foreign.Close()
	if _, err := source.ScanDirectory(context.Background(), catalog.ScanRequest{
		Directory: foreignRecord, Work: &countingScanWork{}, Children: &collectingScanChildren{},
	}); !errors.Is(err, catalog.ErrDirectoryStale) {
		t.Fatalf("foreign source identity error = %v", err)
	}
}

func TestSelectedCatalogSourceClassifiesFilesystemAndIdentityFailures(t *testing.T) {
	sentinel := errors.New("injected identity failure")
	invalidName := filepath.Join(t.TempDir(), "invalid\u200b.bin")
	if err := os.WriteFile(invalidName, []byte("invalid"), 0o600); err != nil {
		t.Fatal(err)
	}
	invalidSynthetic, _ := catalog.DirectoryIDFromBytes(identity16(37))
	if invalid, err := NewSelectedCatalogSource(SelectedCatalogSourceConfig{
		Paths: []string{invalidName}, SyntheticRoot: invalidSynthetic,
	}); err == nil {
		_ = invalid.Close()
		t.Fatal("selected file with a non-portable locator acquired authority")
	}

	selectedFile := filepath.Join(t.TempDir(), "selected.bin")
	if err := os.WriteFile(selectedFile, []byte("selected"), 0o600); err != nil {
		t.Fatal(err)
	}
	synthetic, _ := catalog.DirectoryIDFromBytes(identity16(33))
	if _, err := NewSelectedCatalogSource(SelectedCatalogSourceConfig{
		Paths: []string{selectedFile}, SyntheticRoot: synthetic,
		Identities: CatalogIdentitySourceFunc(func() ([catalog.IdentityBytes]byte, error) {
			return [catalog.IdentityBytes]byte{}, sentinel
		}),
	}); !errors.Is(err, sentinel) {
		t.Fatalf("selected-file identity error = %v", err)
	}

	root := t.TempDir()
	source, selected := selectedDirectorySource(t, root, 34)
	if err := source.roots[0].Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := source.ScanDirectory(context.Background(), catalog.ScanRequest{
		Directory: selected, Work: &countingScanWork{}, Children: &collectingScanChildren{},
	}); err == nil {
		t.Fatal("scan through a closed root authority succeeded")
	}

	handle, err := os.Open(root)
	if err != nil {
		t.Fatal(err)
	}
	defer handle.Close()
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := catalogDirectoryFingerprint(cancelled, handle); !errors.Is(err, context.Canceled) {
		t.Fatalf("fingerprint cancellation error = %v", err)
	}

	directRoot := t.TempDir()
	if err := os.WriteFile(filepath.Join(directRoot, "child.txt"), []byte("child"), 0o600); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(directRoot)
	if err != nil {
		t.Fatal(err)
	}
	authority, err := os.OpenRoot(directRoot)
	if err != nil {
		t.Fatal(err)
	}
	defer authority.Close()
	if _, _, err := source.scanChild(cancelled, authority, catalog.Locator{}, entries[0]); !errors.Is(err, context.Canceled) {
		t.Fatalf("child cancellation error = %v", err)
	}
	components := make([]string, 163)
	for index := range components {
		components[index] = strings.Repeat("a", 200)
	}
	parentLocator, err := catalog.NewLocator(0, strings.Join(components, "/"))
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := source.scanChild(context.Background(), authority, parentLocator, entries[0]); !errors.Is(err, catalog.ErrInvalidPath) {
		t.Fatalf("expanded child locator error = %v", err)
	}
	closedHandle, err := os.Open(directRoot)
	if err != nil {
		t.Fatal(err)
	}
	if err := closedHandle.Close(); err != nil {
		t.Fatal(err)
	}
	request := catalog.ScanRequest{Work: &countingScanWork{}, Children: &collectingScanChildren{}}
	if _, _, err := source.enumerateCatalogChildren(context.Background(), authority, closedHandle, catalog.Locator{}, request); err == nil {
		t.Fatal("enumeration classified a closed directory stream as exhaustion")
	}
	if _, err := catalogDirectoryFingerprint(context.Background(), closedHandle); err == nil {
		t.Fatal("fingerprint classified a closed directory stream as exhaustion")
	}
}

func TestSelectedCatalogSourceOmitsSymlinksWithoutPublishingAuthority(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "target.bin"), []byte("target"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("target.bin", filepath.Join(root, "link.bin")); err != nil {
		t.Skipf("symlink creation unavailable: %v", err)
	}
	source, selected := selectedDirectorySource(t, root, 35)
	children := &collectingScanChildren{}
	result, err := source.ScanDirectory(context.Background(), catalog.ScanRequest{
		Directory: selected, Work: &countingScanWork{}, Children: children,
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.OmittedCount != 1 || len(children.items) != 1 || children.items[0].Name != "target.bin" {
		t.Fatalf("symlink omission = %+v, children=%+v", result, children.items)
	}

	synthetic, _ := catalog.DirectoryIDFromBytes(identity16(36))
	if leaked, err := NewSelectedCatalogSource(SelectedCatalogSourceConfig{
		Paths: []string{filepath.Join(root, "link.bin")}, SyntheticRoot: synthetic,
	}); err == nil {
		_ = leaked.Close()
		t.Fatal("selected symlink acquired catalog authority")
	}
}

type errorOwnedStabilityBinder struct{ closeErr error }

func (*errorOwnedStabilityBinder) BindStable(_ context.Context, binding StableBinding) (content.StableFile, error) {
	return &boundTestFile{file: binding.File}, nil
}

func (binder *errorOwnedStabilityBinder) Close() error { return binder.closeErr }

func emptyLocatorFileRecord(t *testing.T) catalog.NodeRecord {
	t.Helper()
	locator, err := catalog.NewLocator(0, "")
	if err != nil {
		t.Fatal(err)
	}
	file, _ := catalog.FileIDFromBytes(identity16(40))
	parent, _ := catalog.DirectoryIDFromBytes(identity16(41))
	identity, _ := catalog.NewSourceIdentity([]byte("empty-locator-identity"))
	candidate, _ := catalog.NewVersionCandidate([]byte("empty-locator-candidate"))
	record, err := catalog.NewFileNodeRecord(
		file, parent, "empty.bin", locator, identity, candidate, 0, catalog.ModifiedTime{},
	)
	if err != nil {
		t.Fatal(err)
	}
	return record
}

func TestRootedRevisionSourceTransfersOnlySuccessfullyBoundHandles(t *testing.T) {
	root := t.TempDir()
	filename := filepath.Join(root, "file.bin")
	if err := os.WriteFile(filename, []byte("data"), 0o600); err != nil {
		t.Fatal(err)
	}

	var rejectedHandle *os.File
	sentinel := errors.New("binder rejected revision")
	source, err := NewRootedRevisionSource(RootedRevisionSourceConfig{
		RootPaths: []string{root},
		Binder: StabilityBinderFunc(func(_ context.Context, binding StableBinding) (content.StableFile, error) {
			rejectedHandle = binding.File
			return nil, sentinel
		}),
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := source.OpenStable(context.Background(), rootedFileRecord(t, 0, "file.bin", 4)); !errors.Is(err, sentinel) {
		t.Fatalf("binder rejection error = %v", err)
	}
	if _, err := rejectedHandle.Stat(); err == nil {
		t.Fatal("failed binder retained an open file handle")
	}
	if _, err := source.OpenStable(context.Background(), emptyLocatorFileRecord(t)); !errors.Is(err, content.ErrRevisionStale) ||
		content.RevisionComparisonOf(err) != content.RevisionComparisonMismatch {
		t.Fatalf("empty locator error = %v comparison=%v", err, content.RevisionComparisonOf(err))
	}
	if _, err := source.OpenStable(context.Background(), rootedFileRecord(t, 0, "file.bin", 5)); !errors.Is(err, content.ErrRevisionStale) ||
		content.RevisionComparisonOf(err) != content.RevisionComparisonMismatch {
		t.Fatalf("size mismatch error = %v comparison=%v", err, content.RevisionComparisonOf(err))
	}
	if err := source.Close(); err != nil {
		t.Fatal(err)
	}

	success, err := NewRootedRevisionSource(RootedRevisionSourceConfig{
		RootPaths: []string{root},
		Binder: StabilityBinderFunc(func(_ context.Context, binding StableBinding) (content.StableFile, error) {
			return &boundTestFile{file: binding.File}, nil
		}),
	})
	if err != nil {
		t.Fatal(err)
	}
	stable, err := success.OpenStable(context.Background(), rootedFileRecord(t, 0, "file.bin", 4))
	if err != nil {
		t.Fatal(err)
	}
	if err := success.Close(); err != nil {
		t.Fatal(err)
	}
	buffer := make([]byte, 4)
	if count, err := stable.ReadAt(context.Background(), buffer, 0); err != nil || count != 4 || string(buffer) != "data" {
		t.Fatalf("transferred handle read = %d %q, %v", count, buffer, err)
	}
	if err := stable.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestRootedRevisionSourceClassifiesRootAndOwnedBinderFailures(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "file.bin"), []byte("data"), 0o600); err != nil {
		t.Fatal(err)
	}
	source, err := NewRootedRevisionSource(RootedRevisionSourceConfig{
		RootPaths: []string{root},
		Binder: StabilityBinderFunc(func(_ context.Context, binding StableBinding) (content.StableFile, error) {
			return &boundTestFile{file: binding.File}, nil
		}),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := source.roots[0].Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := source.OpenStable(context.Background(), rootedFileRecord(t, 0, "file.bin", 4)); err == nil ||
		content.RevisionComparisonOf(err) != content.RevisionComparisonUnavailable {
		t.Fatalf("closed root authority error=%v comparison=%v", err, content.RevisionComparisonOf(err))
	}
	_ = source.Close()

	sentinel := errors.New("owned binder close failure")
	binder := &errorOwnedStabilityBinder{closeErr: sentinel}
	owned, err := newOwnedRootedRevisionSource([]string{root}, binder)
	if err != nil {
		t.Fatal(err)
	}
	if err := owned.Close(); !errors.Is(err, sentinel) {
		t.Fatalf("owned binder close error = %v", err)
	}
}
