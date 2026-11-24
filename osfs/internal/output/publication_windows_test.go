//go:build windows

package output

import (
	"context"
	"encoding/hex"
	"errors"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/windshare/windshare/core/catalog"
	"github.com/windshare/windshare/core/content"
	"github.com/windshare/windshare/core/transfer"
	"github.com/windshare/windshare/core/transfer/ordinaryoutput"
	"github.com/windshare/windshare/core/transfer/receivecontract"
	"golang.org/x/sys/windows"
)

func TestWindowsModifiedTimePreservesNativePrecisionAndHandleIdentity(t *testing.T) {
	path := t.TempDir()
	root, _, err := OpenContainer(path, false)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	owned, err := openChildFile(root, "partial", os.O_RDWR|os.O_CREATE|os.O_EXCL, privateFileMode)
	if err != nil {
		t.Fatal(err)
	}
	defer owned.Close()
	if _, err := owned.WriteAt([]byte("data"), 0); err != nil {
		t.Fatal(err)
	}

	var before windows.ByHandleFileInformation
	if err := windows.GetFileInformationByHandle(windows.Handle(owned.Fd()), &before); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(filepath.Join(path, "partial"), filepath.Join(path, "moved")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(path, "partial"), []byte("foreign"), 0o600); err != nil {
		t.Fatal(err)
	}
	replacementBefore, err := os.Stat(filepath.Join(path, "partial"))
	if err != nil {
		t.Fatal(err)
	}
	// NTFS represents timestamps in 100 ns units.
	modified, err := catalog.NewModifiedTime(1_700_000_000, 123_456_700, catalog.TimePrecisionNanoseconds)
	if err != nil {
		t.Fatal(err)
	}
	if err := setFileModifiedTimeCatalog(owned, modified); err != nil {
		t.Fatal(err)
	}
	if matches, err := fileMetadataMatches(owned, 4, modified); err != nil || !matches {
		t.Fatalf("native timestamp lost precision: matches=%t err=%v", matches, err)
	}
	var after windows.ByHandleFileInformation
	if err := windows.GetFileInformationByHandle(windows.Handle(owned.Fd()), &after); err != nil {
		t.Fatal(err)
	}
	if after.LastAccessTime != before.LastAccessTime || after.CreationTime != before.CreationTime {
		t.Fatal("mtime update changed unrelated file times")
	}
	replacementAfter, err := os.Stat(filepath.Join(path, "partial"))
	if err != nil || !replacementAfter.ModTime().Equal(replacementBefore.ModTime()) {
		t.Fatalf("mtime update reached replacement file: %v", err)
	}
}

func TestRetryWindowsRenameBoundedBackoffPolicy(t *testing.T) {
	openSharing := &os.PathError{Op: "open", Path: "stage", Err: windows.ERROR_SHARING_VIOLATION}
	accessDenied := windows.ERROR_ACCESS_DENIED
	exist := errors.Join(ErrNamespaceCollision, fs.ErrExist, windows.ERROR_ALREADY_EXISTS)
	unsafeDenied := errors.Join(ErrUnsafeNamespace, windows.ERROR_ACCESS_DENIED)
	unsupported := noReplaceError(windows.ERROR_NOT_SUPPORTED)
	allWaits := windowsRenameBackoff[:]

	for _, tc := range []struct {
		name      string
		results   []error
		wantErr   error
		wantWaits []time.Duration
	}{
		{name: "immediate success", results: []error{nil}},
		{name: "recovers after open sharing violation", results: []error{openSharing, nil}, wantWaits: allWaits[:1]},
		{name: "recovers after access denied", results: []error{accessDenied, nil}, wantWaits: allWaits[:1]},
		{
			name:      "recovers on final attempt",
			results:   []error{openSharing, accessDenied, openSharing, accessDenied, openSharing, accessDenied, nil},
			wantWaits: allWaits,
		},
		{
			name:      "exhausts retries",
			results:   []error{accessDenied, accessDenied, accessDenied, accessDenied, accessDenied, accessDenied, openSharing},
			wantErr:   openSharing,
			wantWaits: allWaits,
		},
		{name: "collision does not retry", results: []error{exist}, wantErr: exist},
		{name: "unsupported rename does not retry", results: []error{unsupported}, wantErr: unsupported},
		{name: "unsafe namespace does not retry", results: []error{unsafeDenied}, wantErr: unsafeDenied},
	} {
		t.Run(tc.name, func(t *testing.T) {
			attempts := 0
			var waited []time.Duration
			err := retryWindowsRename(func() error {
				result := tc.results[attempts]
				attempts++
				return result
			}, func(delay time.Duration) {
				waited = append(waited, delay)
			})
			if !errors.Is(err, tc.wantErr) || (tc.wantErr == nil && err != nil) {
				t.Fatalf("retryWindowsRename = %v, want %v", err, tc.wantErr)
			}
			if attempts != len(tc.results) || !slices.Equal(waited, tc.wantWaits) {
				t.Fatalf("attempts=%d waits=%v, want %d and %v", attempts, waited, len(tc.results), tc.wantWaits)
			}
		})
	}
}

