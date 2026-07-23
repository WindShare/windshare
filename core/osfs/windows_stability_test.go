//go:build windows

package osfs

import (
	"bytes"
	"context"
	"errors"
	"io"
	"math"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/windshare/windshare/core/catalog"
	"github.com/windshare/windshare/core/content"
	"golang.org/x/sys/windows"
)

type fakeWindowsRevisionPlatform struct {
	mu        sync.Mutex
	tokens    []windowsMutationToken
	tokenErrs []error
	root      *fakeWindowsRevisionRoot
	openErr   error
	openErrAt int
	opened    []string
}

func (p *fakeWindowsRevisionPlatform) OpenRoot(path string) (windowsRevisionRoot, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.opened = append(p.opened, path)
	if p.openErr != nil && (p.openErrAt == 0 || len(p.opened) == p.openErrAt) {
		return nil, p.openErr
	}
	if p.root == nil {
		return nil, errors.New("fake root unavailable")
	}
	return p.root, nil
}

func (p *fakeWindowsRevisionPlatform) Token(*os.File) (windowsMutationToken, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.tokenErrs) > 0 {
		err := p.tokenErrs[0]
		p.tokenErrs = p.tokenErrs[1:]
		if err != nil {
			return windowsMutationToken{}, err
		}
	}
	if len(p.tokens) == 0 {
		return windowsMutationToken{}, errors.New("fake preliminary tokens exhausted")
	}
	token := p.tokens[0]
	p.tokens = p.tokens[1:]
	return token, nil
}

type fakeWindowsRevisionRoot struct {
	mu          sync.Mutex
	file        windowsRevisionFile
	openErr     error
	paths       []string
	closeCall   int
	closeErr    error
	identity    [windowsRevisionIdentityBytes]byte
	identityErr error
}

func (r *fakeWindowsRevisionRoot) Identity() ([windowsRevisionIdentityBytes]byte, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.identity, r.identityErr
}

func (r *fakeWindowsRevisionRoot) OpenStable(path string) (windowsRevisionFile, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.paths = append(r.paths, path)
	return r.file, r.openErr
}

func (r *fakeWindowsRevisionRoot) Close() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.closeCall++
	return r.closeErr
}

type fakeWindowsRevisionFile struct {
	mu        sync.Mutex
	tokens    []windowsMutationToken
	tokenErrs []error
	data      []byte
	readToken *windowsMutationToken
	readErr   error
	closeErr  error
	closed    int
}

func (f *fakeWindowsRevisionFile) Token() (windowsMutationToken, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.tokenErrs) > 0 {
		err := f.tokenErrs[0]
		f.tokenErrs = f.tokenErrs[1:]
		if err != nil {
			return windowsMutationToken{}, err
		}
	}
	if len(f.tokens) == 0 {
		return windowsMutationToken{}, errors.New("fake stable tokens exhausted")
	}
	token := f.tokens[0]
	if len(f.tokens) > 1 {
		f.tokens = f.tokens[1:]
	}
	return token, nil
}

func (f *fakeWindowsRevisionFile) ReadAt(destination []byte, offset int64) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.readToken != nil {
		f.tokens = []windowsMutationToken{*f.readToken}
	}
	if offset < 0 || offset >= int64(len(f.data)) {
		return 0, io.EOF
	}
	count := copy(destination, f.data[offset:])
	if f.readErr != nil {
		return count, f.readErr
	}
	if count < len(destination) {
		return count, io.EOF
	}
	return count, nil
}

func (f *fakeWindowsRevisionFile) Close() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.closed++
	return f.closeErr
}

func windowsTestToken(seed byte, size uint64) windowsMutationToken {
	var identity [windowsRevisionIdentityBytes]byte
	identity[0] = seed
	identity[len(identity)-1] = seed + 10
	return windowsMutationToken{
		identity: identity, size: size,
		lastWrite: windowsFiletimeUnixOffset + 20_000_000, changeTime: int64(seed) + 30,
	}
}

func windowsTestRecord(t *testing.T, token windowsMutationToken, relative string) catalog.NodeRecord {
	t.Helper()
	identity, err := catalog.NewSourceIdentity(token.sourceIdentityBytes())
	if err != nil {
		t.Fatal(err)
	}
	candidate, err := catalog.NewVersionCandidate(token.candidateBytes())
	if err != nil {
		t.Fatal(err)
	}
	var file catalog.FileID
	file[0] = 1
	var parent catalog.DirectoryID
	parent[0] = 2
	locator, err := catalog.NewLocator(0, relative)
	if err != nil {
		t.Fatal(err)
	}
	record, err := catalog.NewFileNodeRecord(file, parent, filepath.Base(relative), locator, identity, candidate, token.size, catalog.ModifiedTime{})
	if err != nil {
		t.Fatal(err)
	}
	return record
}

