//go:build windows

package osfs

import (
	"context"
	"io/fs"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/windshare/windshare/core/catalog"
	diag "github.com/windshare/windshare/core/diagnostics"
	"golang.org/x/sys/windows"
)

const (
	testReparseTagWOF         uint32 = 0x80000017
	testReparseTagAppExecLink uint32 = 0x8000001B
	testReparseTagUnknown     uint32 = 0x80001234
	testReparseTagCloudPinned uint32 = 0x9000601A
	testReparseTagCloudRoot   uint32 = 0x9000701A
	testAttrPinned            uint32 = 0x00080000
	testAttrEA                uint32 = 0x00040000
)

func TestWindowsCloudReparseTagWhitelistRejectsSurrogatesAndUnknownTags(t *testing.T) {
	for nibble := range uint32(16) {
		tag := ioReparseTagCloud | (nibble << 12)
		if !isCloudReparseTag(tag) {
			t.Fatalf("expected cloud tag %#08x to be whitelisted", tag)
		}
		if isCloudReparseTag(tag | reparseTagNameSurrogateBit) {
			t.Fatalf("surrogate-marked tag %#08x was whitelisted", tag|reparseTagNameSurrogateBit)
		}
	}
	for _, rejected := range []uint32{
		0,
		windows.IO_REPARSE_TAG_SYMLINK,
		windows.IO_REPARSE_TAG_MOUNT_POINT,
		testReparseTagWOF,
		testReparseTagAppExecLink,
		testReparseTagUnknown,
	} {
		if isCloudReparseTag(rejected) {
			t.Fatalf("non-cloud tag %#08x was whitelisted", rejected)
		}
	}
}

