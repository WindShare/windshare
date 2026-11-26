//go:build windows

package output

import (
	"context"
	"encoding/hex"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/windshare/windshare/core/catalog"
	"github.com/windshare/windshare/core/transfer/receivecontract"
	"golang.org/x/sys/windows"
)

func TestWindowsCreateFilePathAndNormalizeFinalPath(t *testing.T) {
	longSeg := strings.Repeat("a", 70)
	longDrive := filepath.Join(`C:\base`, longSeg, longSeg, longSeg, longSeg)
	if len(longDrive) < windowsMaxShortDirPath {
		t.Fatalf("expected longDrive >= %d chars, got %d", windowsMaxShortDirPath, len(longDrive))
	}
	longUNC := `\\server\share\` + strings.Join([]string{longSeg, longSeg, longSeg, longSeg}, `\`)
	longUNCSlash := `//server/share/` + strings.Join([]string{longSeg, longSeg, longSeg, longSeg}, `/`)
	longDevice := `\\.\pipe\` + strings.Join([]string{longSeg, longSeg, longSeg, longSeg}, `\`)
	longVolume := `\\?\Volume{11111111-1111-1111-1111-111111111111}\` + strings.Join([]string{longSeg, longSeg, longSeg, longSeg}, `\`)

	for _, tc := range []struct {
		name string
		in   string
		want string
	}{
		{name: "empty", in: "", want: ""},
		{name: "short drive unchanged", in: `C:\tmp\dest`, want: `C:\tmp\dest`},
		{name: "short trailing dot unchanged", in: `C:\tmp\dest.`, want: `C:\tmp\dest.`},
		{name: "short UNC unchanged", in: `\\server\share\dest`, want: `\\server\share\dest`},
		{name: "already extended drive unchanged", in: windowsExtendedPrefix + longDrive, want: windowsExtendedPrefix + longDrive},
		{name: "already extended forward slash unchanged", in: `//?/C:/already/extended`, want: `//?/C:/already/extended`},
		{name: "already NT namespace unchanged", in: windowsNTNamespacePrefix + longDrive, want: windowsNTNamespacePrefix + longDrive},
		{name: "already volume GUID unchanged", in: longVolume, want: longVolume},
		{name: "short device path unchanged", in: `\\.\NUL`, want: `\\.\NUL`},
		{name: "long device path unchanged", in: longDevice, want: longDevice},
		{name: "long drive gains extended prefix", in: longDrive, want: windowsExtendedPrefix + longDrive},
		{name: "long drive trailing dot canonicalized", in: longDrive + ".", want: windowsExtendedPrefix + longDrive},
		{name: "long UNC gains extended UNC prefix", in: longUNC, want: windowsExtendedUNCPrefix + strings.TrimPrefix(longUNC, `\\`)},
		{name: "long forward slash UNC normalized", in: longUNCSlash, want: windowsExtendedUNCPrefix + strings.TrimPrefix(longUNC, `\\`)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := windowsCreateFilePath(tc.in)
			if err != nil {
				t.Fatalf("windowsCreateFilePath(%q) unexpected err: %v", tc.in, err)
			}
			if got != tc.want {
				t.Fatalf("windowsCreateFilePath(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}

	for _, tc := range []struct {
		name string
		in   string
		want string
	}{
		{name: "short drive", in: `\\?\C:\dir`, want: `C:\dir`},
		{name: "long drive", in: windowsExtendedPrefix + longDrive, want: longDrive},
		{name: "short UNC", in: `\\?\UNC\server\share\dir`, want: `\\server\share\dir`},
		{name: "long UNC", in: windowsExtendedUNCPrefix + strings.TrimPrefix(longUNC, `\\`), want: longUNC},
		{name: "volume GUID preserved", in: longVolume, want: longVolume},
	} {
		t.Run("normalize/"+tc.name, func(t *testing.T) {
			if got := normalizeWindowsFinalPath(tc.in); got != tc.want {
				t.Fatalf("normalizeWindowsFinalPath(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestWindowsOpenContainerAndRecoverySupportLongPathsBeyondMaxPath(t *testing.T) {
	ctx := context.Background()
	base := t.TempDir()
	seg := strings.Repeat("d", 60)
	longParent := filepath.Join(base, seg, seg, seg, seg)
	longDest := filepath.Join(longParent, "destination-root")
	if len(longDest) <= 260 {
		t.Fatalf("expected destination path > 260 chars, got len=%d (%q)", len(longDest), longDest)
	}

	// 1. OpenContainer(create=true) creates and opens a > MAX_PATH destination
	// without leaking a stranded directory or polluting Dir.Path with \\?\.
	created, disp, err := OpenContainer(longDest, true)
	if err != nil {
		t.Fatalf("OpenContainer(longDest, true) failed: %v", err)
	}
	if disp != AuthorityCreatedRoot || created.Path() != longDest {
		_ = created.Close()
		t.Fatalf("OpenContainer(longDest, true) = (disp=%v, path=%q), want (AuthorityCreatedRoot, %q)", disp, created.Path(), longDest)
	}
	if err := created.Close(); err != nil {
		t.Fatal(err)
	}

	// 2. OpenContainer(create=false) reopens an existing > MAX_PATH destination.
	reopened, disp, err := OpenContainer(longDest, false)
	if err != nil {
		t.Fatalf("OpenContainer(longDest, false) failed: %v", err)
	}
	if disp != CallerProvidedContainer || reopened.Path() != longDest {
		_ = reopened.Close()
		t.Fatalf("OpenContainer(longDest, false) = (disp=%v, path=%q), want (CallerProvidedContainer, %q)", disp, reopened.Path(), longDest)
	}
	if err := reopened.Close(); err != nil {
		t.Fatal(err)
	}

	// 3. Full BindDestination, CreateOperation, ListOperations, and DiscardOperation
	// work end-to-end on the > MAX_PATH destination.
	auth, err := New(Config{RootPath: longDest, CreateRoot: false})
	if err != nil {
		t.Fatal(err)
	}
	if err := auth.BindDestination(ctx); err != nil {
		_ = auth.Close()
		t.Fatalf("BindDestination(longDest) failed: %v", err)
	}
	sel := testSelectionSpec(t, 0x91)
	lookup, err := auth.SelectOperation(ctx, sel, receivecontract.AutomaticOperation())
	if err != nil || lookup.Kind() != LookupCreateReady {
		_ = auth.Close()
		t.Fatalf("SelectOperation(longDest) = (kind=%v, err=%v), want LookupCreateReady", lookup.Kind(), err)
	}
	art, err := receivecontract.NewSingleFileDirectoryTree(testID16[catalog.FileID](0x92), "long.bin", "long.bin")
	if err != nil {
		_ = auth.Close()
		t.Fatal(err)
	}
	op, err := auth.CreateOperation(ctx, lookup, art)
	if err != nil {
		_ = auth.Close()
		t.Fatalf("CreateOperation(longDest) failed: %v", err)
	}
	intent, _ := op.ReceiveIntent()
	if err := auth.Close(); err != nil {
		t.Fatal(err)
	}

	inv, err := ListOperations(ctx, longDest)
	if err != nil || inv.Status() != RecoveryListReady || len(inv.Summaries()) != 1 ||
		inv.Summaries()[0].OperationID() != intent.OperationID() {
		t.Fatalf("ListOperations(longDest) = (%+v, %v)", inv, err)
	}
	summary, err := DiscardOperation(ctx, longDest, intent.OperationID())
	if err != nil || summary.State() != RecoveryOperationDiscarded {
		t.Fatalf("DiscardOperation(longDest) = (%+v, %v)", summary, err)
	}
	if _, err := os.Lstat(filepath.Join(longDest, ControlDirName)); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("control dir survived discard on longDest: %v", err)
	}

	// 4. Junctions targeting or living beyond MAX_PATH resolve and open cleanly
	// without leaking \\?\ into Dir.Path.
	targetInfo, err := os.Stat(longDest)
	if err != nil {
		t.Fatal(err)
	}
	shortLink := filepath.Join(base, "short-to-long-junction")
	createWindowsJunction(t, shortLink, windowsExtendedPrefix+longDest)
	viaShort, _, err := OpenContainer(shortLink, false)
	if err != nil {
		t.Fatalf("OpenContainer(shortLink -> longDest) failed: %v", err)
	}
	if strings.HasPrefix(viaShort.Path(), windowsExtendedPrefix) {
		_ = viaShort.Close()
		t.Fatalf("viaShort.Path() leaked extended prefix: %q", viaShort.Path())
	}
	viaShortInfo, err := os.Stat(viaShort.Path())
	_ = viaShort.Close()
	if err != nil || !os.SameFile(targetInfo, viaShortInfo) {
		t.Fatalf("viaShort target mismatch: err=%v path=%q", err, viaShort.Path())
	}

	longLink := filepath.Join(longParent, "long-junction-link")
	if len(longLink) <= 260 {
		t.Fatalf("expected longLink > 260 chars, got len=%d", len(longLink))
	}
	createWindowsJunction(t, windowsExtendedPrefix+longLink, windowsExtendedPrefix+longDest)
	tag, err := windowsReparseTag(longLink)
	if err != nil || tag != windows.IO_REPARSE_TAG_MOUNT_POINT {
		t.Fatalf("windowsReparseTag(longLink) = (0x%x, %v), want IO_REPARSE_TAG_MOUNT_POINT", tag, err)
	}
	resolved, err := windowsResolvedContainerPath(longLink)
	if err != nil || strings.HasPrefix(resolved, windowsExtendedPrefix) {
		t.Fatalf("windowsResolvedContainerPath(longLink) = (%q, %v)", resolved, err)
	}
	viaLong, _, err := OpenContainer(longLink, false)
	if err != nil {
		t.Fatalf("OpenContainer(longLink -> longDest) failed: %v", err)
	}
	viaLongInfo, err := os.Stat(viaLong.Path())
	_ = viaLong.Close()
	if err != nil || !os.SameFile(targetInfo, viaLongInfo) || strings.HasPrefix(viaLong.Path(), windowsExtendedPrefix) {
		t.Fatalf("viaLong target mismatch: err=%v path=%q", err, viaLong.Path())
	}
}

func TestWindowsOpenContainerPreservesTrailingDotNormalization(t *testing.T) {
	base := t.TempDir()

	// Short path (< 248): os.MkdirAll and openDirHandle must both apply standard
	// Win32 normalization instead of mismatching via unconditional \\?\.
	shortWithDot := filepath.Join(base, "short-dot") + "."
	if len(shortWithDot) >= windowsMaxShortDirPath {
		t.Fatalf("expected shortWithDot < %d chars, got %d", windowsMaxShortDirPath, len(shortWithDot))
	}
	shortDir, disp, err := OpenContainer(shortWithDot, true)
	if err != nil {
		t.Fatalf("OpenContainer(shortWithDot, true) failed: %v", err)
	}
	if disp != AuthorityCreatedRoot {
		_ = shortDir.Close()
		t.Fatalf("shortWithDot disposition = %v, want AuthorityCreatedRoot", disp)
	}
	if err := shortDir.Close(); err != nil {
		t.Fatal(err)
	}

	// Long path (>= 248): GetFullPathName normalizes trailing dots before \\?\
	// identically to Go's os.MkdirAll / os.Lstat.
	seg := strings.Repeat("e", 60)
	longWithDot := filepath.Join(base, seg, seg, seg, seg, "long-dot") + "."
	if len(longWithDot) <= 260 {
		t.Fatalf("expected longWithDot > 260 chars, got %d", len(longWithDot))
	}
	longDir, disp, err := OpenContainer(longWithDot, true)
	if err != nil {
		t.Fatalf("OpenContainer(longWithDot, true) failed: %v", err)
	}
	if disp != AuthorityCreatedRoot {
		_ = longDir.Close()
		t.Fatalf("longWithDot disposition = %v, want AuthorityCreatedRoot", disp)
	}
	if err := longDir.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestWindowsListOperationsReportsSharingViolationTaskLockAsAttentionLease(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	_, intent, _ := createPausedSingleFileOp(t, root, 0x99)
	lockPath := filepath.Join(root, ControlDirName, hex.EncodeToString(intent.OperationID().Bytes()), taskLockFileName)

	// Open task.lock without FILE_SHARE_WRITE so acquireLockFile's O_RDWR open
	// receives ERROR_SHARING_VIOLATION rather than LockFileEx's ERROR_LOCK_VIOLATION.
	encoded, err := windows.UTF16PtrFromString(lockPath)
	if err != nil {
		t.Fatal(err)
	}
	held, err := windows.CreateFile(encoded, windows.FILE_GENERIC_READ, windows.FILE_SHARE_READ, nil,
		windows.OPEN_EXISTING, windows.FILE_ATTRIBUTE_NORMAL, 0)
	if err != nil {
		t.Fatal(err)
	}
	closed := false
	t.Cleanup(func() {
		if !closed {
			_ = windows.CloseHandle(held)
		}
	})

	inv, err := ListOperations(ctx, root)
	if err != nil || inv.Status() != RecoveryListNeedsAttention || inv.UnknownEntries() || len(inv.Summaries()) != 1 {
		t.Fatalf("sharing-violation inventory = (%+v, %v), want needs-attention without unknownEntries", inv, err)
	}
	got := inv.Summaries()[0]
	if got.OperationID() != intent.OperationID() || got.Busy() ||
		got.State() != RecoveryOperationNeedsAttention ||
		got.NeedsAttentionReason() != AttentionLease ||
		len(got.Items()) != 0 {
		t.Fatalf("sharing-violation summary = %+v, want AttentionLease without items", got)
	}

	if err := windows.CloseHandle(held); err != nil {
		t.Fatal(err)
	}
	closed = true

	recovered, err := ListOperations(ctx, root)
	if err != nil || recovered.Status() != RecoveryListReady || recovered.UnknownEntries() || len(recovered.Summaries()) != 1 ||
		recovered.Summaries()[0].State() != RecoveryOperationResumable {
		t.Fatalf("recovered inventory = (%+v, %v), want ready resumable", recovered, err)
	}
}