func createWindowsJunction(t *testing.T, link, target string) {
	t.Helper()
	out, err := exec.Command("cmd", "/d", "/c", "mklink", "/J", link, target).CombinedOutput()
	if err != nil {
		t.Fatalf("mklink /J %q %q: %v (%s)", link, target, err, strings.TrimSpace(string(out)))
	}
	t.Cleanup(func() { _ = os.Remove(link) })
}

func TestWindowsChildOpsStayAnchoredAfterParentMovedAndReplaced(t *testing.T) {
	base := t.TempDir()
	origPath := filepath.Join(base, "target")
	movedPath := filepath.Join(base, "moved")
	if err := os.Mkdir(origPath, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(origPath, "existing.txt"), []byte("orig"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(origPath, "keep.txt"), []byte("stay"), 0o600); err != nil {
		t.Fatal(err)
	}

	dir, _, err := OpenContainer(origPath, false)
	if err != nil {
		t.Fatal(err)
	}
	defer dir.Close()

	// Move the open directory and replace its original path with another directory.
	if err := os.Rename(origPath, movedPath); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(origPath, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(origPath, "decoy.txt"), []byte("decoy"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(origPath, "existing.txt"), []byte("replacement"), 0o600); err != nil {
		t.Fatal(err)
	}
	replacementBefore, err := os.Stat(origPath)
	if err != nil {
		t.Fatal(err)
	}

	if kind, _, err := lstatChild(dir, "decoy.txt"); err != nil || kind != EntryAbsent {
		t.Fatalf("lstatChild saw replacement decoy: kind=%v err=%v", kind, err)
	}
	if kind, size, err := lstatChild(dir, "existing.txt"); err != nil || kind != EntryRegularFile || size != 4 {
		t.Fatalf("lstatChild(existing.txt) = (%v, %d, %v), want (regular, 4, nil)", kind, size, err)
	}
	for pass := range 2 {
		names, err := readDirNames(dir)
		if err != nil {
			t.Fatalf("readDirNames pass %d: %v", pass, err)
		}
		slices.Sort(names)
		if !slices.Equal(names, []string{"existing.txt", "keep.txt"}) {
			t.Fatalf("readDirNames pass %d = %v, want [existing.txt keep.txt]", pass, names)
		}
	}
	if exact, err := entryMatchesSpelling(dir, "existing.txt"); err != nil || !exact {
		t.Fatalf("entryMatchesSpelling exact = (%t, %v)", exact, err)
	}
	if exact, err := entryMatchesSpelling(dir, "EXISTING.TXT"); err != nil || exact {
		t.Fatalf("entryMatchesSpelling case alias = (%t, %v)", exact, err)
	}

	if err := mkdirChild(dir, "sub", publicDirMode); err != nil {
		t.Fatalf("mkdirChild(sub): %v", err)
	}
	sub, err := openChildDir(dir, "sub")
	if err != nil {
		t.Fatalf("openChildDir(sub): %v", err)
	}
	defer sub.Close()

	nested, err := openChildFile(sub, "nested.txt", os.O_RDWR|os.O_CREATE|os.O_EXCL, publicFileMode)
	if err != nil {
		t.Fatalf("openChildFile(nested.txt): %v", err)
	}
	if _, err := nested.Write([]byte("nested-data")); err != nil {
		_ = nested.Close()
		t.Fatal(err)
	}
	if err := nested.Close(); err != nil {
		t.Fatal(err)
	}

	if err := renameFileNoReplace(dir, "existing.txt", sub, "published.txt", false); err != nil {
		t.Fatalf("renameFileNoReplace: %v", err)
	}
	if err := removeChild(dir, "keep.txt", false); err != nil {
		t.Fatalf("removeChild(keep.txt): %v", err)
	}

	targetTime := time.Unix(1_700_000_000, 123_456_700)
	if err := setDirModifiedTime(dir, targetTime); err != nil {
		t.Fatalf("setDirModifiedTime: %v", err)
	}

	// Verify all mutations landed in the original moved directory.
	if got, err := os.ReadFile(filepath.Join(movedPath, "sub", "nested.txt")); err != nil || string(got) != "nested-data" {
		t.Fatalf("moved nested.txt = (%q, %v)", got, err)
	}
	if got, err := os.ReadFile(filepath.Join(movedPath, "sub", "published.txt")); err != nil || string(got) != "orig" {
		t.Fatalf("moved published.txt = (%q, %v)", got, err)
	}
	if _, err := os.Stat(filepath.Join(movedPath, "keep.txt")); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("moved keep.txt survived removeChild: %v", err)
	}
	movedStat, err := os.Stat(movedPath)
	if err != nil || !movedStat.ModTime().Equal(targetTime) {
		t.Fatalf("moved mtime = (%v, %v), want %v", movedStat.ModTime(), err, targetTime)
	}

	// Verify the replacement directory at origPath stayed completely untouched.
	if _, err := os.Stat(filepath.Join(origPath, "sub")); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("replacement directory received sub: %v", err)
	}
	if got, err := os.ReadFile(filepath.Join(origPath, "existing.txt")); err != nil || string(got) != "replacement" {
		t.Fatalf("replacement existing.txt changed = (%q, %v)", got, err)
	}
	replacementAfter, err := os.Stat(origPath)
	if err != nil || !replacementAfter.ModTime().Equal(replacementBefore.ModTime()) {
		t.Fatalf("replacement mtime changed: before=%v after=%v err=%v", replacementBefore.ModTime(), replacementAfter.ModTime(), err)
	}
}

