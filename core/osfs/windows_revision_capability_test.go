//go:build windows

package osfs

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"syscall"
	"testing"

	"github.com/windshare/windshare/core/catalog"
	"github.com/windshare/windshare/core/content"
	"golang.org/x/sys/windows"
)

type fakeWindowsRevisionMetadata struct {
	identity       windowsRevisionFileIDInfo
	identityErr    error
	information    windows.ByHandleFileInformation
	informationErr error
	basic          windowsFileBasicInfo
	basicErr       error
	filesystem     string
	filesystemErr  error
	path           string
	pathErr        error
	volumeCalls    int
	driveType      uint32
	driveErr       error
}

func (api *fakeWindowsRevisionMetadata) FileIdentity(windows.Handle) (windowsRevisionFileIDInfo, error) {
	return api.identity, api.identityErr
}
func (api *fakeWindowsRevisionMetadata) FileInformation(windows.Handle) (windows.ByHandleFileInformation, error) {
	return api.information, api.informationErr
}
func (api *fakeWindowsRevisionMetadata) BasicInformation(windows.Handle) (windowsFileBasicInfo, error) {
	return api.basic, api.basicErr
}
func (api *fakeWindowsRevisionMetadata) Filesystem(windows.Handle) (string, error) {
	api.volumeCalls++
	return api.filesystem, api.filesystemErr
}
func (api *fakeWindowsRevisionMetadata) FinalPath(windows.Handle) (string, error) {
	return api.path, api.pathErr
}

func (api *fakeWindowsRevisionMetadata) DriveType(string) (uint32, error) {
	return api.driveType, api.driveErr
}

func windowsMetadataFixture() fakeWindowsRevisionMetadata {
	return fakeWindowsRevisionMetadata{
		identity: windowsRevisionFileIDInfo{VolumeSerialNumber: 9, FileID: [16]byte{1, 2, 3}},
		information: windows.ByHandleFileInformation{
			VolumeSerialNumber: 9, FileIndexHigh: 3, FileIndexLow: 4, FileSizeLow: 4,
			CreationTime:  windows.Filetime{LowDateTime: 19},
			LastWriteTime: windows.Filetime{HighDateTime: 27, LowDateTime: 31},
		},
		basic:      windowsFileBasicInfo{LastWriteTime: windowsFiletimeUnixOffset + 1, ChangeTime: windowsFiletimeUnixOffset + 2},
		filesystem: "NTFS", path: `C:\file.bin`, driveType: windows.DRIVE_FIXED,
	}
}

