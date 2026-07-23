package osfs

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/windshare/windshare/core/catalog"
	"github.com/windshare/windshare/core/content"
)

type boundTestFile struct{ file *os.File }

func (f *boundTestFile) ExactSize() uint64 {
	info, _ := f.file.Stat()
	return uint64(info.Size())
}
func (f *boundTestFile) ModifiedTime() catalog.ModifiedTime { return catalog.ModifiedTime{} }
func (f *boundTestFile) Verify(context.Context) error       { return nil }
func (f *boundTestFile) ReadAt(_ context.Context, destination []byte, offset uint64) (int, error) {
	return f.file.ReadAt(destination, int64(offset))
}
func (f *boundTestFile) Close() error { return f.file.Close() }

type ownedTestStabilityBinder struct{ closes int }

func (*ownedTestStabilityBinder) BindStable(_ context.Context, binding StableBinding) (content.StableFile, error) {
	return &boundTestFile{file: binding.File}, nil
}

func (b *ownedTestStabilityBinder) Close() error {
	b.closes++
	return nil
}

func rootedFileRecord(t *testing.T, slot catalog.RootSlot, relativePath string, size uint64) catalog.NodeRecord {
	t.Helper()
	var file catalog.FileID
	file[0] = 1
	var parent catalog.DirectoryID
	parent[0] = 2
	locator, err := catalog.NewLocator(slot, relativePath)
	if err != nil {
		t.Fatal(err)
	}
	identity, _ := catalog.NewSourceIdentity([]byte("identity"))
	candidate, _ := catalog.NewVersionCandidate([]byte("candidate"))
	record, err := catalog.NewFileNodeRecord(file, parent, filepath.Base(relativePath), locator, identity, candidate, size, catalog.ModifiedTime{})
	if err != nil {
		t.Fatal(err)
	}
	return record
}

