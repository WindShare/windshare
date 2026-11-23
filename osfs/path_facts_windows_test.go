//go:build windows

package osfs

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/windshare/windshare/core/catalog"
	"github.com/windshare/windshare/core/content"
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

func TestQueryWindowsHandlePlaceholderStateMasksExtendedAttributes(t *testing.T) {
	if procCfGetPlaceholderStateFromAttributeTag.Find() != nil {
		t.Skip("cldapi.dll CfGetPlaceholderStateFromAttributeTag unavailable on this host")
	}
	fileAttrs := uint32(windows.FILE_ATTRIBUTE_ARCHIVE | windows.FILE_ATTRIBUTE_REPARSE_POINT | testAttrEA)
	fileState := queryWindowsHandlePlaceholderState(fileAttrs, testReparseTagCloudPinned)
	if fileState == cfPlaceholderStateInvalid || fileState&cfPlaceholderStatePlaceholder == 0 ||
		fileState&(cfPlaceholderStatePartial|cfPlaceholderStatePartiallyOnDisk) != 0 {
		t.Fatalf("handle placeholder state with EA on hydrated file = %#08x", fileState)
	}
	kind, audit := classifyWindowsEntry(windowsEntryFacts{
		source: windowsAttributesFromHandle, hasAttributes: true, attributes: fileAttrs,
		hasReparseTag: true, reparseTag: testReparseTagCloudPinned,
		placeholderState: fileState, hasFileType: true, fileType: windows.FILE_TYPE_DISK,
	})
	if kind != sourceEntryRegularFile || !audit.hydratedCloud {
		t.Fatalf("hydrated cloud file with EA = (kind=%d, hydrated=%v)", kind, audit.hydratedCloud)
	}

	dirAttrs := uint32(windows.FILE_ATTRIBUTE_DIRECTORY | windows.FILE_ATTRIBUTE_REPARSE_POINT | testAttrEA)
	dirState := queryWindowsHandlePlaceholderState(dirAttrs, testReparseTagCloudRoot)
	if dirState == cfPlaceholderStateInvalid || dirState&cfPlaceholderStatePlaceholder == 0 ||
		dirState&(cfPlaceholderStatePartial|cfPlaceholderStatePartiallyOnDisk) != 0 {
		t.Fatalf("handle placeholder state with EA on hydrated dir = %#08x", dirState)
	}
	dirKind, dirAudit := classifyWindowsEntry(windowsEntryFacts{
		source: windowsAttributesFromHandle, hasAttributes: true, attributes: dirAttrs,
		hasReparseTag: true, reparseTag: testReparseTagCloudRoot,
		placeholderState: dirState, hasFileType: true, fileType: windows.FILE_TYPE_DISK,
	})
	if dirKind != sourceEntryDirectory || !dirAudit.hydratedCloud {
		t.Fatalf("hydrated cloud dir with EA = (kind=%d, hydrated=%v)", dirKind, dirAudit.hydratedCloud)
	}

	recallAttrs := uint32(windows.FILE_ATTRIBUTE_ARCHIVE | windows.FILE_ATTRIBUTE_REPARSE_POINT | windowsAttrRecallOnDataAccess)
	recallState := queryWindowsHandlePlaceholderState(recallAttrs, testReparseTagCloudPinned)
	if recallState&cfPlaceholderStatePartial == 0 {
		t.Fatalf("RECALL_ON_DATA_ACCESS state = %#08x, want PARTIAL", recallState)
	}
}