func TestWindowsRevisionMetadataUsesCapabilitiesAndExplicitContinuity(t *testing.T) {
	tests := []struct {
		name      string
		configure func(*fakeWindowsRevisionMetadata)
		want      content.RevisionContinuity
		wantTag   byte
	}{
		{"full-width local", func(*fakeWindowsRevisionMetadata) {}, content.CatalogRevisionContinuity, windowsIdentityFullWidth},
		{"full-width local ReFS", func(api *fakeWindowsRevisionMetadata) { api.filesystem = "ReFS" }, content.CatalogRevisionContinuity, windowsIdentityFullWidth},
		{"full-width FAT with change time", func(api *fakeWindowsRevisionMetadata) { api.filesystem = "FAT32" }, content.OpenHandleRevisionContinuity, windowsIdentityFullWidth},
		{"full-width NAS with change time", func(api *fakeWindowsRevisionMetadata) { api.path = `\\?\UNC\nas\share\file` }, content.OpenHandleRevisionContinuity, windowsIdentityFullWidth},
		{"full-width mapped NAS", func(api *fakeWindowsRevisionMetadata) { api.path = `Z:\file`; api.driveType = windows.DRIVE_REMOTE }, content.OpenHandleRevisionContinuity, windowsIdentityFullWidth},
		{"unavailable locality", func(api *fakeWindowsRevisionMetadata) { api.pathErr = windows.ERROR_ACCESS_DENIED }, content.OpenHandleRevisionContinuity, windowsIdentityFullWidth},
		{"unknown locality", func(api *fakeWindowsRevisionMetadata) { api.driveType = windows.DRIVE_UNKNOWN }, content.OpenHandleRevisionContinuity, windowsIdentityFullWidth},
		{"failed drive probe", func(api *fakeWindowsRevisionMetadata) { api.driveErr = windows.ERROR_ACCESS_DENIED }, content.OpenHandleRevisionContinuity, windowsIdentityFullWidth},
		{"full-width UNC", func(api *fakeWindowsRevisionMetadata) {
			api.path = `\\?\UNC\nas\share\file`
			api.filesystem = "remote"
			api.filesystemErr = windows.ERROR_NOT_SUPPORTED
		}, content.OpenHandleRevisionContinuity, windowsIdentityFullWidth},
		{"FAT32", func(api *fakeWindowsRevisionMetadata) {
			api.identityErr = windows.ERROR_INVALID_PARAMETER
			api.filesystem = "FAT32"
			api.basic.ChangeTime = 0
		}, content.OpenHandleRevisionContinuity, windowsIdentityLegacy},
		{"exFAT", func(api *fakeWindowsRevisionMetadata) {
			api.identityErr = windows.ERROR_NOT_SUPPORTED
			api.filesystem = "exFAT"
			api.basic.ChangeTime = 0
		}, content.OpenHandleRevisionContinuity, windowsIdentityLegacy},
		{"legacy NAS", func(api *fakeWindowsRevisionMetadata) {
			api.identityErr = windows.ERROR_INVALID_FUNCTION
			api.filesystem = "NAS"
			api.path = `\\nas\share\file`
		}, content.OpenHandleRevisionContinuity, windowsIdentityLegacy},
		{"no extended basic metadata", func(api *fakeWindowsRevisionMetadata) { api.basicErr = windows.ERROR_CALL_NOT_IMPLEMENTED }, content.OpenHandleRevisionContinuity, windowsIdentityFullWidth},
		{"zero change time", func(api *fakeWindowsRevisionMetadata) { api.basic.ChangeTime = 0 }, content.OpenHandleRevisionContinuity, windowsIdentityFullWidth},
		{"negative change time", func(api *fakeWindowsRevisionMetadata) { api.basic.ChangeTime = -1 }, content.OpenHandleRevisionContinuity, windowsIdentityFullWidth},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			api := windowsMetadataFixture()
			test.configure(&api)
			token, directory, err := inspectWindowsObjectToken(1, &api)
			if err != nil || directory || token.size != 4 || token.identity[0] != test.wantTag || token.continuity() != test.want {
				t.Fatalf("token=%+v directory=%v err=%v", token, directory, err)
			}
			if api.basicErr != nil && token.lastWrite != int64(uint64(api.information.LastWriteTime.HighDateTime)<<32|uint64(api.information.LastWriteTime.LowDateTime)) {
				t.Fatal("fallback lost observed last-write metadata")
			}
			if test.wantTag == windowsIdentityLegacy && binary.BigEndian.Uint64(token.identity[9:17]) != uint64(api.information.FileIndexHigh)<<32|uint64(api.information.FileIndexLow) {
				t.Fatal("legacy handle identity was truncated")
			}
			record := windowsTestRecord(t, token, "file.bin")
			binder := &WindowsStabilityBinder{roots: []windowsRevisionRoot{&fakeWindowsRevisionRoot{profile: token.profile}}}
			continuity, err := binder.RevisionContinuity(record)
			if err != nil || continuity != test.want {
				t.Fatalf("continuity=%v err=%v", continuity, err)
			}
		})
	}
}