func windowsPreliminaryFile(t *testing.T) *os.File {
	t.Helper()
	path := filepath.Join(t.TempDir(), "preliminary.bin")
	if err := os.WriteFile(path, []byte("data"), 0o600); err != nil {
		t.Fatal(err)
	}
	file, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	return file
}

func TestWindowsStabilityBinderUsesRootSlotAndChecksEveryMutationBoundary(t *testing.T) {
	baseline := windowsTestToken(1, 4)
	stableHandle := &fakeWindowsRevisionFile{
		tokens: []windowsMutationToken{baseline, baseline, baseline, baseline}, data: []byte("data"),
	}
	root := &fakeWindowsRevisionRoot{file: stableHandle}
	platform := &fakeWindowsRevisionPlatform{tokens: []windowsMutationToken{baseline, baseline}, root: root}
	binder, err := newWindowsStabilityBinder([]string{t.TempDir()}, platform)
	if err != nil {
		t.Fatal(err)
	}
	preliminary := windowsPreliminaryFile(t)
	stable, err := binder.BindStable(context.Background(), StableBinding{
		File: preliminary, Record: windowsTestRecord(t, baseline, "folder/data.bin"), RelativePath: "folder/data.bin",
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(root.paths) != 1 || root.paths[0] != filepath.FromSlash("folder/data.bin") {
		t.Fatalf("root-relative paths=%v", root.paths)
	}
	if _, err := preliminary.Stat(); err == nil {
		t.Fatal("successful binder did not consume preliminary handle")
	}
	if err := stable.Verify(context.Background()); err != nil {
		t.Fatal(err)
	}
	buffer := make([]byte, 4)
	if count, err := stable.ReadAt(context.Background(), buffer, 0); err != nil || count != 4 || !bytes.Equal(buffer, []byte("data")) {
		t.Fatalf("read=%q count=%d err=%v", buffer, count, err)
	}
	if !stable.ModifiedTime().Present() || stable.ExactSize() != 4 {
		t.Fatalf("metadata=%+v size=%d", stable.ModifiedTime(), stable.ExactSize())
	}
	if err := stable.Close(); err != nil {
		t.Fatal(err)
	}
	if err := binder.Close(); err != nil || root.closeCall != 1 || stableHandle.closed != 1 {
		t.Fatalf("binder close=%v root=%d file=%d", err, root.closeCall, stableHandle.closed)
	}
}

func TestWindowsStabilityBinderFailsClosedAtEveryBoundary(t *testing.T) {
	baseline := windowsTestToken(1, 4)
	changed := baseline
	changed.changeTime++
	openFailure := errors.Join(content.ErrUnsupportedStability, windows.ERROR_SHARING_VIOLATION)
	tests := []struct {
		name           string
		preliminary    []windowsMutationToken
		stable         []windowsMutationToken
		openErr        error
		bindingPath    string
		want           error
		wantOpen       bool
		wantFileClosed int
	}{
		{name: "candidate stale before open", preliminary: []windowsMutationToken{changed}, stable: []windowsMutationToken{baseline}, bindingPath: "file.bin", want: content.ErrRevisionStale},
		{name: "mutation during open", preliminary: []windowsMutationToken{baseline, changed}, stable: []windowsMutationToken{baseline}, bindingPath: "file.bin", want: content.ErrRevisionStale, wantOpen: true, wantFileClosed: 1},
		{name: "different locked object", preliminary: []windowsMutationToken{baseline, baseline}, stable: []windowsMutationToken{changed}, bindingPath: "file.bin", want: content.ErrRevisionStale, wantOpen: true, wantFileClosed: 1},
		{name: "writer already open", preliminary: []windowsMutationToken{baseline}, stable: []windowsMutationToken{baseline}, openErr: openFailure, bindingPath: "file.bin", want: content.ErrUnsupportedStability, wantOpen: true},
		{name: "empty relative path", preliminary: []windowsMutationToken{baseline}, stable: []windowsMutationToken{baseline}, want: content.ErrUnsupportedStability},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			handle := &fakeWindowsRevisionFile{tokens: test.stable, data: []byte("data")}
			root := &fakeWindowsRevisionRoot{file: handle, openErr: test.openErr}
			platform := &fakeWindowsRevisionPlatform{tokens: test.preliminary, root: root}
			binder, err := newWindowsStabilityBinder([]string{t.TempDir()}, platform)
			if err != nil {
				t.Fatal(err)
			}
			preliminary := windowsPreliminaryFile(t)
			_, err = binder.BindStable(context.Background(), StableBinding{
				File: preliminary, Record: windowsTestRecord(t, baseline, "file.bin"), RelativePath: test.bindingPath,
			})
			if !errors.Is(err, test.want) {
				t.Fatalf("error=%v want=%v", err, test.want)
			}
			if got := len(root.paths) != 0; got != test.wantOpen {
				t.Fatalf("opened=%v paths=%v", got, root.paths)
			}
			if handle.closed != test.wantFileClosed {
				t.Fatalf("stable handle closes=%d want=%d", handle.closed, test.wantFileClosed)
			}
			_ = preliminary.Close()
			_ = binder.Close()
		})
	}
}