func TestWindowsSessionStaysAnchoredAcrossMovedResultDirAndJunctionReplacement(t *testing.T) {
	ctx := context.Background()
	base := t.TempDir()
	destRoot := filepath.Join(base, "dest")
	movedResult := filepath.Join(base, "moved-result")
	victimDir := filepath.Join(base, "victim")
	for _, p := range []string{destRoot, victimDir} {
		if err := os.Mkdir(p, 0o755); err != nil {
			t.Fatal(err)
		}
	}

	auth, err := New(Config{RootPath: destRoot})
	if err != nil {
		t.Fatal(err)
	}
	defer auth.Close()
	if err := auth.BindDestination(ctx); err != nil {
		t.Fatal(err)
	}
	sel := testSelectionSpec(t, 0x81)
	lookup, err := auth.SelectOperation(ctx, sel, receivecontract.AutomaticOperation())
	if err != nil || lookup.Kind() != LookupCreateReady {
		t.Fatalf("select = (%v, %v)", lookup.Kind(), err)
	}
	art, err := receivecontract.NewResultRootDirectoryTree(receivecontract.NewSyntheticSelectionResultRoot())
	if err != nil {
		t.Fatal(err)
	}
	op, err := auth.CreateOperation(ctx, lookup, art)
	if err != nil {
		t.Fatal(err)
	}
	intent, _ := op.ReceiveIntent()
	res, _ := intent.MaterializationPlan().DestinationReservation()
	resPath := filepath.Join(destRoot, res.PhysicalName())

	session, err := auth.OpenOperation(ctx, op)
	if err != nil {
		t.Fatal(err)
	}
	rootAdm := testAdmitRoot(t, session, intent, 0x82)

	// Move the active result root away and replace its path with a junction to victimDir.
	if err := os.Rename(resPath, movedResult); err != nil {
		t.Fatalf("rename active result root: %v", err)
	}
	createWindowsJunction(t, resPath, victimDir)

	nestedMod, err := catalog.NewModifiedTime(1_700_000_100, 200_000_000, catalog.TimePrecisionNanoseconds)
	if err != nil {
		t.Fatal(err)
	}
	rootSrc := ordinaryoutput.EmptySourceCatalogPath()
	rootReq, err := transfer.NewDirectoryMaterializationRequest(
		intent,
		transfer.AuthenticatedSourceDirectory{
			DirectoryID: rootAdm.DirectoryID(), Generation: rootAdm.Generation(),
			SourcePath: rootSrc, ModifiedTime: rootAdm.ModifiedTime(),
		},
		ordinaryoutput.SourceNodeSelected,
		transfer.MaterializedDirectoryClaim{},
	)
	if err != nil {
		t.Fatal(err)
	}
	rootClaim, err := transfer.NewMaterializedDirectoryClaim(rootAdm, rootReq)
	if err != nil {
		t.Fatal(err)
	}
	nestedSrc, err := ordinaryoutput.NewSourceCatalogPath("nested")
	if err != nil {
		t.Fatal(err)
	}
	nestedReq, err := transfer.NewDirectoryMaterializationRequest(
		intent,
		transfer.AuthenticatedSourceDirectory{
			DirectoryID: testID16[catalog.DirectoryID](0x83), Generation: testID16[catalog.DirectoryGeneration](0x84),
			SourcePath: nestedSrc, ParentAdmission: rootAdm, ModifiedTime: nestedMod,
		},
		ordinaryoutput.SourceNodeSelected,
		rootClaim,
	)
	if err != nil {
		t.Fatal(err)
	}
	nestedAdm, err := session.AdmitDirectory(ctx, nestedReq)
	if err != nil {
		t.Fatalf("AdmitDirectory(nested): %v", err)
	}

	payload := []byte("anchored-write")
	geom, err := content.NewFileGeometry(uint64(len(payload)), catalog.DefaultChunkSize)
	if err != nil {
		t.Fatal(err)
	}
	desc, err := content.NewFileRevisionDescriptor(
		intent.ShareInstance(), testID16[catalog.FileID](0x85), testID16[content.FileRevision](0x86),
		geom, catalog.ModifiedTime{},
	)
	if err != nil {
		t.Fatal(err)
	}
	nestedClaim, err := transfer.NewMaterializedDirectoryClaim(nestedAdm, nestedReq)
	if err != nil {
		t.Fatal(err)
	}
	fileParent, err := transfer.NewDirectoryMaterializationFileParent(
		nestedReq.Source().DirectoryID, nestedReq.Source().Generation, nestedReq.Source().SourcePath,
		nestedAdm, nestedClaim,
	)
	if err != nil {
		t.Fatal(err)
	}
	fileSrc, err := ordinaryoutput.NewSourceCatalogPath("nested/payload.bin")
	if err != nil {
		t.Fatal(err)
	}
	fileRel, err := transfer.NewMaterializationRootRelativePath(nestedAdm.Path() + "/payload.bin")
	if err != nil {
		t.Fatal(err)
	}
	matFile, err := transfer.NewMaterializationFile(intent, fileSrc, fileRel, desc, session.SessionID(), fileParent)
	if err != nil {
		t.Fatal(err)
	}
	start, err := session.BeginFile(ctx, matFile)
	if err != nil {
		t.Fatalf("BeginFile: %v", err)
	}
	tx, _, ok := start.Transaction()
	if !ok {
		t.Fatalf("expected active transaction, got %+v", start)
	}
	if err := tx.WriteRange(ctx, 0, payload); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Accept(ctx); err != nil {
		t.Fatal(err)
	}
	if settled, err := tx.Commit(ctx); err != nil || settled.Kind() != transfer.FilePublished {
		t.Fatalf("Commit = (%v, %v)", settled.Kind(), err)
	}
	if settled, err := session.FinalizeDirectory(ctx, nestedAdm); err != nil || settled.Kind() != transfer.DirectoryFinalized {
		t.Fatalf("FinalizeDirectory(nested) = (%v, %v)", settled.Kind(), err)
	}
	if _, err := session.FinalizeDirectory(ctx, rootAdm); err != nil {
		t.Fatal(err)
	}
	if _, err := session.FinalizeTree(ctx, transfer.DirectTreeOutcomeSuccess); err != nil {
		t.Fatal(err)
	}

	if entries, err := os.ReadDir(victimDir); err != nil || len(entries) != 0 {
		t.Fatalf("junction victim directory received writes: entries=%v err=%v", entries, err)
	}
	got, err := os.ReadFile(filepath.Join(movedResult, "nested", "payload.bin"))
	if err != nil || string(got) != string(payload) {
		t.Fatalf("moved nested/payload.bin = (%q, %v)", got, err)
	}
	nestedStat, err := os.Stat(filepath.Join(movedResult, "nested"))
	wantTime := time.Unix(nestedMod.Seconds(), int64(nestedMod.Nanoseconds()))
	if err != nil || !nestedStat.ModTime().Equal(wantTime) {
		t.Fatalf("moved nested mtime = (%v, %v), want %v", nestedStat.ModTime(), err, wantTime)
	}
}