func TestClassifyWindowsEntryCoversCloudStatesRecallSourcesAndFailClosedTags(t *testing.T) {
	hydratedState := cfPlaceholderStatePlaceholder | cfPlaceholderStateInSync
	syncRootState := hydratedState | cfPlaceholderStateSyncRoot
	partialState := hydratedState | cfPlaceholderStatePartial
	partiallyOnDiskState := hydratedState | cfPlaceholderStatePartiallyOnDisk

	cases := []struct {
		name         string
		facts        windowsEntryFacts
		wantKind     sourceEntryKind
		wantHydrated bool
	}{
		{
			name: "ordinary regular file",
			facts: windowsEntryFacts{
				source: windowsAttributesFromHandle, hasAttributes: true,
				attributes: windows.FILE_ATTRIBUTE_ARCHIVE,
			},
			wantKind: sourceEntryRegularFile,
		},
		{
			name: "ordinary file with FILE_ATTRIBUTE_EA on handle query is allowed",
			facts: windowsEntryFacts{
				source: windowsAttributesFromHandle, hasAttributes: true,
				attributes: windows.FILE_ATTRIBUTE_ARCHIVE | testAttrEA,
			},
			wantKind: sourceEntryRegularFile,
		},
		{
			name: "directory enum RECALL_ON_OPEN without reparse tag is cloud placeholder",
			facts: windowsEntryFacts{
				source: windowsAttributesFromDirectoryEnum, hasAttributes: true,
				attributes: windows.FILE_ATTRIBUTE_ARCHIVE | windowsAttrRecallOnOpen,
			},
			wantKind: sourceEntryCloudPlaceholder,
		},
		{
			name: "disguise mode cloud-only file with RECALL_ON_DATA_ACCESS",
			facts: windowsEntryFacts{
				source: windowsAttributesFromHandle, hasAttributes: true,
				attributes: windows.FILE_ATTRIBUTE_ARCHIVE | windowsAttrRecallOnDataAccess,
			},
			wantKind: sourceEntryCloudPlaceholder,
		},
		{
			name: "hydrated cached cloud file",
			facts: windowsEntryFacts{
				source: windowsAttributesFromHandle, mode: fs.ModeIrregular | 0o666,
				hasAttributes: true, attributes: windows.FILE_ATTRIBUTE_ARCHIVE | windows.FILE_ATTRIBUTE_REPARSE_POINT,
				hasReparseTag: true, reparseTag: testReparseTagCloudPinned,
				placeholderState: hydratedState, hasFileType: true, fileType: windows.FILE_TYPE_DISK,
			},
			wantKind: sourceEntryRegularFile, wantHydrated: true,
		},
		{
			name: "hydrated pinned cloud file",
			facts: windowsEntryFacts{
				source: windowsAttributesFromHandle, mode: fs.ModeIrregular | 0o666,
				hasAttributes: true, attributes: windows.FILE_ATTRIBUTE_ARCHIVE | windows.FILE_ATTRIBUTE_REPARSE_POINT | testAttrPinned,
				hasReparseTag: true, reparseTag: testReparseTagCloudPinned,
				placeholderState: hydratedState, hasFileType: true, fileType: windows.FILE_TYPE_DISK,
			},
			wantKind: sourceEntryRegularFile, wantHydrated: true,
		},
		{
			name: "hydrated cloud file with FILE_ATTRIBUTE_EA on handle query is allowed",
			facts: windowsEntryFacts{
				source: windowsAttributesFromHandle, mode: fs.ModeIrregular | 0o666,
				hasAttributes: true, attributes: windows.FILE_ATTRIBUTE_ARCHIVE | windows.FILE_ATTRIBUTE_REPARSE_POINT | testAttrEA,
				hasReparseTag: true, reparseTag: testReparseTagCloudPinned,
				placeholderState: hydratedState, hasFileType: true, fileType: windows.FILE_TYPE_DISK,
			},
			wantKind: sourceEntryRegularFile, wantHydrated: true,
		},
		{
			name: "hydrated cloud file with RECALL_ON_OPEN in dir enum is cloud placeholder",
			facts: windowsEntryFacts{
				source: windowsAttributesFromDirectoryEnum, mode: fs.ModeIrregular | 0o666,
				hasAttributes: true, attributes: windows.FILE_ATTRIBUTE_ARCHIVE | windows.FILE_ATTRIBUTE_REPARSE_POINT | windowsAttrRecallOnOpen,
				hasReparseTag: true, reparseTag: testReparseTagCloudPinned,
				placeholderState: hydratedState, hasFileType: true, fileType: windows.FILE_TYPE_DISK,
			},
			wantKind: sourceEntryCloudPlaceholder,
		},
		{
			name: "cloud sync root directory",
			facts: windowsEntryFacts{
				source: windowsAttributesFromHandle, mode: fs.ModeDir | fs.ModeIrregular | 0o777,
				hasAttributes: true, attributes: windows.FILE_ATTRIBUTE_DIRECTORY | windows.FILE_ATTRIBUTE_REPARSE_POINT,
				hasReparseTag: true, reparseTag: testReparseTagCloudRoot,
				placeholderState: syncRootState, hasFileType: true, fileType: windows.FILE_TYPE_DISK,
			},
			wantKind: sourceEntryDirectory, wantHydrated: true,
		},
		{
			name: "cloud partial placeholder",
			facts: windowsEntryFacts{
				source: windowsAttributesFromHandle, mode: fs.ModeIrregular | 0o666,
				hasAttributes: true, attributes: windows.FILE_ATTRIBUTE_ARCHIVE | windows.FILE_ATTRIBUTE_REPARSE_POINT,
				hasReparseTag: true, reparseTag: 0x9000401A,
				placeholderState: partialState, hasFileType: true, fileType: windows.FILE_TYPE_DISK,
			},
			wantKind: sourceEntryCloudPlaceholder,
		},
		{
			name: "cloud partially on disk placeholder",
			facts: windowsEntryFacts{
				source: windowsAttributesFromHandle, mode: fs.ModeIrregular | 0o666,
				hasAttributes: true, attributes: windows.FILE_ATTRIBUTE_ARCHIVE | windows.FILE_ATTRIBUTE_REPARSE_POINT,
				hasReparseTag: true, reparseTag: 0x9000401A,
				placeholderState: partiallyOnDiskState, hasFileType: true, fileType: windows.FILE_TYPE_DISK,
			},
			wantKind: sourceEntryCloudPlaceholder,
		},
		{
			name: "cloud invalid placeholder state fails closed",
			facts: windowsEntryFacts{
				source: windowsAttributesFromHandle, mode: fs.ModeIrregular | 0o666,
				hasAttributes: true, attributes: windows.FILE_ATTRIBUTE_ARCHIVE | windows.FILE_ATTRIBUTE_REPARSE_POINT,
				hasReparseTag: true, reparseTag: testReparseTagCloudPinned,
				placeholderState: cfPlaceholderStateInvalid, hasFileType: true, fileType: windows.FILE_TYPE_DISK,
			},
			wantKind: sourceEntryReparsePoint,
		},
		{
			name: "cloud unrecognized placeholder state bit fails closed",
			facts: windowsEntryFacts{
				source: windowsAttributesFromHandle, mode: fs.ModeIrregular | 0o666,
				hasAttributes: true, attributes: windows.FILE_ATTRIBUTE_ARCHIVE | windows.FILE_ATTRIBUTE_REPARSE_POINT,
				hasReparseTag: true, reparseTag: testReparseTagCloudPinned,
				placeholderState: hydratedState | 0x00000040, hasFileType: true, fileType: windows.FILE_TYPE_DISK,
			},
			wantKind: sourceEntryReparsePoint,
		},
		{
			name: "symlink reparse tag rejected",
			facts: windowsEntryFacts{
				source: windowsAttributesFromHandle, hasAttributes: true,
				attributes:    windows.FILE_ATTRIBUTE_REPARSE_POINT,
				hasReparseTag: true, reparseTag: windows.IO_REPARSE_TAG_SYMLINK,
			},
			wantKind: sourceEntrySymbolicLink,
		},
		{
			name: "mount point junction rejected",
			facts: windowsEntryFacts{
				source: windowsAttributesFromHandle, hasAttributes: true,
				attributes:    windows.FILE_ATTRIBUTE_DIRECTORY | windows.FILE_ATTRIBUTE_REPARSE_POINT,
				hasReparseTag: true, reparseTag: windows.IO_REPARSE_TAG_MOUNT_POINT,
			},
			wantKind: sourceEntryReparsePoint,
		},
		{
			name: "WOF reparse point rejected",
			facts: windowsEntryFacts{
				source: windowsAttributesFromHandle, hasAttributes: true,
				attributes:    windows.FILE_ATTRIBUTE_ARCHIVE | windows.FILE_ATTRIBUTE_REPARSE_POINT,
				hasReparseTag: true, reparseTag: testReparseTagWOF,
			},
			wantKind: sourceEntryReparsePoint,
		},
		{
			name: "unknown custom reparse tag rejected",
			facts: windowsEntryFacts{
				source: windowsAttributesFromHandle, hasAttributes: true,
				attributes:    windows.FILE_ATTRIBUTE_ARCHIVE | windows.FILE_ATTRIBUTE_REPARSE_POINT,
				hasReparseTag: true, reparseTag: testReparseTagUnknown,
			},
			wantKind: sourceEntryReparsePoint,
		},
		{
			name: "hydrated cloud non-disk file type rejected as special file",
			facts: windowsEntryFacts{
				source: windowsAttributesFromHandle, mode: fs.ModeIrregular | 0o666,
				hasAttributes: true, attributes: windows.FILE_ATTRIBUTE_REPARSE_POINT,
				hasReparseTag: true, reparseTag: testReparseTagCloudPinned,
				placeholderState: hydratedState, hasFileType: true, fileType: windows.FILE_TYPE_PIPE,
			},
			wantKind: sourceEntrySpecialFile,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			gotKind, gotAudit := classifyWindowsEntry(tc.facts)
			if gotKind != tc.wantKind || gotAudit.hydratedCloud != tc.wantHydrated {
				t.Fatalf("classifyWindowsEntry() = (kind=%d, hydrated=%v), want (kind=%d, hydrated=%v)",
					gotKind, gotAudit.hydratedCloud, tc.wantKind, tc.wantHydrated)
			}
		})
	}
}