func TestWindowsStabilityBinderRejectsConfigurationAndRootOpenFailure(t *testing.T) {
	root := &fakeWindowsRevisionRoot{}
	platform := &fakeWindowsRevisionPlatform{root: root}
	if _, err := newWindowsStabilityBinder(nil, platform); !errors.Is(err, content.ErrUnsupportedStability) {
		t.Fatalf("empty root set error=%v", err)
	}
	tooMany := make([]string, int(catalog.MaxRootSlots)+1)
	for index := range tooMany {
		tooMany[index] = t.TempDir()
	}
	if _, err := newWindowsStabilityBinder(tooMany, platform); !errors.Is(err, content.ErrUnsupportedStability) {
		t.Fatalf("oversized root set error=%v", err)
	}
	if _, err := newWindowsStabilityBinder([]string{t.TempDir()}, nil); !errors.Is(err, content.ErrUnsupportedStability) {
		t.Fatalf("nil platform error=%v", err)
	}

	openFailure := errors.New("native root open failed")
	closeFailure := errors.New("retained root close failed")
	root.closeErr = closeFailure
	platform = &fakeWindowsRevisionPlatform{root: root, openErr: openFailure, openErrAt: 2}
	if _, err := newWindowsStabilityBinder([]string{t.TempDir(), t.TempDir()}, platform); !errors.Is(err, openFailure) || !errors.Is(err, closeFailure) || root.closeCall != 1 {
		t.Fatalf("partial root cleanup closes=%d err=%v", root.closeCall, err)
	}
}

func TestWindowsRootFactoryRejectsSplitRetainedAuthority(t *testing.T) {
	path := t.TempDir()
	root := &fakeWindowsRevisionRoot{}
	binder, err := newWindowsStabilityBinder([]string{path}, &fakeWindowsRevisionPlatform{root: root})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := newOwnedRootedRevisionSource([]string{path}, binder); !errors.Is(err, content.ErrUnsupportedStability) {
		t.Fatalf("split root authority error=%v", err)
	}
	if root.closeCall != 1 {
		t.Fatalf("native root cleanup calls=%d", root.closeCall)
	}
}

func TestWindowsPersistentIdentityUsesFullReFSWidth(t *testing.T) {
	first := windowsTestToken(1, 4)
	second := first
	second.identity[len(second.identity)-1]++
	if first.sameOpenedRevision(second) || bytes.Equal(first.sourceIdentityBytes(), second.sourceIdentityBytes()) ||
		bytes.Equal(first.candidateBytes(), second.candidateBytes()) {
		t.Fatal("distinct 128-bit file IDs collapsed to the legacy 64-bit identity")
	}
	firstOutput, err := windowsOutputObjectIdentity(first.identity)
	if err != nil {
		t.Fatal(err)
	}
	secondOutput, err := windowsOutputObjectIdentity(second.identity)
	if err != nil {
		t.Fatal(err)
	}
	if firstOutput == secondOutput {
		t.Fatal("output identity ignored the high file-ID bytes")
	}
}