func TestWindowsHandleOpsExceedMaxPathAndEnforceSafety(t *testing.T) {
	base := t.TempDir()
	root, _, err := OpenContainer(base, false)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()

	// 1. Deep path exceeding Win32 MAX_PATH (260 chars) succeeds via handle-relative NT ops.
	seg := strings.Repeat("d", 40)
	cur := root
	var opened []*Dir
	for len(cur.Path()) < 300 {
		if err := mkdirChild(cur, seg, publicDirMode); err != nil {
			t.Fatalf("mkdirChild at depth len=%d: %v", len(cur.Path()), err)
		}
		next, err := openChildDir(cur, seg)
		if err != nil {
			t.Fatalf("openChildDir at depth len=%d: %v", len(cur.Path()), err)
		}
		opened = append(opened, next)
		cur = next
	}
	leafFile, err := openChildFile(cur, "stage.part", os.O_RDWR|os.O_CREATE|os.O_EXCL, publicFileMode)
	if err != nil {
		t.Fatalf("openChildFile past MAX_PATH: %v", err)
	}
	if _, err := leafFile.Write([]byte("deep")); err != nil {
		_ = leafFile.Close()
		t.Fatal(err)
	}
	if err := leafFile.Close(); err != nil {
		t.Fatal(err)
	}
	if err := renameFileNoReplace(cur, "stage.part", cur, "final.txt", false); err != nil {
		t.Fatalf("renameFileNoReplace past MAX_PATH: %v", err)
	}
	if kind, size, err := lstatChild(cur, "final.txt"); err != nil || kind != EntryRegularFile || size != 4 {
		t.Fatalf("lstatChild past MAX_PATH = (%v, %d, %v)", kind, size, err)
	}
	for i := len(opened) - 1; i >= 0; i-- {
		_ = opened[i].Close()
	}
	if err := removeChildTree(root, seg); err != nil {
		t.Fatalf("removeChildTree past MAX_PATH: %v", err)
	}

	// 2. Colon names are rejected before NtCreateFile can create an NTFS alternate data stream.
	if err := validateNativeName("12:30.log"); !errors.Is(err, ErrNameRejected) {
		t.Fatalf("validateNativeName(12:30.log) = %v, want ErrNameRejected", err)
	}

	// 3. O_CREATE|O_TRUNC never truncates or follows a reparse point.
	target := filepath.Join(base, "jtarget")
	if err := os.Mkdir(target, 0o755); err != nil {
		t.Fatal(err)
	}
	createWindowsJunction(t, filepath.Join(base, "jlink"), target)
	if f, err := openChildFile(root, "jlink", os.O_RDWR|os.O_CREATE|os.O_TRUNC, publicFileMode); !errors.Is(err, ErrUnsafeNamespace) {
		if f != nil {
			_ = f.Close()
		}
		t.Fatalf("openChildFile(O_TRUNC) on junction = %v, want ErrUnsafeNamespace", err)
	}

	// 4. Legacy FileDispositionInfo fallback clears read-only attribute and deletes file.
	ro, err := openChildFile(root, "readonly.txt", os.O_RDWR|os.O_CREATE|os.O_EXCL, publicFileMode)
	if err != nil {
		t.Fatal(err)
	}
	if err := ro.Close(); err != nil {
		t.Fatal(err)
	}
	roPath := filepath.Join(base, "readonly.txt")
	if err := os.Chmod(roPath, 0o444); err != nil {
		t.Fatal(err)
	}
	h, err := ntCreateAt(root, "readonly.txt", windows.DELETE|windows.FILE_READ_ATTRIBUTES|windows.SYNCHRONIZE,
		windows.FILE_OPEN, windows.FILE_NON_DIRECTORY_FILE)
	if err != nil {
		t.Fatal(err)
	}
	fbErr := deleteOpenHandleFallback(h)
	closeErr := windows.CloseHandle(h)
	if fbErr != nil || closeErr != nil {
		t.Fatalf("deleteOpenHandleFallback = (%v, close=%v)", fbErr, closeErr)
	}
	if _, err := os.Stat(roPath); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("readonly.txt survived fallback delete: %v", err)
	}

	// 5. Nil Dir fails closed without panicking.
	if _, err := openChildDir(nil, "a"); !errors.Is(err, fs.ErrClosed) {
		t.Fatalf("openChildDir(nil) = %v, want fs.ErrClosed", err)
	}
	if _, err := readDirNames(nil); !errors.Is(err, fs.ErrClosed) {
		t.Fatalf("readDirNames(nil) = %v, want fs.ErrClosed", err)
	}
}