func TestCanOpenRootRelativeRegularFilePreflight(t *testing.T) {
	if !canOpenRootRelativeRegularFile(windowsPathFactFileInfo{mode: 0o666, attributes: windows.FILE_ATTRIBUTE_ARCHIVE}) {
		t.Fatal("ordinary regular file rejected")
	}
	if !canOpenRootRelativeRegularFile(windowsPathFactFileInfo{mode: fs.ModeIrregular | 0o666, attributes: windows.FILE_ATTRIBUTE_REPARSE_POINT}) {
		t.Fatal("hydrated cloud candidate rejected at preflight")
	}
	for _, rejected := range []windowsPathFactFileInfo{
		{mode: fs.ModeSymlink},
		{mode: fs.ModeDir | fs.ModeIrregular, attributes: windows.FILE_ATTRIBUTE_DIRECTORY | windows.FILE_ATTRIBUTE_REPARSE_POINT},
		{mode: fs.ModeIrregular | 0o666, attributes: windows.FILE_ATTRIBUTE_REPARSE_POINT | windowsAttrRecallOnDataAccess},
		{mode: 0o666, attributes: windowsAttrOffline},
		{mode: fs.ModeNamedPipe},
	} {
		if canOpenRootRelativeRegularFile(rejected) {
			t.Fatalf("expected %+v to fail preflight", rejected)
		}
	}
}