func TestWindowsExtendedPathHandlesDriveUNCAndExistingPrefixes(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"", ""},
		{`C:\dir\file.txt`, `\\?\C:\dir\file.txt`},
		{`C:\`, `\\?\C:\`},
		{`\\server\share\dir\file.txt`, `\\?\UNC\server\share\dir\file.txt`},
		{`//server/share/dir/file.txt`, `\\?\UNC\server\share\dir\file.txt`},
		{`\\?\C:\already\extended`, `\\?\C:\already\extended`},
		{`\\?\UNC\server\share\file`, `\\?\UNC\server\share\file`},
		{`\??\C:\nt\path`, `\??\C:\nt\path`},
		{`\\.\NUL`, `\\.\NUL`},
		{`relative\file.txt`, `relative\file.txt`},
	}
	for _, tc := range cases {
		if got := windowsExtendedPath(tc.in); got != tc.want {
			t.Fatalf("windowsExtendedPath(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestInspectSelectedRootKindSupportsLongPathBeyondMaxPath(t *testing.T) {
	base := t.TempDir()
	segment := strings.Repeat("a", 60)
	deepDir := filepath.Join(base, segment, segment, segment, segment, segment)
	if err := os.MkdirAll(deepDir, 0o700); err != nil {
		t.Fatal(err)
	}
	longFile := filepath.Join(deepDir, "payload.txt")
	if len(longFile) <= 260 {
		t.Fatalf("expected path > 260 chars, got len=%d", len(longFile))
	}
	if err := os.WriteFile(longFile, []byte("ok"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, kind, _, err := inspectSelectedRootKind(longFile)
	if err != nil || kind != sourceEntryRegularFile {
		t.Fatalf("inspectSelectedRootKind(longFile) = (kind=%d, err=%v)", kind, err)
	}
}

func TestClassifyWindowsEntryStripsStaleModeIrregularWhenHandleFileTypeKnown(t *testing.T) {
	kind, _ := classifyWindowsEntry(windowsEntryFacts{
		source: windowsAttributesFromHandle, mode: fs.ModeIrregular | 0o666,
		hasAttributes: true, attributes: windows.FILE_ATTRIBUTE_ARCHIVE,
		hasFileType: true, fileType: windows.FILE_TYPE_DISK,
	})
	if kind != sourceEntryRegularFile {
		t.Fatalf("non-reparse file with stale ModeIrregular = %d, want regular", kind)
	}
	pipeKind, _ := classifyWindowsEntry(windowsEntryFacts{
		source: windowsAttributesFromHandle, mode: fs.ModeNamedPipe,
		hasAttributes: true, attributes: windows.FILE_ATTRIBUTE_ARCHIVE,
		hasFileType: true, fileType: windows.FILE_TYPE_DISK,
	})
	if pipeKind != sourceEntrySpecialFile {
		t.Fatalf("non-reparse file with ModeNamedPipe = %d, want special", pipeKind)
	}
}

func TestInspectWindowsRelativeNoRecallPathsAuditAndNilDefenses(t *testing.T) {
	if _, err := inspectOpenedSourceHandle(nil); err == nil {
		t.Fatal("inspectOpenedSourceHandle(nil) succeeded")
	}
	if _, _, _, err := inspectWindowsChildNoRecall(nil, "file.txt", false); err == nil {
		t.Fatal("inspectWindowsChildNoRecall(nil) succeeded")
	}
	if _, _, _, err := inspectWindowsRelativeNoRecall(0, "file.txt", false); err == nil {
		t.Fatal("inspectWindowsRelativeNoRecall(0) succeeded")
	}

	rootDir := t.TempDir()
	nestedDir := filepath.Join(rootDir, "sub")
	if err := os.Mkdir(nestedDir, 0o700); err != nil {
		t.Fatal(err)
	}
	nestedFile := filepath.Join(nestedDir, "nested.txt")
	if err := os.WriteFile(nestedFile, []byte("nested"), 0o600); err != nil {
		t.Fatal(err)
	}
	deniedFile := filepath.Join(rootDir, "denied.txt")
	if err := os.WriteFile(deniedFile, []byte("secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	denyCatalogRead(t, deniedFile)

	root, err := os.OpenRoot(rootDir)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	dirHandle, err := openRootObjectNoRecall(root, ".")
	if err != nil {
		t.Fatal(err)
	}
	defer dirHandle.Close()

	// Forward-slash nested relative paths must resolve and match opened identity.
	beforeID, kind, _, err := inspectWindowsChildNoRecall(dirHandle, "sub/nested.txt", false)
	if err != nil || kind != sourceEntryRegularFile {
		t.Fatalf("inspect sub/nested.txt = (kind=%d, err=%v)", kind, err)
	}
	openedFile, err := openRootObjectNoRecall(root, "sub/nested.txt")
	if err != nil {
		t.Fatal(err)
	}
	opened, err := inspectOpenedSourceHandle(openedFile)
	_ = openedFile.Close()
	if err != nil || !beforeID.sameObject(opened.identity) || opened.size != 6 {
		t.Fatalf("opened nested = (%+v, err=%v) same=%v", opened, err, beforeID.sameObject(opened.identity))
	}

	// Escaping or absolute paths must be rejected before NtCreateFile.
	for _, invalid := range []string{"../escape.txt", rootDir, `C:\Windows`} {
		if _, _, _, err := inspectWindowsChildNoRecall(dirHandle, invalid, false); !errors.Is(err, content.ErrRevisionStale) {
			t.Fatalf("inspect invalid %q err = %v, want ErrRevisionStale", invalid, err)
		}
	}

	// Metadata-only relative inspection must succeed even when FILE_READ_DATA is denied.
	if _, deniedKind, _, err := inspectWindowsChildNoRecall(dirHandle, "denied.txt", false); err != nil || deniedKind != sourceEntryRegularFile {
		t.Fatalf("metadata-only inspect on read-denied file = (kind=%d, err=%v)", deniedKind, err)
	}

	// dirRecallOnOpen interception must set windowsAttrRecallOnOpen in audit (#13).
	_, recallKind, recallAudit, err := inspectWindowsChildNoRecall(dirHandle, "sub/nested.txt", true)
	if err != nil || recallKind != sourceEntryCloudPlaceholder ||
		recallAudit.attributes&windowsAttrRecallOnOpen == 0 || recallAudit.hydratedCloud {
		t.Fatalf("dirRecallOnOpen = (kind=%d, audit=%+v, err=%v)", recallKind, recallAudit, err)
	}
}

func TestClassifyRootRelativeOpenErrorMapsUnhandledReparseToStale(t *testing.T) {
	cantAccess := classifyRootRelativeOpenError("open stable revision", "app.exe", windows.ERROR_CANT_ACCESS_FILE)
	if !errors.Is(cantAccess, content.ErrRevisionStale) || content.RevisionComparisonOf(cantAccess) != content.RevisionComparisonUnavailable {
		t.Fatalf("ERROR_CANT_ACCESS_FILE = (%v, comparison=%v)", cantAccess, content.RevisionComparisonOf(cantAccess))
	}
	for _, reparseErr := range []error{windows.ERROR_REPARSE, windows.ERROR_REPARSE_OBJECT, windows.ERROR_REPARSE_POINT_ENCOUNTERED} {
		classified := classifyRootRelativeOpenError("inspect stable revision", "alias/file.bin", reparseErr)
		if !errors.Is(classified, content.ErrRevisionStale) || content.RevisionComparisonOf(classified) != content.RevisionComparisonMismatch {
			t.Fatalf("reparse error %v = (%v, comparison=%v)", reparseErr, classified, content.RevisionComparisonOf(classified))
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
	source.inspectChildHandleFn = func(name string, handle *os.File) (openedSourceHandle, error) {
		opened, err := inspectOpenedSourceHandle(handle)
		if err != nil {
			return openedSourceHandle{}, err
		}
		if name == "a-dehydrating.txt" {
			opened.kind = sourceEntryCloudPlaceholder
			opened.audit = sourceEntryAudit{
				attributes:       windows.FILE_ATTRIBUTE_REPARSE_POINT | windowsAttrRecallOnDataAccess,
				reparseTag:       0x9000401A,
				placeholderState: cfPlaceholderStatePlaceholder | cfPlaceholderStatePartial,
				hasCloudState:    true,
			}
		}
		return opened, nil
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
