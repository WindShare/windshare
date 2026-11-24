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

	"github.com/windshare/windshare/core/content"
	"golang.org/x/sys/windows"
)

type fakeWindowsRevisionMetadata struct {
	identity             windowsRevisionFileIDInfo
	identityErr          error
	information          windows.ByHandleFileInformation
	informationErr       error
	basic                windowsFileBasicInfo
	basicErr             error
	attributeTag         windowsAttributeTagInfo
	attributeTagErr      error
	fileType             uint32
	fileTypeErr          error
	placeholderState     uint32
	lastPlaceholderAttrs uint32
	filesystem           string
	filesystemErr        error
	path                 string
	pathErr              error
	volumeCalls          int
	attrTagCalls         int
	fileTypeCalls        int
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
func (api *fakeWindowsRevisionMetadata) AttributeTag(windows.Handle) (windowsAttributeTagInfo, error) {
	api.attrTagCalls++
	if api.attributeTagErr != nil {
		return windowsAttributeTagInfo{}, api.attributeTagErr
	}
	info := api.attributeTag
	if info.FileAttributes == 0 {
		info.FileAttributes = api.information.FileAttributes
	}
	return info, nil
}
func (api *fakeWindowsRevisionMetadata) FileType(windows.Handle) (uint32, error) {
	api.fileTypeCalls++
	if api.fileTypeErr != nil {
		return 0, api.fileTypeErr
	}
	if api.fileType == 0 {
		return windows.FILE_TYPE_DISK, nil
	}
	return api.fileType, nil
}
func (api *fakeWindowsRevisionMetadata) PlaceholderState(attributes uint32, _ uint32) uint32 {
	api.lastPlaceholderAttrs = attributes
	return api.placeholderState
}
func (api *fakeWindowsRevisionMetadata) Filesystem(windows.Handle) (string, error) {
	api.volumeCalls++
	return api.filesystem, api.filesystemErr
}
func (api *fakeWindowsRevisionMetadata) FinalPath(windows.Handle) (string, error) {
	return api.path, api.pathErr
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
		filesystem: "NTFS", path: `C:\file.bin`,
	}
}