func TestSelectedCatalogSourceOmitsChildDehydratedBeforeOpenAndCommitsSiblings(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "a-dehydrating.txt"), []byte("cloud"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "b-readable.txt"), []byte("local"), 0o600); err != nil {
		t.Fatal(err)
	}
	stream, err := diag.NewStream(32, nil)
	if err != nil {
		t.Fatal(err)
	}
	var observed []catalog.SourceOmissions
	synthetic, _ := catalog.DirectoryIDFromBytes(identity16(88))
	source, err := NewSelectedCatalogSource(SelectedCatalogSourceConfig{
		Paths:         []string{root},
		SyntheticRoot: synthetic,
		Diagnostics:   stream.Sink(),
		Omissions: catalog.SourceOmissionObserverFunc(func(summary catalog.SourceOmissions) {
			observed = append(observed, summary)
		}),
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = source.Close() })

	// Simulate the child dehydrating between preflight and handle inspection.
	source.inspectChildHandleFn = func(handle *os.File, info fs.FileInfo) (sourceEntryKind, sourceEntryAudit, error) {
		if info.Name() == "a-dehydrating.txt" {
			return sourceEntryCloudPlaceholder, sourceEntryAudit{
				attributes:       windows.FILE_ATTRIBUTE_REPARSE_POINT | windowsAttrRecallOnDataAccess,
				reparseTag:       0x9000401A,
				placeholderState: cfPlaceholderStatePlaceholder | cfPlaceholderStatePartial,
				hasCloudState:    true,
			}, nil
		}
		return inspectHandleKind(handle, info)
	}

	children := &collectingScanChildren{}
	result, err := source.ScanDirectory(context.Background(), catalog.ScanRequest{
		Directory: source.SelectedRoots()[0],
		Work:      &countingScanWork{},
		Children:  children,
	})
	if err != nil {
		t.Fatalf("ScanDirectory failed on single-child dehydration: %v", err)
	}
	result.PublishOmissions(0, nil)
	stream.Close()

	if result.OmittedCount != 1 || len(children.items) != 1 || children.items[0].Name != "b-readable.txt" {
		t.Fatalf("scan=%+v children=%+v", result, children.items)
	}
	if len(observed) != 1 || observed[0].Count != 1 || len(observed[0].Samples) != 1 ||
		observed[0].Samples[0].Name != "a-dehydrating.txt" ||
		observed[0].Samples[0].Reason != catalog.SourceOmissionCloudPlaceholder {
		t.Fatalf("observed omissions=%+v", observed)
	}
	foundTrace := false
	for record := range stream.Records() {
		if record.Name() == "catalog_source_omission" {
			p := record.Payload()
			if p.Get("reason").Text() == string(catalog.SourceOmissionCloudPlaceholder) &&
				p.Get("reparse_tag").Text() == "0x9000401a" &&
				p.Get("decision").Text() == "cloud_placeholder" {
				foundTrace = true
			}
		}
	}
	if !foundTrace {
		t.Fatal("missing structured cloud_placeholder omission diagnostic trace")
	}
}

type windowsPathFactFileInfo struct {
	mode       fs.FileMode
	attributes uint32
}

func (information windowsPathFactFileInfo) Name() string       { return "entry" }
func (information windowsPathFactFileInfo) Size() int64        { return 0 }
func (information windowsPathFactFileInfo) Mode() fs.FileMode  { return information.mode }
func (information windowsPathFactFileInfo) ModTime() time.Time { return time.Time{} }
func (information windowsPathFactFileInfo) IsDir() bool        { return information.mode.IsDir() }
func (information windowsPathFactFileInfo) Sys() any {
	return &syscall.Win32FileAttributeData{FileAttributes: information.attributes}
}