func TestWindowsSourceAndOutputVolumeSupportMatrix(t *testing.T) {
	localNTFS := windowsVolume{
		filesystem: "NTFS", path: `\\?\C:\root`, driveType: windows.DRIVE_FIXED,
		flags: windows.FILE_SUPPORTS_HARD_LINKS | windowsFileSupportsPOSIXUnlinkRename,
	}
	localReFS := windowsVolume{
		filesystem: "ReFS", path: `\\?\R:\root`, driveType: windows.DRIVE_REMOVABLE,
		flags: windows.FILE_SUPPORTS_HARD_LINKS | windowsFileSupportsPOSIXUnlinkRename,
	}
	for _, supported := range []windowsVolume{localNTFS, localReFS} {
		if err := validateWindowsLocalPersistentVolume(supported); err != nil {
			t.Fatalf("stable volume %+v: %v", supported, err)
		}
		if err := validateWindowsOutputVolume(supported); err != nil {
			t.Fatalf("output volume %+v: %v", supported, err)
		}
	}
	unsupported := []windowsVolume{
		{filesystem: "FAT32", path: `\\?\F:\root`, driveType: windows.DRIVE_REMOVABLE, flags: localNTFS.flags},
		{filesystem: "NTFS", path: `\\?\UNC\server\share\root`, driveType: windows.DRIVE_REMOTE, flags: localNTFS.flags},
		{filesystem: "NTFS", path: `\\?\Z:\root`, driveType: windows.DRIVE_REMOTE, flags: localNTFS.flags},
	}
	for _, volume := range unsupported {
		if err := validateWindowsLocalPersistentVolume(volume); err == nil {
			t.Fatalf("unsupported stable volume admitted: %+v", volume)
		}
		if err := validateWindowsOutputVolume(volume); !errors.Is(err, ErrUnsupportedOutputVolume) {
			t.Fatalf("unsupported output volume %+v error=%v", volume, err)
		}
	}
	withoutHardLinks := localReFS
	withoutHardLinks.flags = 0
	if err := validateWindowsOutputVolume(withoutHardLinks); !errors.Is(err, ErrUnsupportedOutputVolume) {
		t.Fatalf("hard-link-free ReFS output error=%v", err)
	}
	withoutHandleUnlink := localNTFS
	withoutHandleUnlink.flags = windows.FILE_SUPPORTS_HARD_LINKS
	if err := validateWindowsOutputVolume(withoutHandleUnlink); !errors.Is(err, ErrUnsupportedOutputVolume) {
		t.Fatalf("path-racy NTFS output error=%v", err)
	}
}

func TestWindowsOutputCleanupFollowsVerifiedHandleAcrossReplacement(t *testing.T) {
	rootPath := t.TempDir()
	originalPath := filepath.Join(rootPath, "owned.bin")
	movedPath := filepath.Join(rootPath, "owned-moved.bin")
	if err := os.WriteFile(originalPath, []byte("ours"), filePerm); err != nil {
		t.Fatal(err)
	}
	root, err := os.OpenRoot(rootPath)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	file, err := openOutputRemovalFile(root, "owned.bin")
	if err != nil {
		t.Fatal(err)
	}
	identity, err := outputObjectIdentity(file)
	if err != nil {
		_ = file.Close()
		t.Fatal(err)
	}
	if err := os.Rename(originalPath, movedPath); err != nil {
		_ = file.Close()
		t.Fatal(err)
	}
	if err := os.WriteFile(originalPath, []byte("foreign"), filePerm); err != nil {
		_ = file.Close()
		t.Fatal(err)
	}
	current, err := outputObjectIdentity(file)
	if err != nil || current != identity {
		_ = file.Close()
		t.Fatalf("opened identity changed=%v err=%v", current, err)
	}
	if err := errors.Join(removeOpenedOutputFile(root, "owned.bin", file), file.Close()); err != nil {
		t.Fatal(err)
	}
	if got, err := os.ReadFile(originalPath); err != nil || string(got) != "foreign" {
		t.Fatalf("replacement output=%q err=%v", got, err)
	}
	if _, err := os.Stat(movedPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("verified owned object survived handle deletion: %v", err)
	}
}

func TestWindowsStabilityBinderPropagatesSyscallBoundaryFailures(t *testing.T) {
	baseline := windowsTestToken(3, 4)
	record := windowsTestRecord(t, baseline, "file.bin")
	sentinel := errors.New("syscall inspection failed")
	tests := []struct {
		name        string
		ctx         context.Context
		path        string
		slot        catalog.RootSlot
		platformErr []error
		stableErr   []error
		closeFirst  bool
		want        error
		wantClosed  int
	}{
		{name: "canceled", ctx: func() context.Context { ctx, cancel := context.WithCancel(context.Background()); cancel(); return ctx }(), path: "file.bin", want: context.Canceled},
		{name: "nonlocal path", ctx: context.Background(), path: "../file.bin", want: content.ErrRevisionStale},
		{name: "unknown root slot", ctx: context.Background(), path: "file.bin", slot: 1, want: content.ErrRevisionStale},
		{name: "preliminary token", ctx: context.Background(), path: "file.bin", platformErr: []error{sentinel}, want: sentinel},
		{name: "post-open token", ctx: context.Background(), path: "file.bin", platformErr: []error{nil, sentinel}, want: sentinel, wantClosed: 1},
		{name: "stable token", ctx: context.Background(), path: "file.bin", stableErr: []error{sentinel}, want: sentinel, wantClosed: 1},
		{name: "closed binder", ctx: context.Background(), path: "file.bin", closeFirst: true, want: content.ErrRevisionStoreClosed},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			handle := &fakeWindowsRevisionFile{tokens: []windowsMutationToken{baseline}, tokenErrs: test.stableErr, data: []byte("data")}
			root := &fakeWindowsRevisionRoot{file: handle}
			platform := &fakeWindowsRevisionPlatform{
				root: root, tokens: []windowsMutationToken{baseline, baseline}, tokenErrs: test.platformErr,
			}
			binder, err := newWindowsStabilityBinder([]string{t.TempDir()}, platform)
			if err != nil {
				t.Fatal(err)
			}
			if test.closeFirst {
				_ = binder.Close()
			}
			preliminary := windowsPreliminaryFile(t)
			_, err = binder.BindStable(test.ctx, StableBinding{
				File: preliminary, Record: record, RootSlot: test.slot, RelativePath: test.path,
			})
			if !errors.Is(err, test.want) || handle.closed != test.wantClosed {
				t.Fatalf("error=%v want=%v stable closes=%d", err, test.want, handle.closed)
			}
			_ = preliminary.Close()
			_ = binder.Close()
		})
	}
}