func TestWindowsDiscardUnlinksScratchJunctionWithoutDeletingTarget(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	_, intent, _ := createPausedSingleFileOp(t, root, 0xc7)
	opID := intent.OperationID()

	victimDir := t.TempDir()
	victimFile := filepath.Join(victimDir, "keep.txt")
	if err := os.WriteFile(victimFile, []byte("precious"), 0o600); err != nil {
		t.Fatal(err)
	}

	opDir := filepath.Join(root, ControlDirName, hex.EncodeToString(opID.Bytes()))
	createWindowsJunction(t, filepath.Join(opDir, "linked-dir"), victimDir)

	summary, err := DiscardOperation(ctx, root, opID)
	if err != nil || summary.State() != RecoveryOperationDiscarded {
		t.Fatalf("DiscardOperation with scratch junction = (%+v, %v)", summary, err)
	}
	if _, err := os.Stat(opDir); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("operation scratch dir survived discard: %v", err)
	}
	if got, err := os.ReadFile(victimFile); err != nil || string(got) != "precious" {
		t.Fatalf("junction target was modified by scratch cleanup: (%q, %v)", got, err)
	}
}

func TestWindowsNestedReservedNamesReportRelativeAdjustmentsWithoutSidecar(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	auth, err := New(Config{RootPath: root, CreateRoot: true})
	if err != nil {
		t.Fatal(err)
	}
	defer auth.Close()
	if err := auth.BindDestination(ctx); err != nil {
		t.Fatal(err)
	}
	sel := testSelectionSpec(t, 0xd1)
	lookup, err := auth.SelectOperation(ctx, sel, receivecontract.AutomaticOperation())
	if err != nil || lookup.Kind() != LookupCreateReady {
		t.Fatalf("select = (%v, %v)", lookup.Kind(), err)
	}
	art, err := receivecontract.NewResultRootDirectoryTree(receivecontract.NewSyntheticSelectionResultRoot())
	if err != nil {
		t.Fatal(err)
	}
	op, err := auth.CreateOperation(ctx, lookup, art)
	if err != nil {
		t.Fatal(err)
	}
	intent, _ := op.ReceiveIntent()
	res, _ := intent.MaterializationPlan().DestinationReservation()
	session, err := auth.OpenOperation(ctx, op)
	if err != nil {
		t.Fatal(err)
	}
	rootAdm := testAdmitRoot(t, session, intent, 0xd3)

	// Admit a Windows-reserved nested directory ("PRN") and write a colon-named
	// file inside it alongside a reserved file ("CON") and an ordinary file.
	rootSrc := ordinaryoutput.EmptySourceCatalogPath()
	rootReq, err := transfer.NewDirectoryMaterializationRequest(
		intent,
		transfer.AuthenticatedSourceDirectory{
			DirectoryID: rootAdm.DirectoryID(), Generation: rootAdm.Generation(),
			SourcePath: rootSrc, ModifiedTime: rootAdm.ModifiedTime(),
		},
		ordinaryoutput.SourceNodeSelected,
		transfer.MaterializedDirectoryClaim{},
	)
	if err != nil {
		t.Fatal(err)
	}
	rootClaim, err := transfer.NewMaterializedDirectoryClaim(rootAdm, rootReq)
	if err != nil {
		t.Fatal(err)
	}
	prnSrc, err := ordinaryoutput.NewSourceCatalogPath("PRN")
	if err != nil {
		t.Fatal(err)
	}
	prnReq, err := transfer.NewDirectoryMaterializationRequest(
		intent,
		transfer.AuthenticatedSourceDirectory{
			DirectoryID: testID16[catalog.DirectoryID](0xd4), Generation: testID16[catalog.DirectoryGeneration](0xd5),
			SourcePath: prnSrc, ParentAdmission: rootAdm,
		},
		ordinaryoutput.SourceNodeSelected,
		rootClaim,
	)
	if err != nil {
		t.Fatal(err)
	}
	prnAdm, err := session.AdmitDirectory(ctx, prnReq)
	if err != nil {
		t.Fatalf("AdmitDirectory(PRN): %v", err)
	}
	prnClaim, err := transfer.NewMaterializedDirectoryClaim(prnAdm, prnReq)
	if err != nil {
		t.Fatal(err)
	}

	writeCommitted := func(file transfer.MaterializationFile, body string) {
		t.Helper()
		start, err := session.BeginFile(ctx, file)
		if err != nil {
			t.Fatal(err)
		}
		tx, _, ok := start.Transaction()
		if !ok {
			t.Fatalf("expected transaction for %s", file.MaterializationRelativePath())
		}
		if err := tx.WriteRange(ctx, 0, []byte(body)); err != nil {
			t.Fatal(err)
		}
		if settled, err := tx.Commit(ctx); err != nil || settled.Kind() != transfer.FilePublished {
			t.Fatalf("commit %s = (%v, %v)", file.MaterializationRelativePath(), settled.Kind(), err)
		}
	}

	writeCommitted(testMaterializationFile(t, session, intent, 0xd6, "CON", 3, rootAdm), "con")
	writeCommitted(testMaterializationFile(t, session, intent, 0xd8, "ok.txt", 2, rootAdm), "ok")

	logGeom, err := content.NewFileGeometry(3, catalog.DefaultChunkSize)
	if err != nil {
		t.Fatal(err)
	}
	logDesc, err := content.NewFileRevisionDescriptor(
		intent.ShareInstance(), testID16[catalog.FileID](0xda), testID16[content.FileRevision](0xdb), logGeom, catalog.ModifiedTime{},
	)
	if err != nil {
		t.Fatal(err)
	}
	logParent, err := transfer.NewDirectoryMaterializationFileParent(
		prnReq.Source().DirectoryID, prnReq.Source().Generation, prnReq.Source().SourcePath, prnAdm, prnClaim,
	)
	if err != nil {
		t.Fatal(err)
	}
	logSrc, err := ordinaryoutput.NewSourceCatalogPath("PRN/12:30.log")
	if err != nil {
		t.Fatal(err)
	}
	logRel, err := transfer.NewMaterializationRootRelativePath(prnAdm.Path() + "/12:30.log")
	if err != nil {
		t.Fatal(err)
	}
	logFile, err := transfer.NewMaterializationFile(intent, logSrc, logRel, logDesc, session.SessionID(), logParent)
	if err != nil {
		t.Fatal(err)
	}
	writeCommitted(logFile, "log")

	if _, err := session.FinalizeDirectory(ctx, prnAdm); err != nil {
		t.Fatal(err)
	}
	if _, err := session.FinalizeDirectory(ctx, rootAdm); err != nil {
		t.Fatal(err)
	}
	if _, err := session.FinalizeTree(ctx, transfer.DirectTreeOutcomeSuccess); err != nil {
		t.Fatal(err)
	}

	summary := op.NameAdjustments()
	if summary.Count != 3 || len(summary.Samples) != 3 {
		t.Fatalf("windows adjustments = %+v, want 3 entries", summary)
	}
	wantLogical := []string{"CON", "PRN", "PRN/12:30.log"}
	destDir := filepath.Join(root, res.PhysicalName())
	for i, want := range wantLogical {
		got := summary.Samples[i]
		if got.LogicalPath != want || got.PhysicalPath == want {
			t.Fatalf("sample[%d] = %+v, want logical %q", i, got, want)
		}
		if _, err := os.Stat(filepath.Join(destDir, filepath.FromSlash(got.PhysicalPath))); err != nil {
			t.Fatalf("adjusted path %q missing on disk: %v", got.PhysicalPath, err)
		}
	}
	topEntries, err := os.ReadDir(root)
	if err != nil || len(topEntries) != 1 || topEntries[0].Name() != res.PhysicalName() {
		t.Fatalf("unexpected sidecar beside destination: %v (%v)", topEntries, err)
	}
}