func TestWindowsRevisionMetadataUsesCapabilities(t *testing.T) {
	tests := []struct {
		name      string
		configure func(*fakeWindowsRevisionMetadata)
		wantTag   byte
	}{
		{"full-width NTFS", func(*fakeWindowsRevisionMetadata) {}, windowsIdentityFullWidth},
		{"full-width ReFS", func(api *fakeWindowsRevisionMetadata) { api.filesystem = "ReFS" }, windowsIdentityFullWidth},
		{"full-width FAT with change time", func(api *fakeWindowsRevisionMetadata) { api.filesystem = "FAT32" }, windowsIdentityFullWidth},
		{"full-width UNC", func(api *fakeWindowsRevisionMetadata) {
			api.path = `\\?\UNC\nas\share\file`
			api.filesystem = "remote"
			api.filesystemErr = windows.ERROR_NOT_SUPPORTED
		}, windowsIdentityFullWidth},
		{"FAT32", func(api *fakeWindowsRevisionMetadata) {
			api.identityErr = windows.ERROR_INVALID_PARAMETER
			api.filesystem = "FAT32"
			api.basic.ChangeTime = 0
		}, windowsIdentityLegacy},
		{"exFAT", func(api *fakeWindowsRevisionMetadata) {
			api.identityErr = windows.ERROR_NOT_SUPPORTED
			api.filesystem = "exFAT"
			api.basic.ChangeTime = 0
		}, windowsIdentityLegacy},
		{"legacy NAS", func(api *fakeWindowsRevisionMetadata) {
			api.identityErr = windows.ERROR_INVALID_FUNCTION
			api.filesystem = "NAS"
			api.path = `\\nas\share\file`
		}, windowsIdentityLegacy},
		{"no extended basic metadata", func(api *fakeWindowsRevisionMetadata) { api.basicErr = windows.ERROR_CALL_NOT_IMPLEMENTED }, windowsIdentityFullWidth},
		{"zero change time", func(api *fakeWindowsRevisionMetadata) { api.basic.ChangeTime = 0 }, windowsIdentityFullWidth},
		{"negative change time", func(api *fakeWindowsRevisionMetadata) { api.basic.ChangeTime = -1 }, windowsIdentityFullWidth},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			api := windowsMetadataFixture()
			test.configure(&api)
			token, directory, err := inspectWindowsObjectMetadata(1, &api)
			if err != nil || directory || token.size != 4 || token.identity[0] != test.wantTag {
				t.Fatalf("token=%+v directory=%v err=%v", token, directory, err)
			}
			if api.basicErr != nil && token.lastWrite != int64(uint64(api.information.LastWriteTime.HighDateTime)<<32|uint64(api.information.LastWriteTime.LowDateTime)) {
				t.Fatal("fallback lost observed last-write metadata")
			}
			if test.wantTag == windowsIdentityLegacy && binary.BigEndian.Uint64(token.identity[9:17]) != uint64(api.information.FileIndexHigh)<<32|uint64(api.information.FileIndexLow) {
				t.Fatal("legacy handle identity was truncated")
			}
			record := windowsTestRecord(t, token, "file.bin")
			if !token.matches(record) {
				t.Fatalf("token did not match its own catalog record: token=%+v", token)
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
			_, _, err := inspectWindowsObjectMetadata(1, &api)
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
		token, directory, err := inspectWindowsObjectMetadata(1, &api)
		if err != nil || !directory || token.size != 0 || token.identity[0] != windowsIdentityLegacy {
			t.Fatalf("path=%q token=%+v directory=%v err=%v", path, token, directory, err)
		}
	}
}

func TestWindowsLegacyStableHandleSurvivesRename(t *testing.T) {
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

func TestWindowsLegacyCapabilitiesUseRealWriteExclusion(t *testing.T) {
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
		t.Fatalf("expected legacy metadata match after same-size rewrite: token=%+v err=%v", after, err)
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

func TestWindowsStableTokenUsesCachedFilesystemMetadata(t *testing.T) {
	for _, legacy := range []bool{false, true} {
		api := windowsMetadataFixture()
		if legacy {
			api.identityErr = windows.ERROR_NOT_SUPPORTED
		}
		file := windowsPreliminaryFile(t)
		defer file.Close()
		metadata := windowsRootRevisionMetadata{windowsRevisionMetadata: &api, filesystem: "NTFS"}
		stable := nativeWindowsRevisionFile{file: file, metadata: metadata}
		for range 3 {
			if _, err := stable.Token(); err != nil {
				t.Fatal(err)
			}
		}
		if api.volumeCalls != 0 {
			t.Fatal("block verification repeated filesystem metadata probes")
		}
	}
}

func TestWindowsOrdinaryFileReadDoesNotAddAttributeTagOrFileTypeQueries(t *testing.T) {
	api := windowsMetadataFixture()
	file := windowsPreliminaryFile(t)
	defer file.Close()
	metadata := windowsRootRevisionMetadata{windowsRevisionMetadata: &api, filesystem: "NTFS"}
	native := &nativeWindowsRevisionFile{file: file, metadata: metadata}
	baseline, err := native.Token()
	if err != nil {
		t.Fatal(err)
	}
	stable := &windowsStableFile{handle: native, baseline: baseline}
	if err := stable.Verify(context.Background()); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 4)
	if _, err := stable.ReadAt(context.Background(), buf, 0); err != nil {
		t.Fatal(err)
	}
	if api.attrTagCalls != 0 || api.fileTypeCalls != 0 {
		t.Fatalf("ordinary file incurred extra queries: attrTag=%d fileType=%d", api.attrTagCalls, api.fileTypeCalls)
	}
}

func TestWindowsCloudMetadataAdmitsHydratedAndDetectsMidShareDehydrationDrift(t *testing.T) {
	hydratedState := cfPlaceholderStatePlaceholder | cfPlaceholderStateInSync
	api := windowsMetadataFixture()
	api.information.FileAttributes = windows.FILE_ATTRIBUTE_ARCHIVE | windows.FILE_ATTRIBUTE_REPARSE_POINT
	api.attributeTag = windowsAttributeTagInfo{
		FileAttributes: api.information.FileAttributes,
		ReparseTag:     testReparseTagCloudPinned,
	}
	api.placeholderState = hydratedState
	api.fileType = windows.FILE_TYPE_DISK

	file := windowsPreliminaryFile(t)
	defer file.Close()
	metadata := windowsRootRevisionMetadata{windowsRevisionMetadata: &api, filesystem: "NTFS"}
	native := &nativeWindowsRevisionFile{file: file, metadata: metadata}
	baseline, err := native.Token()
	if err != nil {
		t.Fatalf("hydrated cloud file rejected: %v", err)
	}
	stable := &windowsStableFile{handle: native, baseline: baseline}
	if err := stable.Verify(context.Background()); err != nil {
		t.Fatalf("hydrated cloud verify failed: %v", err)
	}
	buf := make([]byte, 4)
	if _, err := stable.ReadAt(context.Background(), buf, 0); err != nil {
		t.Fatalf("hydrated cloud read failed: %v", err)
	}
	// FileType must be checked once when first binding the cloud handle and reused afterwards.
	if api.fileTypeCalls != 1 {
		t.Fatalf("hydrated cloud fileTypeCalls = %d, want 1", api.fileTypeCalls)
	}

	// Simulate mid-share dehydration via RECALL_ON_DATA_ACCESS and OFFLINE.
	for _, mutatedAttr := range []uint32{windowsAttrRecallOnDataAccess, windowsAttrOffline} {
		api.information.FileAttributes = windows.FILE_ATTRIBUTE_ARCHIVE | windows.FILE_ATTRIBUTE_REPARSE_POINT | mutatedAttr
		api.attributeTag.FileAttributes = api.information.FileAttributes
		if err := stable.Verify(context.Background()); !errors.Is(err, content.ErrSourceDrift) {
			t.Fatalf("mutated attr %#x Verify() = %v, want ErrSourceDrift", mutatedAttr, err)
		}
		if _, err := stable.ReadAt(context.Background(), buf, 0); !errors.Is(err, content.ErrSourceDrift) {
			t.Fatalf("mutated attr %#x ReadAt() = %v, want ErrSourceDrift", mutatedAttr, err)
		}
	}

	// Simulate non-disk file type or unsupported reparse tag failing closed on bind/reopen.
	for _, tc := range []struct {
		name      string
		configure func(*fakeWindowsRevisionMetadata)
		want      error
	}{
		{
			name: "non-disk hydrated cloud handle",
			configure: func(m *fakeWindowsRevisionMetadata) {
				m.fileType = windows.FILE_TYPE_PIPE
			},
			want: content.ErrRevisionStale,
		},
		{
			name: "WOF reparse tag",
			configure: func(m *fakeWindowsRevisionMetadata) {
				m.attributeTag.ReparseTag = testReparseTagWOF
			},
			want: content.ErrRevisionStale,
		},
		{
			name: "mount point tag",
			configure: func(m *fakeWindowsRevisionMetadata) {
				m.attributeTag.ReparseTag = windows.IO_REPARSE_TAG_MOUNT_POINT
			},
			want: content.ErrRevisionStale,
		},
		{
			name: "invalid placeholder state",
			configure: func(m *fakeWindowsRevisionMetadata) {
				m.placeholderState = cfPlaceholderStateInvalid
			},
			want: content.ErrRevisionStale,
		},
		{
			name: "partial placeholder on reopen",
			configure: func(m *fakeWindowsRevisionMetadata) {
				m.placeholderState = hydratedState | cfPlaceholderStatePartial
			},
			want: content.ErrRevisionStale,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := windowsMetadataFixture()
			m.information.FileAttributes = windows.FILE_ATTRIBUTE_ARCHIVE | windows.FILE_ATTRIBUTE_REPARSE_POINT
			m.attributeTag = windowsAttributeTagInfo{FileAttributes: m.information.FileAttributes, ReparseTag: testReparseTagCloudPinned}
			m.placeholderState = hydratedState
			m.fileType = windows.FILE_TYPE_DISK
			tc.configure(&m)
			if _, err := inspectWindowsFileToken(1, &m); !errors.Is(err, tc.want) {
				t.Fatalf("inspectWindowsFileToken() = %v, want %v", err, tc.want)
			}
		})
	}
}

func TestVerifyWindowsHandleMetadataMasksEAAndGuardsNilFiles(t *testing.T) {
	api := windowsMetadataFixture()
	api.information.FileAttributes = windows.FILE_ATTRIBUTE_ARCHIVE | windows.FILE_ATTRIBUTE_REPARSE_POINT | testAttrEA
	api.attributeTag = windowsAttributeTagInfo{
		FileAttributes: api.information.FileAttributes,
		ReparseTag:     testReparseTagCloudPinned,
	}
	api.placeholderState = cfPlaceholderStatePlaceholder | cfPlaceholderStateInSync
	api.fileType = windows.FILE_TYPE_DISK

	if _, err := inspectWindowsFileToken(1, &api); err != nil {
		t.Fatalf("inspectWindowsFileToken with EA = %v", err)
	}
	if api.lastPlaceholderAttrs&windowsAttrEA != 0 {
		t.Fatalf("PlaceholderState received unmasked EA bit: %#08x", api.lastPlaceholderAttrs)
	}
	if _, err := (nativeWindowsRevisionPlatform{}).Token(nil); !errors.Is(err, content.ErrUnsupportedStability) {
		t.Fatalf("Token(nil) = %v, want ErrUnsupportedStability", err)
	}
	if _, err := (&nativeWindowsRevisionFile{}).Token(); !errors.Is(err, content.ErrUnsupportedStability) {
		t.Fatalf("nil file Token() = %v, want ErrUnsupportedStability", err)
	}
}