func TestWindowsStableFileDetectsMutationBeforeAndDuringRead(t *testing.T) {
	baseline := windowsTestToken(1, 4)
	changed := baseline
	changed.lastWrite++
	before := &windowsStableFile{
		handle: &fakeWindowsRevisionFile{tokens: []windowsMutationToken{changed}, data: []byte("data")}, baseline: baseline,
	}
	if err := before.Verify(context.Background()); !errors.Is(err, content.ErrSourceDrift) {
		t.Fatalf("before-read drift=%v", err)
	}
	duringHandle := &fakeWindowsRevisionFile{
		tokens: []windowsMutationToken{baseline}, data: []byte("data"), readToken: &changed,
	}
	during := &windowsStableFile{handle: duringHandle, baseline: baseline}
	if _, err := during.ReadAt(context.Background(), make([]byte, 4), 0); !errors.Is(err, content.ErrSourceDrift) {
		t.Fatalf("during-read drift=%v", err)
	}
	if _, err := during.ReadAt(context.Background(), make([]byte, 1), math.MaxUint64); !errors.Is(err, content.ErrBlockOutOfRange) {
		t.Fatalf("overflow read=%v", err)
	}
	_ = during.Close()
	if err := during.Verify(context.Background()); !errors.Is(err, content.ErrSourceDrift) {
		t.Fatalf("closed verification=%v", err)
	}
}

func TestWindowsStableFileContextIOAndCloseFailures(t *testing.T) {
	baseline := windowsTestToken(5, 4)
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	stable := &windowsStableFile{handle: &fakeWindowsRevisionFile{tokens: []windowsMutationToken{baseline}}, baseline: baseline}
	if err := stable.Verify(canceled); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled verify=%v", err)
	}
	if _, err := stable.ReadAt(canceled, make([]byte, 1), 0); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled read=%v", err)
	}

	sentinel := errors.New("stable syscall failed")
	stable = &windowsStableFile{handle: &fakeWindowsRevisionFile{tokenErrs: []error{sentinel}}, baseline: baseline}
	if err := stable.Verify(context.Background()); !errors.Is(err, sentinel) {
		t.Fatalf("token failure=%v", err)
	}

	metadataOnly := baseline
	metadataOnly.changeTime++
	stable = &windowsStableFile{handle: &fakeWindowsRevisionFile{tokens: []windowsMutationToken{metadataOnly}}, baseline: baseline}
	if err := stable.Verify(context.Background()); err != nil {
		t.Fatalf("rename-only change time rejected: %v", err)
	}

	eofHandle := &fakeWindowsRevisionFile{tokens: []windowsMutationToken{baseline}, data: []byte("data"), readErr: io.EOF}
	stable = &windowsStableFile{handle: eofHandle, baseline: baseline}
	if count, err := stable.ReadAt(context.Background(), make([]byte, 4), 0); err != nil || count != 4 {
		t.Fatalf("exact EOF normalization count=%d err=%v", count, err)
	}
	shortHandle := &fakeWindowsRevisionFile{tokens: []windowsMutationToken{baseline}, data: []byte("data")}
	stable = &windowsStableFile{handle: shortHandle, baseline: baseline}
	if count, err := stable.ReadAt(context.Background(), make([]byte, 5), 0); !errors.Is(err, io.EOF) || count != 4 {
		t.Fatalf("short read count=%d err=%v", count, err)
	}
	readFailure := &fakeWindowsRevisionFile{tokens: []windowsMutationToken{baseline}, data: []byte("data"), readErr: sentinel}
	stable = &windowsStableFile{handle: readFailure, baseline: baseline}
	if _, err := stable.ReadAt(context.Background(), make([]byte, 4), 0); !errors.Is(err, sentinel) {
		t.Fatalf("read syscall error=%v", err)
	}
	closeFailure := &fakeWindowsRevisionFile{closeErr: sentinel}
	stable = &windowsStableFile{handle: closeFailure, baseline: baseline}
	if err := stable.Close(); !errors.Is(err, sentinel) {
		t.Fatalf("close error=%v", err)
	}
	if err := stable.Close(); err != nil || closeFailure.closed != 1 {
		t.Fatalf("idempotent close=%v calls=%d", err, closeFailure.closed)
	}
}