func TestWindowsRevisionMetadataRejectsMissingIdentityAndOperationalFailures(t *testing.T) {
	tests := []struct {
		name      string
		configure func(*fakeWindowsRevisionMetadata)
		want      error
	}{
		{"ReFS legacy collision", func(api *fakeWindowsRevisionMetadata) {
			api.identityErr = windows.ERROR_NOT_SUPPORTED
			api.filesystem = "rEfS"
		}, content.ErrUnsupportedStability},
		{"unknown legacy semantics", func(api *fakeWindowsRevisionMetadata) {
			api.identityErr = windows.ERROR_NOT_SUPPORTED
			api.filesystem = ""
		}, content.ErrUnsupportedStability},
		{"unavailable legacy semantics", func(api *fakeWindowsRevisionMetadata) {
			api.identityErr = windows.ERROR_NOT_SUPPORTED
			api.filesystemErr = windows.ERROR_ACCESS_DENIED
		}, content.ErrUnsupportedStability},
		{"empty full identity", func(api *fakeWindowsRevisionMetadata) { api.identity.FileID = [16]byte{} }, content.ErrUnsupportedStability},
		{"empty legacy file identity", func(api *fakeWindowsRevisionMetadata) {
			api.identityErr = windows.ERROR_NOT_SUPPORTED
			api.information.FileIndexHigh = 0
			api.information.FileIndexLow = 0
			api.path = `C:\file`
		}, content.ErrUnsupportedStability},
		{"empty legacy directory identity", func(api *fakeWindowsRevisionMetadata) {
			api.identityErr = windows.ERROR_NOT_SUPPORTED
			api.information.FileIndexHigh = 0
			api.information.FileIndexLow = 0
			api.information.FileAttributes = windows.FILE_ATTRIBUTE_DIRECTORY
			api.path = `C:\directory`
		}, content.ErrUnsupportedStability},
		{"identity permission failure", func(api *fakeWindowsRevisionMetadata) { api.identityErr = windows.ERROR_ACCESS_DENIED }, windows.ERROR_ACCESS_DENIED},
		{"information failure", func(api *fakeWindowsRevisionMetadata) { api.informationErr = windows.ERROR_INVALID_HANDLE }, windows.ERROR_INVALID_HANDLE},
		{"basic permission failure", func(api *fakeWindowsRevisionMetadata) { api.basicErr = windows.ERROR_ACCESS_DENIED }, windows.ERROR_ACCESS_DENIED},
		{"reparse object", func(api *fakeWindowsRevisionMetadata) {
			api.information.FileAttributes = windows.FILE_ATTRIBUTE_REPARSE_POINT
		}, content.ErrRevisionStale},
		{"size overflow", func(api *fakeWindowsRevisionMetadata) { api.information.FileSizeHigh = ^uint32(0) }, content.ErrRevisionStale},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			api := windowsMetadataFixture()
			test.configure(&api)
			_, _, err := inspectWindowsObjectToken(1, &api)
			if !errors.Is(err, test.want) {
				t.Fatalf("err=%v want=%v", err, test.want)
			}
		})
	}
}