func TestRootedRevisionSourceKeepsLocatorRootConfined(t *testing.T) {
	firstRoot := t.TempDir()
	secondRoot := t.TempDir()
	if err := os.WriteFile(filepath.Join(firstRoot, "same.bin"), []byte("first"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(secondRoot, "same.bin"), []byte("second"), 0o600); err != nil {
		t.Fatal(err)
	}
	var bindings int
	source, err := NewRootedRevisionSource(RootedRevisionSourceConfig{
		RootPaths: []string{firstRoot, secondRoot},
		Binder: StabilityBinderFunc(func(_ context.Context, binding StableBinding) (content.StableFile, error) {
			bindings++
			return &boundTestFile{file: binding.File}, nil
		}),
	})
	if err != nil {
		t.Fatal(err)
	}
	defer source.Close()
	stable, err := source.OpenStable(context.Background(), rootedFileRecord(t, 1, "same.bin", 6))
	if err != nil {
		t.Fatal(err)
	}
	defer stable.Close()
	data := make([]byte, 6)
	if count, err := stable.ReadAt(context.Background(), data, 0); err != nil && !(err == io.EOF && count == len(data)) {
		t.Fatal(err)
	}
	if string(data) != "second" || bindings != 1 {
		t.Fatalf("bound data = %q, bindings=%d", data, bindings)
	}
}

func TestRootedRevisionSourceFailsClosedWithoutStabilityProof(t *testing.T) {
	if _, err := NewRootedRevisionSource(RootedRevisionSourceConfig{RootPaths: []string{t.TempDir()}}); !errors.Is(err, content.ErrUnsupportedStability) {
		t.Fatalf("missing binder error = %v", err)
	}
	source, err := NewRootedRevisionSource(RootedRevisionSourceConfig{
		RootPaths: []string{t.TempDir()},
		Binder: StabilityBinderFunc(func(context.Context, StableBinding) (content.StableFile, error) {
			return nil, content.ErrUnsupportedStability
		}),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := source.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := source.OpenStable(context.Background(), rootedFileRecord(t, 0, "missing", 0)); !errors.Is(err, content.ErrRevisionStoreClosed) {
		t.Fatalf("closed source error = %v", err)
	}
}

func TestRootedRevisionSourceOwnsExplicitlyCloseableBinder(t *testing.T) {
	binder := &ownedTestStabilityBinder{}
	source, err := newOwnedRootedRevisionSource([]string{t.TempDir()}, binder)
	if err != nil {
		t.Fatal(err)
	}
	if err := source.Close(); err != nil {
		t.Fatal(err)
	}
	if err := source.Close(); err != nil || binder.closes != 1 {
		t.Fatalf("second close=%v binder closes=%d", err, binder.closes)
	}

	failureBinder := &ownedTestStabilityBinder{}
	notDirectory := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(notDirectory, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := newOwnedRootedRevisionSource([]string{t.TempDir(), filepath.Join(notDirectory, "child")}, failureBinder); err == nil || failureBinder.closes != 1 {
		t.Fatalf("constructor error=%v binder closes=%d", err, failureBinder.closes)
	}
}

func TestRootedRevisionSourceRejectsInvalidRootsRecordsAndBinders(t *testing.T) {
	binder := StabilityBinderFunc(func(context.Context, StableBinding) (content.StableFile, error) { return nil, nil })
	if _, err := NewRootedRevisionSource(RootedRevisionSourceConfig{Binder: binder}); err == nil {
		t.Fatal("empty root set accepted")
	}
	tooMany := make([]string, int(catalog.MaxRootSlots)+1)
	if _, err := NewRootedRevisionSource(RootedRevisionSourceConfig{RootPaths: tooMany, Binder: binder}); err == nil {
		t.Fatal("oversized root set accepted")
	}

	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "file.bin"), []byte("data"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(root, "directory"), 0o700); err != nil {
		t.Fatal(err)
	}
	source, err := NewRootedRevisionSource(RootedRevisionSourceConfig{RootPaths: []string{root}, Binder: binder})
	if err != nil {
		t.Fatal(err)
	}
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := source.OpenStable(canceled, rootedFileRecord(t, 0, "file.bin", 4)); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled open=%v", err)
	}
	if _, err := source.OpenStable(context.Background(), catalog.NodeRecord{}); !errors.Is(err, content.ErrRevisionNotFound) {
		t.Fatalf("non-file record=%v", err)
	}
	for _, record := range []catalog.NodeRecord{
		rootedFileRecord(t, 1, "file.bin", 4),
		rootedFileRecord(t, 0, "missing.bin", 4),
		rootedFileRecord(t, 0, "directory", 0),
	} {
		if _, err := source.OpenStable(context.Background(), record); !errors.Is(err, content.ErrRevisionStale) {
			t.Fatalf("invalid record error=%v", err)
		}
	}
	if _, err := source.OpenStable(context.Background(), rootedFileRecord(t, 0, "file.bin", 4)); err == nil {
		t.Fatal("nil stable file admitted")
	}
	_ = source.Close()

	sentinel := errors.New("binder rejected revision")
	source, err = NewRootedRevisionSource(RootedRevisionSourceConfig{
		RootPaths: []string{root},
		Binder: StabilityBinderFunc(func(context.Context, StableBinding) (content.StableFile, error) {
			return nil, sentinel
		}),
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := source.OpenStable(context.Background(), rootedFileRecord(t, 0, "file.bin", 4)); !errors.Is(err, sentinel) {
		t.Fatalf("binder error=%v", err)
	}
	_ = source.Close()
}

func TestRootedRevisionSourceUsesPrivateNativeLocatorSpelling(t *testing.T) {
	root := t.TempDir()
	decomposed := "Cafe\u0301.bin"
	composed := "Café.bin"
	decomposedPath := filepath.Join(root, decomposed)
	composedPath := filepath.Join(root, composed)
	if err := os.WriteFile(decomposedPath, []byte("native"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(composedPath, []byte("public"), 0o600); err != nil {
		t.Fatal(err)
	}
	decomposedInfo, err := os.Stat(decomposedPath)
	if err != nil {
		t.Fatal(err)
	}
	composedInfo, err := os.Stat(composedPath)
	if err != nil {
		t.Fatal(err)
	}
	if os.SameFile(decomposedInfo, composedInfo) {
		t.Skip("filesystem aliases canonically equivalent Unicode spellings")
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
	defer source.Close()
	stable, err := source.OpenStable(context.Background(), rootedFileRecord(t, 0, decomposed, 6))
	if err != nil {
		t.Fatal(err)
	}
	defer stable.Close()
	data := make([]byte, 6)
	if _, err := stable.ReadAt(context.Background(), data, 0); err != nil {
		t.Fatal(err)
	}
	if string(data) != "native" {
		t.Fatalf("private locator reopened canonical sibling: %q", data)
	}
}

func TestRootedRevisionSourceRejectsFinalSymlinkBeforeBinding(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "target.bin"), []byte("target"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("target.bin", filepath.Join(root, "link.bin")); err != nil {
		t.Skipf("symlink creation unavailable: %v", err)
	}
	var bindings int
	source, err := NewRootedRevisionSource(RootedRevisionSourceConfig{
		RootPaths: []string{root},
		Binder: StabilityBinderFunc(func(_ context.Context, binding StableBinding) (content.StableFile, error) {
			bindings++
			return &boundTestFile{file: binding.File}, nil
		}),
	})
	if err != nil {
		t.Fatal(err)
	}
	defer source.Close()
	if _, err := source.OpenStable(context.Background(), rootedFileRecord(t, 0, "link.bin", 6)); !errors.Is(err, content.ErrRevisionStale) {
		t.Fatalf("final symlink open = %v", err)
	}
	if bindings != 0 {
		t.Fatal("rejected symlink reached the stability binder")
	}
}