func TestWindowsStableNativeOpenContractDeniesWritesAndReparseTraversal(t *testing.T) {
	if windowsStableDesiredAccess() != windows.FILE_GENERIC_READ {
		t.Fatalf("stable desired access=%#x", windowsStableDesiredAccess())
	}
	if windowsStableShareMode()&windows.FILE_SHARE_WRITE != 0 ||
		windowsStableShareMode() != windows.FILE_SHARE_READ|windows.FILE_SHARE_DELETE {
		t.Fatalf("stable share mode=%#x", windowsStableShareMode())
	}
	wantOptions := uint32(windows.FILE_NON_DIRECTORY_FILE | windows.FILE_OPEN_REPARSE_POINT |
		windows.FILE_RANDOM_ACCESS | windows.FILE_SYNCHRONOUS_IO_NONALERT)
	if windowsStableOpenOptions() != wantOptions {
		t.Fatalf("stable open options=%#x want=%#x", windowsStableOpenOptions(), wantOptions)
	}
	if got := windowsNTPath(`C:\root\file`); got != `\??\C:\root\file` {
		t.Fatalf("NT path=%q", got)
	}
	if got := windowsNTPath(`\\server\share\file`); got != `\??\UNC\server\share\file` {
		t.Fatalf("UNC NT path=%q", got)
	}
	if got := windowsNTPath(`\\?\C:\root\file`); got != `\??\C:\root\file` {
		t.Fatalf("extended NT path=%q", got)
	}
	if got := windowsNTPath(`\\?\UNC\server\share\file`); got != `\??\UNC\server\share\file` {
		t.Fatalf("extended UNC NT path=%q", got)
	}
	for _, unsupported := range []error{windows.ERROR_INVALID_PARAMETER, windows.ERROR_NOT_SUPPORTED, windows.ERROR_CALL_NOT_IMPLEMENTED} {
		if !errors.Is(classifyWindowsStableOpenError(unsupported), content.ErrUnsupportedStability) ||
			!errors.Is(classifyWindowsRootOpenError(unsupported), content.ErrUnsupportedStability) ||
			!errors.Is(classifyWindowsIdentityError(unsupported), content.ErrUnsupportedStability) {
			t.Fatalf("unsupported native contract error=%v", unsupported)
		}
	}
	if !errors.Is(classifyWindowsStableOpenError(windows.ERROR_FILE_NOT_FOUND), content.ErrRevisionStale) {
		t.Fatal("missing stable path was not classified as a stale revision")
	}
	sentinel := errors.New("unclassified native failure")
	if !errors.Is(classifyWindowsStableOpenError(sentinel), sentinel) || !errors.Is(classifyWindowsRootOpenError(sentinel), sentinel) ||
		!errors.Is(normalizeWindowsNTError(sentinel), sentinel) {
		t.Fatal("unclassified native error identity was lost")
	}
	status := windows.NTStatus(0xC000000F)
	if !errors.Is(normalizeWindowsNTError(status), status.Errno()) {
		t.Fatalf("NTSTATUS normalization=%v", normalizeWindowsNTError(status))
	}
}