func TestWindowsLegacyVolumeRootIdentityAndDirectoryMetadata(t *testing.T) {
	for _, path := range []string{`C:\`, `\\?\C:\`, `\\?\UNC\nas\share\`} {
		api := windowsMetadataFixture()
		api.identityErr = windows.ERROR_NOT_SUPPORTED
		api.filesystem = "FAT32"
		api.path = path
		api.information.FileIndexHigh, api.information.FileIndexLow = 0, 0
		api.information.FileAttributes = windows.FILE_ATTRIBUTE_DIRECTORY
		api.information.FileSizeHigh = ^uint32(0)
		api.basic.ChangeTime = 0
		token, directory, err := inspectWindowsObjectToken(1, &api)
		if err != nil || !directory || token.size != 0 || token.identity[0] != windowsIdentityLegacy {
			t.Fatalf("path=%q token=%+v directory=%v err=%v", path, token, directory, err)
		}
	}
}

func TestWindowsLegacyStableHandleSurvivesRenameWithoutClaimingReopenContinuity(t *testing.T) {
	baseline := windowsTestToken(1, 4)
	baseline.identity[0] = windowsIdentityLegacy
	baseline.changeTime = 0
	renamed := baseline
	renamed.identity[9]++
	handle := &fakeWindowsRevisionFile{tokens: []windowsMutationToken{baseline}, data: []byte("data")}
	binder, err := newWindowsStabilityBinder([]string{t.TempDir()}, &fakeWindowsRevisionPlatform{
		tokens: []windowsMutationToken{baseline, baseline}, root: &fakeWindowsRevisionRoot{file: handle},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer binder.Close()
	preliminary := windowsPreliminaryFile(t)
	defer preliminary.Close()
	record := windowsTestRecord(t, baseline, "file.bin")
	stable, err := binder.BindStable(context.Background(), StableBinding{File: preliminary, Record: record, RelativePath: "file.bin"})
	if err != nil {
		t.Fatal(err)
	}
	defer stable.Close()
	handle.tokens = []windowsMutationToken{renamed}
	buffer := make([]byte, 4)
	if _, err := stable.ReadAt(context.Background(), buffer, 0); err != nil || !bytes.Equal(buffer, []byte("data")) {
		t.Fatalf("retained legacy read=%q err=%v", buffer, err)
	}
	changed := renamed
	changed.lastWrite++
	handle.tokens = []windowsMutationToken{changed}
	if err := stable.Verify(context.Background()); !errors.Is(err, content.ErrSourceDrift) {
		t.Fatalf("changed legacy data=%v", err)
	}
	if baseline.continuity() != content.OpenHandleRevisionContinuity {
		t.Fatal("legacy metadata acquired a reopen guarantee")
	}
}

func TestWindowsRevisionContinuityRejectsMalformedCandidate(t *testing.T) {
	baseline := windowsTestToken(1, 4)
	for _, mutate := range []func([]byte) []byte{
		func(candidate []byte) []byte { return candidate[:len(candidate)-1] },
		func(candidate []byte) []byte { candidate[len(candidate)-1] = 255; return candidate },
		func(candidate []byte) []byte { candidate[0] = windowsIdentityLegacy; return candidate },
		func(candidate []byte) []byte { candidate[2]++; return candidate },
	} {
		identity, _ := catalog.NewSourceIdentity(baseline.sourceIdentityBytes())
		candidate, _ := catalog.NewVersionCandidate(mutate(baseline.candidateBytes()))
		file, _ := catalog.FileIDFromBytes([]byte("1234567890123456"))
		parent, _ := catalog.DirectoryIDFromBytes([]byte("abcdefghijklmnop"))
		locator, _ := catalog.NewLocator(0, "file.bin")
		record, err := catalog.NewFileNodeRecord(file, parent, "file.bin", locator, identity, candidate, 4, catalog.ModifiedTime{})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := (&WindowsStabilityBinder{}).RevisionContinuity(record); !errors.Is(err, content.ErrRevisionStale) {
			t.Fatalf("malformed candidate error=%v", err)
		}
	}
	if modified, err := (windowsMutationToken{}).modifiedTime(); err != nil || modified.Present() {
		t.Fatalf("missing modified time=%v err=%v", modified, err)
	}
}

// The metadata decorator reproduces FAT's missing identity/change-time APIs,
// while every open, share-mode exclusion, read, and close is a real Windows call.
type legacyWindowsNativeMetadata struct{ nativeWindowsRevisionMetadata }

func (legacyWindowsNativeMetadata) FileIdentity(windows.Handle) (windowsRevisionFileIDInfo, error) {
	return windowsRevisionFileIDInfo{}, windows.ERROR_NOT_SUPPORTED
}

func (api legacyWindowsNativeMetadata) BasicInformation(handle windows.Handle) (windowsFileBasicInfo, error) {
	basic, err := api.nativeWindowsRevisionMetadata.BasicInformation(handle)
	basic.ChangeTime = 0
	return basic, err
}

func TestWindowsLegacyCapabilitiesUseRealWriteExclusionAndExposeAmbiguousReopen(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "source.bin")
	if err := os.WriteFile(path, []byte("old!"), 0o600); err != nil {
		t.Fatal(err)
	}
	platform := nativeWindowsRevisionPlatform{metadata: legacyWindowsNativeMetadata{}}
	binder, err := newWindowsStabilityBinder([]string{root}, platform)
	if err != nil {
		t.Fatal(err)
	}
	defer binder.Close()
	preliminary, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer preliminary.Close()
	information, err := preliminary.Stat()
	if err != nil {
		t.Fatal(err)
	}
	baseline, err := platform.Token(preliminary)
	if err != nil {
		t.Fatal(err)
	}
	record := windowsTestRecord(t, baseline, "source.bin")
	stable, err := binder.BindStable(context.Background(), StableBinding{File: preliminary, Record: record, RelativePath: "source.bin"})
	if err != nil {
		t.Fatal(err)
	}
	defer stable.Close()
	if err := os.WriteFile(path, []byte("new!"), 0o600); !errors.Is(err, windows.ERROR_SHARING_VIOLATION) {
		t.Fatalf("legacy stable handle admitted a writer: %v", err)
	}
	buffer := make([]byte, 4)
	if _, err := stable.ReadAt(context.Background(), buffer, 0); err != nil || string(buffer) != "old!" {
		t.Fatalf("locked read=%q err=%v", buffer, err)
	}
	if err := stable.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("new!"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(path, information.ModTime(), information.ModTime()); err != nil {
		t.Fatal(err)
	}
	reopened, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	after, err := platform.Token(reopened)
	if err != nil || !after.matches(record) {
		t.Fatalf("expected ambiguous legacy metadata after same-size rewrite: token=%+v err=%v", after, err)
	}
	continuity, err := binder.RevisionContinuity(record)
	if err != nil || continuity != content.OpenHandleRevisionContinuity {
		t.Fatalf("ambiguous reopen acquired catalog continuity: scope=%v err=%v", continuity, err)
	}
	next, err := binder.BindStable(context.Background(), StableBinding{File: reopened, Record: record, RelativePath: "source.bin"})
	if err != nil {
		t.Fatal(err)
	}
	defer next.Close()
	if _, err := next.ReadAt(context.Background(), buffer, 0); err != nil || string(buffer) != "new!" {
		t.Fatalf("fresh locked read=%q err=%v", buffer, err)
	}
}

func TestRootedRevisionSourceForwardsContinuityAndClosedLifetime(t *testing.T) {
	baseline := windowsTestToken(1, 4)
	baseline.identity[0] = windowsIdentityLegacy
	record := windowsTestRecord(t, baseline, "file.bin")
	source := &RootedRevisionSource{binder: &WindowsStabilityBinder{}}
	if scope, err := source.RevisionContinuity(record); err != nil || scope != content.OpenHandleRevisionContinuity {
		t.Fatalf("forwarded scope=%v err=%v", scope, err)
	}
	if err := source.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := source.RevisionContinuity(record); !errors.Is(err, content.ErrRevisionStoreClosed) {
		t.Fatalf("closed continuity error=%v", err)
	}
	defaultSource := &RootedRevisionSource{binder: StabilityBinderFunc(func(context.Context, StableBinding) (content.StableFile, error) {
		return nil, content.ErrUnsupportedStability
	})}
	if scope, err := defaultSource.RevisionContinuity(record); err != nil || scope != content.CatalogRevisionContinuity {
		t.Fatalf("default binder scope=%v err=%v", scope, err)
	}
}

type windowsTimestampFileInfo struct {
	fs.FileInfo
	data any
}

func (information windowsTimestampFileInfo) Sys() any { return information.data }

func TestWindowsCatalogTimestampMatchesStableHandleWithoutConfusingAbsentAndEpoch(t *testing.T) {
	file := windowsPreliminaryFile(t)
	defer file.Close()
	information, err := file.Stat()
	if err != nil {
		t.Fatal(err)
	}
	for _, ticks := range []int64{0, 1, windowsFiletimeUnixOffset - 1, windowsFiletimeUnixOffset, windowsFiletimeUnixOffset + 1} {
		data := &syscall.Win32FileAttributeData{LastWriteTime: syscall.Filetime{
			HighDateTime: uint32(uint64(ticks) >> 32), LowDateTime: uint32(ticks),
		}}
		modified, err := catalogModifiedTime(windowsTimestampFileInfo{FileInfo: information, data: data})
		stableModified, stableErr := (windowsMutationToken{lastWrite: ticks}).modifiedTime()
		if err != nil || stableErr != nil || modified != stableModified || modified.Present() != (ticks != 0) {
			t.Fatalf("FILETIME=%d catalog=%+v stable=%+v errors=%v/%v", ticks, modified, stableModified, err, stableErr)
		}
		if ticks == windowsFiletimeUnixOffset && (modified.Seconds() != 0 || modified.Nanoseconds() != 0) {
			t.Fatalf("Unix epoch metadata=%+v", modified)
		}
	}
	for _, data := range []any{nil, (*syscall.Win32FileAttributeData)(nil)} {
		modified, err := catalogModifiedTime(windowsTimestampFileInfo{FileInfo: information, data: data})
		portable, portableErr := portableCatalogModifiedTime(information)
		if err != nil || portableErr != nil || modified != portable {
			t.Fatalf("portable FileInfo catalog=%+v expected=%+v errors=%v/%v", modified, portable, err, portableErr)
		}
	}
}

func TestWindowsStableTokenUsesCachedFilesystemProfile(t *testing.T) {
	for _, legacy := range []bool{false, true} {
		api := windowsMetadataFixture()
		if legacy {
			api.identityErr = windows.ERROR_NOT_SUPPORTED
		}
		file := windowsPreliminaryFile(t)
		defer file.Close()
		metadata := windowsRootRevisionMetadata{windowsRevisionMetadata: &api, filesystem: "NTFS"}
		stable := nativeWindowsRevisionFile{file: file, metadata: metadata, profile: windowsRevisionProfileLocalNTFS}
		for range 3 {
			if _, err := stable.Token(); err != nil {
				t.Fatal(err)
			}
		}
		if api.volumeCalls != 0 {
			t.Fatal("block verification repeated filesystem profile probes")
		}
	}
}

func TestWindowsTransientProfileObservationsDoNotChangeContentEvidence(t *testing.T) {
	tests := []struct {
		name           string
		catalogProfile windowsRevisionProfile
		rootProfile    windowsRevisionProfile
		beforeProfile  windowsRevisionProfile
		afterProfile   windowsRevisionProfile
		want           content.RevisionContinuity
	}{
		{"catalog probe recovers", windowsRevisionProfileUnknown, windowsRevisionProfileLocalNTFS, windowsRevisionProfileLocalNTFS, windowsRevisionProfileLocalNTFS, content.OpenHandleRevisionContinuity},
		{"root probe fails", windowsRevisionProfileLocalNTFS, windowsRevisionProfileUnknown, windowsRevisionProfileLocalNTFS, windowsRevisionProfileUnknown, content.OpenHandleRevisionContinuity},
		{"preliminary probe recovers", windowsRevisionProfileLocalNTFS, windowsRevisionProfileLocalNTFS, windowsRevisionProfileUnknown, windowsRevisionProfileLocalNTFS, content.CatalogRevisionContinuity},
		{"root profile differs", windowsRevisionProfileLocalNTFS, windowsRevisionProfileLocalReFS, windowsRevisionProfileLocalReFS, windowsRevisionProfileLocalReFS, content.OpenHandleRevisionContinuity},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			catalogToken := windowsTestToken(1, 4)
			catalogToken.profile = test.catalogProfile
			before, after, opened := catalogToken, catalogToken, catalogToken
			before.profile, after.profile, opened.profile = test.beforeProfile, test.afterProfile, test.rootProfile
			handle := &fakeWindowsRevisionFile{tokens: []windowsMutationToken{opened}, data: []byte("data")}
			root := &fakeWindowsRevisionRoot{file: handle, profile: test.rootProfile}
			binder, err := newWindowsStabilityBinder([]string{t.TempDir()}, &fakeWindowsRevisionPlatform{
				root: root, tokens: []windowsMutationToken{before, after},
			})
			if err != nil {
				t.Fatal(err)
			}
			defer binder.Close()
			record := windowsTestRecord(t, catalogToken, "file.bin")
			if scope, err := binder.RevisionContinuity(record); err != nil || scope != test.want {
				t.Fatalf("scope before native open=%v want=%v err=%v", scope, test.want, err)
			}
			preliminary := windowsPreliminaryFile(t)
			defer preliminary.Close()
			stable, err := binder.BindStable(context.Background(), StableBinding{File: preliminary, Record: record, RelativePath: "file.bin"})
			if err != nil {
				t.Fatalf("profile-only change rejected unchanged bytes: %v", err)
			}
			defer stable.Close()
			buffer := make([]byte, 4)
			if _, err := stable.ReadAt(context.Background(), buffer, 0); err != nil || string(buffer) != "data" {
				t.Fatalf("read=%q err=%v", buffer, err)
			}
			changed := after
			changed.changeTime++
			if before.sameCatalogEvidence(changed) || changed.matches(record) {
				t.Fatal("profile-independent comparison ignored a real content candidate change")
			}
		})
	}
}
