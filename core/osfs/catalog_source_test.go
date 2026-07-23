package osfs

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"

	"github.com/windshare/windshare/core/catalog"
)

func TestSelectedCatalogSourceDefersDescendantEnumerationUntilScan(t *testing.T) {
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, "child-dir"), 0o700); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"a.txt", "b.txt"} {
		if err := os.WriteFile(filepath.Join(root, name), []byte(name), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	synthetic, _ := catalog.DirectoryIDFromBytes(identity16(1))
	var identities atomic.Uint32
	source, err := NewSelectedCatalogSource(SelectedCatalogSourceConfig{
		Paths: []string{root}, SyntheticRoot: synthetic,
		Identities: CatalogIdentitySourceFunc(func() ([catalog.IdentityBytes]byte, error) {
			var identity [catalog.IdentityBytes]byte
			identity[0] = byte(identities.Add(1) + 1)
			return identity, nil
		}),
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = source.Close() })
	if got := identities.Load(); got != 1 {
		t.Fatalf("ready path allocated %d node identities; descendants were enumerated", got)
	}
	selected := source.SelectedRoots()
	if len(selected) != 1 {
		t.Fatalf("selected roots = %d", len(selected))
	}
	work := &countingScanWork{}
	children := &collectingScanChildren{}
	result, err := source.ScanDirectory(context.Background(), catalog.ScanRequest{
		Directory: selected[0], Work: work, Children: children,
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.OmittedCount != 0 || len(children.items) != 3 || work.units != 3 || identities.Load() != 4 {
		t.Fatalf("scan result=%+v children=%d work=%d identities=%d", result, len(children.items), work.units, identities.Load())
	}
	for _, child := range children.items {
		if child.Locator.RelativePath() == "" {
			t.Fatalf("child lost private locator authority: %+v", child)
		}
		if child.DirectoryID.IsZero() && (child.FileID.IsZero() || child.VersionCandidate.IsZero()) {
			t.Fatalf("file child lost private revision authority: %+v", child)
		}
	}
}

func TestSelectedFileRootBuildsStableRevisionAuthority(t *testing.T) {
	filename := filepath.Join(t.TempDir(), "selected.bin")
	payload := []byte("stable selected file")
	if err := os.WriteFile(filename, payload, 0o600); err != nil {
		t.Fatal(err)
	}
	synthetic, _ := catalog.DirectoryIDFromBytes(identity16(9))
	source, err := NewSelectedCatalogSource(SelectedCatalogSourceConfig{Paths: []string{filename}, SyntheticRoot: synthetic})
	if err != nil {
		t.Fatal(err)
	}
	revisions, err := source.RevisionSource()
	if err != nil {
		t.Fatal(err)
	}
	selected := source.SelectedRoots()
	stable, err := revisions.OpenStable(context.Background(), selected[0])
	if err != nil {
		t.Fatal(err)
	}
	buffer := make([]byte, len(payload))
	if count, err := stable.ReadAt(context.Background(), buffer, 0); err != nil || count != len(payload) || string(buffer) != string(payload) {
		t.Fatalf("stable read = %d %q, %v", count, buffer, err)
	}
	if err := stable.Verify(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := stable.Close(); err != nil {
		t.Fatal(err)
	}
	if err := revisions.Close(); err != nil {
		t.Fatal(err)
	}
	if err := source.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := source.RevisionSource(); err == nil {
		t.Fatal("closed selected source created revision authority")
	}
	if err := source.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestSelectedCatalogSourceRejectsInvalidConfigurationAndIdentityExhaustion(t *testing.T) {
	synthetic, _ := catalog.DirectoryIDFromBytes(identity16(8))
	if _, err := NewSelectedCatalogSource(SelectedCatalogSourceConfig{SyntheticRoot: synthetic}); err == nil {
		t.Fatal("empty selected roots were accepted")
	}
	if _, err := NewSelectedCatalogSource(SelectedCatalogSourceConfig{Paths: []string{t.TempDir()}}); err == nil {
		t.Fatal("zero synthetic root was accepted")
	}
	if _, err := (CatalogIdentitySourceFunc(nil)).NewCatalogIdentity(); err == nil {
		t.Fatal("nil identity function was accepted")
	}
	if _, err := NewSelectedCatalogSource(SelectedCatalogSourceConfig{
		Paths: []string{t.TempDir()}, SyntheticRoot: synthetic,
		Identities: CatalogIdentitySourceFunc(func() ([catalog.IdentityBytes]byte, error) {
			return [catalog.IdentityBytes]byte{}, nil
		}),
	}); err == nil {
		t.Fatal("zero identity exhaustion was accepted")
	}
}

func TestSelectedCatalogSourceRejectsInvalidAndClosedScan(t *testing.T) {
	synthetic, _ := catalog.DirectoryIDFromBytes(identity16(7))
	source, err := NewSelectedCatalogSource(SelectedCatalogSourceConfig{Paths: []string{t.TempDir()}, SyntheticRoot: synthetic})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := source.ScanDirectory(context.Background(), catalog.ScanRequest{}); err == nil {
		t.Fatal("invalid scan request was accepted")
	}
	selected := source.SelectedRoots()[0]
	if err := source.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := source.ScanDirectory(context.Background(), catalog.ScanRequest{
		Directory: selected, Work: &countingScanWork{}, Children: &collectingScanChildren{},
	}); err == nil {
		t.Fatal("closed scan authority was accepted")
	}
}

func TestSelectedCatalogScannerPropagatesWorkSinkAndMutationFailures(t *testing.T) {
	sentinel := errors.New("injected scan boundary")
	newSource := func(t *testing.T, identities CatalogIdentitySource) (string, *SelectedCatalogSource, catalog.NodeRecord) {
		t.Helper()
		root := t.TempDir()
		if err := os.WriteFile(filepath.Join(root, "child.txt"), []byte("child"), 0o600); err != nil {
			t.Fatal(err)
		}
		synthetic, _ := catalog.DirectoryIDFromBytes(identity16(6))
		source, err := NewSelectedCatalogSource(SelectedCatalogSourceConfig{
			Paths: []string{root}, SyntheticRoot: synthetic, Identities: identities,
		})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = source.Close() })
		return root, source, source.SelectedRoots()[0]
	}

	_, workSource, workRoot := newSource(t, nil)
	if _, err := workSource.ScanDirectory(context.Background(), catalog.ScanRequest{
		Directory: workRoot, Work: errorScanWork{err: sentinel}, Children: &collectingScanChildren{},
	}); !errors.Is(err, sentinel) {
		t.Fatalf("work failure = %v", err)
	}

	_, sinkSource, sinkRoot := newSource(t, nil)
	if _, err := sinkSource.ScanDirectory(context.Background(), catalog.ScanRequest{
		Directory: sinkRoot, Work: &countingScanWork{}, Children: errorScanChildren{err: sentinel},
	}); !errors.Is(err, sentinel) {
		t.Fatalf("sink failure = %v", err)
	}

	mutationRoot, mutationSource, mutationRecord := newSource(t, nil)
	if _, err := mutationSource.ScanDirectory(context.Background(), catalog.ScanRequest{
		Directory: mutationRecord, Work: &countingScanWork{},
		Children: &mutatingScanChildren{root: mutationRoot},
	}); !errors.Is(err, catalog.ErrDirectoryStale) {
		t.Fatalf("enumeration mutation failure = %v", err)
	}

	deleteRoot, deleteSource, deleteRecord := newSource(t, nil)
	if _, err := deleteSource.ScanDirectory(context.Background(), catalog.ScanRequest{
		Directory: deleteRecord, Work: &deletingScanWork{path: filepath.Join(deleteRoot, "child.txt")},
		Children: &collectingScanChildren{},
	}); err == nil {
		t.Fatal("child deletion race was accepted")
	}

	var identityCalls atomic.Int32
	_, identitySource, identityRecord := newSource(t, CatalogIdentitySourceFunc(func() ([catalog.IdentityBytes]byte, error) {
		if identityCalls.Add(1) == 1 {
			var identity [catalog.IdentityBytes]byte
			identity[0] = 44
			return identity, nil
		}
		return [catalog.IdentityBytes]byte{}, sentinel
	}))
	if _, err := identitySource.ScanDirectory(context.Background(), catalog.ScanRequest{
		Directory: identityRecord, Work: &countingScanWork{}, Children: &collectingScanChildren{},
	}); !errors.Is(err, sentinel) {
		t.Fatalf("identity failure = %v", err)
	}

	_, cancelledSource, cancelledRecord := newSource(t, nil)
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := cancelledSource.ScanDirectory(cancelled, catalog.ScanRequest{
		Directory: cancelledRecord, Work: &countingScanWork{}, Children: &collectingScanChildren{},
	}); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled scan = %v", err)
	}
}

func TestSelectedCatalogSourceReportsMissingRoot(t *testing.T) {
	synthetic, _ := catalog.DirectoryIDFromBytes(identity16(5))
	if _, err := NewSelectedCatalogSource(SelectedCatalogSourceConfig{
		Paths: []string{filepath.Join(t.TempDir(), "missing")}, SyntheticRoot: synthetic,
	}); err == nil {
		t.Fatal("missing selected root was accepted")
	}
}

type errorScanWork struct{ err error }

func (work errorScanWork) Consume(uint64) error { return work.err }

type errorScanChildren struct{ err error }

func (children errorScanChildren) Add(context.Context, catalog.ScannedChild) error {
	return children.err
}

type mutatingScanChildren struct {
	root    string
	mutated bool
}

func (children *mutatingScanChildren) Add(context.Context, catalog.ScannedChild) error {
	if !children.mutated {
		children.mutated = true
		return os.WriteFile(filepath.Join(children.root, "added-during-scan.txt"), []byte("mutation"), 0o600)
	}
	return nil
}

type deletingScanWork struct {
	path    string
	deleted bool
}

func (work *deletingScanWork) Consume(uint64) error {
	if work.deleted {
		return nil
	}
	work.deleted = true
	return os.Remove(work.path)
}

type countingScanWork struct{ units uint64 }

func (work *countingScanWork) Consume(units uint64) error {
	work.units += units
	return nil
}

type collectingScanChildren struct{ items []catalog.ScannedChild }

func (children *collectingScanChildren) Add(_ context.Context, child catalog.ScannedChild) error {
	children.items = append(children.items, child)
	return nil
}

func identity16(value byte) []byte {
	identity := make([]byte, catalog.IdentityBytes)
	identity[0] = value
	return identity
}