func TestWindowsNativeBaselineAndRootFailuresAreExplicit(t *testing.T) {
	if _, _, err := WindowsCatalogBaseline(nil); !errors.Is(err, content.ErrUnsupportedStability) {
		t.Fatalf("nil catalog baseline error=%v", err)
	}
	rootPath := t.TempDir()
	directory, err := os.Open(rootPath)
	if err != nil {
		t.Fatal(err)
	}
	_, _, baselineErr := WindowsCatalogBaseline(directory)
	_ = directory.Close()
	if errors.Is(baselineErr, content.ErrUnsupportedStability) {
		t.Skipf("test volume is outside the Windows stability support matrix: %v", baselineErr)
	}
	if !errors.Is(baselineErr, content.ErrRevisionStale) {
		t.Fatalf("directory catalog baseline error=%v", baselineErr)
	}
	closed, err := os.Open(filepath.Join(rootPath, "closed.bin"))
	if errors.Is(err, os.ErrNotExist) {
		if writeErr := os.WriteFile(filepath.Join(rootPath, "closed.bin"), []byte("x"), filePerm); writeErr != nil {
			t.Fatal(writeErr)
		}
		closed, err = os.Open(filepath.Join(rootPath, "closed.bin"))
	}
	if err != nil {
		t.Fatal(err)
	}
	_ = closed.Close()
	if _, _, err := WindowsCatalogBaseline(closed); err == nil {
		t.Fatal("closed catalog handle admitted")
	}

	missing := filepath.Join(t.TempDir(), "missing")
	if _, err := NewWindowsStabilityBinder([]string{rootPath, missing}); err == nil {
		t.Fatal("missing retained root admitted")
	}
	rootFile := filepath.Join(t.TempDir(), "not-a-root")
	if err := os.WriteFile(rootFile, []byte("x"), filePerm); err != nil {
		t.Fatal(err)
	}
	if _, err := NewWindowsStabilityBinder([]string{rootFile}); err == nil {
		t.Fatal("regular file admitted as retained root")
	}
	if _, err := NewWindowsRootedRevisionSource(nil); !errors.Is(err, content.ErrUnsupportedStability) {
		t.Fatalf("empty Windows source roots error=%v", err)
	}
	binder, err := NewWindowsStabilityBinder([]string{rootPath})
	if err != nil {
		t.Fatal(err)
	}
	nativeRoot := binder.roots[0]
	if _, err := nativeRoot.OpenStable("missing.bin"); !errors.Is(err, content.ErrRevisionStale) {
		t.Fatalf("missing root-relative native open=%v", err)
	}
	if _, err := nativeRoot.OpenStable("../outside.bin"); !errors.Is(err, content.ErrRevisionStale) {
		t.Fatalf("nonlocal root-relative native open=%v", err)
	}
	if err := binder.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := nativeRoot.OpenStable("closed.bin"); !errors.Is(err, content.ErrRevisionStoreClosed) {
		t.Fatalf("closed native root open=%v", err)
	}
	if err := binder.Close(); err != nil {
		t.Fatalf("second binder close=%v", err)
	}
}

func TestWindowsRootedRevisionSourceRealWriteExclusionAndReplacement(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "source.bin")
	moved := filepath.Join(root, "source-moved.bin")
	original := []byte("original revision")
	if err := os.WriteFile(path, original, 0o600); err != nil {
		t.Fatal(err)
	}
	baselineHandle, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	identity, candidate, err := WindowsCatalogBaseline(baselineHandle)
	_ = baselineHandle.Close()
	if errors.Is(err, content.ErrUnsupportedStability) {
		t.Skipf("Windows test volume is outside the frozen NTFS/ReFS support matrix: %v", err)
	}
	if err != nil {
		t.Fatal(err)
	}
	var fileID catalog.FileID
	fileID[0] = 1
	var parent catalog.DirectoryID
	parent[0] = 2
	locator, _ := catalog.NewLocator(0, "source.bin")
	record, err := catalog.NewFileNodeRecord(fileID, parent, "source.bin", locator, identity, candidate, uint64(len(original)), catalog.ModifiedTime{})
	if err != nil {
		t.Fatal(err)
	}
	source, err := NewWindowsRootedRevisionSource([]string{root})
	if err != nil {
		t.Fatal(err)
	}
	defer source.Close()
	stable, err := source.OpenStable(context.Background(), record)
	if err != nil {
		t.Fatal(err)
	}
	defer stable.Close()

	name, _ := windows.UTF16PtrFromString(path)
	writer, writeErr := windows.CreateFile(
		name, windows.GENERIC_WRITE,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE,
		nil, windows.OPEN_EXISTING, windows.FILE_ATTRIBUTE_NORMAL, 0,
	)
	if writeErr == nil {
		_ = windows.CloseHandle(writer)
		t.Fatal("write-excluding stable handle admitted a concurrent writer")
	}
	if !errors.Is(writeErr, windows.ERROR_SHARING_VIOLATION) {
		t.Fatalf("concurrent writer error=%v", writeErr)
	}
	if err := os.Rename(path, moved); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("replacement revision"), 0o600); err != nil {
		t.Fatal(err)
	}
	buffer := make([]byte, len(original))
	if _, err := stable.ReadAt(context.Background(), buffer, 0); err != nil || !bytes.Equal(buffer, original) {
		t.Fatalf("stable replacement read=%q err=%v", buffer, err)
	}
	entries, err := os.ReadDir(root)
	if err != nil || len(entries) != 2 {
		t.Fatalf("source directory entries=%v err=%v", entries, err)
	}
}

func TestWindowsStableSourceRejectsPreexistingWritableMapping(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "mapped.bin")
	original := []byte("original revision")
	if err := os.WriteFile(path, original, 0o600); err != nil {
		t.Fatal(err)
	}
	name, _ := windows.UTF16PtrFromString(path)
	writer, err := windows.CreateFile(
		name, windows.GENERIC_READ|windows.GENERIC_WRITE,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE,
		nil, windows.OPEN_EXISTING, windows.FILE_ATTRIBUTE_NORMAL, 0,
	)
	if err != nil {
		t.Fatal(err)
	}
	mapping, err := windows.CreateFileMapping(writer, nil, windows.PAGE_READWRITE, 0, uint32(len(original)), nil)
	if err != nil {
		_ = windows.CloseHandle(writer)
		t.Fatal(err)
	}
	view, err := windows.MapViewOfFile(mapping, windows.FILE_MAP_WRITE, 0, 0, uintptr(len(original)))
	if err != nil {
		_ = windows.CloseHandle(mapping)
		_ = windows.CloseHandle(writer)
		t.Fatal(err)
	}
	defer windows.UnmapViewOfFile(view)
	_ = windows.CloseHandle(mapping)
	_ = windows.CloseHandle(writer)

	baseline, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	identity, candidate, err := WindowsCatalogBaseline(baseline)
	_ = baseline.Close()
	if errors.Is(err, content.ErrUnsupportedStability) {
		t.Skipf("Windows test volume is outside the frozen support matrix: %v", err)
	}
	if err != nil {
		t.Fatal(err)
	}
	var fileID catalog.FileID
	fileID[0] = 1
	var parent catalog.DirectoryID
	parent[0] = 2
	locator, _ := catalog.NewLocator(0, "mapped.bin")
	record, err := catalog.NewFileNodeRecord(fileID, parent, "mapped.bin", locator, identity, candidate, uint64(len(original)), catalog.ModifiedTime{})
	if err != nil {
		t.Fatal(err)
	}
	source, err := NewWindowsRootedRevisionSource([]string{root})
	if err != nil {
		t.Fatal(err)
	}
	defer source.Close()
	stable, err := source.OpenStable(context.Background(), record)
	if stable != nil || !errors.Is(err, content.ErrUnsupportedStability) || !errors.Is(err, windows.ERROR_SHARING_VIOLATION) {
		if stable != nil {
			_ = stable.Close()
		}
		t.Fatalf("preexisting writable mapping stable=%v error=%v", stable, err)
	}
}

func TestWindowsRootedRevisionSourceRejectsExistingWriterAndIntermediateReparse(t *testing.T) {
	root := t.TempDir()
	inside := filepath.Join(root, "inside")
	if err := os.Mkdir(inside, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(inside, "source.bin")
	if err := os.WriteFile(path, []byte("data"), 0o600); err != nil {
		t.Fatal(err)
	}
	baseline, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	identity, candidate, err := WindowsCatalogBaseline(baseline)
	_ = baseline.Close()
	if errors.Is(err, content.ErrUnsupportedStability) {
		t.Skipf("Windows test volume is outside the frozen support matrix: %v", err)
	}
	if err != nil {
		t.Fatal(err)
	}
	makeRecord := func(relative string) catalog.NodeRecord {
		var fileID catalog.FileID
		fileID[0] = 1
		var parent catalog.DirectoryID
		parent[0] = 2
		locator, _ := catalog.NewLocator(0, relative)
		record, recordErr := catalog.NewFileNodeRecord(fileID, parent, filepath.Base(relative), locator, identity, candidate, 4, catalog.ModifiedTime{})
		if recordErr != nil {
			t.Fatal(recordErr)
		}
		return record
	}
	source, err := NewWindowsRootedRevisionSource([]string{root})
	if err != nil {
		t.Fatal(err)
	}
	defer source.Close()

	name, _ := windows.UTF16PtrFromString(path)
	writer, err := windows.CreateFile(
		name, windows.GENERIC_WRITE,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE,
		nil, windows.OPEN_EXISTING, windows.FILE_ATTRIBUTE_NORMAL, 0,
	)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := source.OpenStable(context.Background(), makeRecord("inside/source.bin")); !errors.Is(err, content.ErrUnsupportedStability) {
		t.Fatalf("existing writer error=%v", err)
	}
	_ = windows.CloseHandle(writer)

	alias := filepath.Join(root, "alias")
	if err := os.Symlink("inside", alias); err != nil {
		t.Skipf("directory symlink unavailable: %v", err)
	}
	if _, err := source.OpenStable(context.Background(), makeRecord("alias/source.bin")); !errors.Is(err, content.ErrRevisionStale) {
		t.Fatalf("intermediate reparse error=%v", err)
	}
}
