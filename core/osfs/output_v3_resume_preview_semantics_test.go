package osfs

import (
	"context"
	"errors"
	"io/fs"
	"math"
	"testing"

	"github.com/windshare/windshare/core/osfs/internal/resumestate"
	"github.com/windshare/windshare/core/transfer"
)

func TestOutputV3ResumeAPIsRejectUnboundAndCanceledCapabilities(t *testing.T) {
	if inventory, err := (*FilesystemOutputAuthority)(nil).listResumeState(
		context.Background(), FilesystemResumeRoot{RootPath: "root"},
	); inventory != nil || !errors.Is(err, transfer.ErrInvalidOutputBinding) {
		t.Fatalf("nil authority listing = (%v, %v)", inventory, err)
	}
	root := v3RecoveryRoot(t)
	authority := v3RecoveryAuthority(t, root, nil)
	if inventory, err := authority.listResumeState(context.Background(), FilesystemResumeRoot{}); inventory != nil ||
		!errors.Is(err, transfer.ErrInvalidOutputBinding) {
		t.Fatalf("unbound root listing = (%v, %v)", inventory, err)
	}
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	if inventory, err := authority.listResumeState(canceled, FilesystemResumeRoot{RootPath: root}); inventory != nil ||
		!errors.Is(err, context.Canceled) {
		t.Fatalf("canceled listing = (%v, %v)", inventory, err)
	}

	if settlement, err := (*FilesystemOutputAuthority)(nil).discardResumeState(
		context.Background(), ResumeStateRef{},
	); settlement.Kind != 0 || !errors.Is(err, transfer.ErrInvalidOutputBinding) {
		t.Fatalf("nil authority discard = (%+v, %v)", settlement, err)
	}
	reference := ResumeStateRef{inventory: &ResumeStateInventory{}}
	if settlement, err := authority.discardResumeState(canceled, reference); settlement.Kind != 0 ||
		!errors.Is(err, context.Canceled) {
		t.Fatalf("canceled discard = (%+v, %v)", settlement, err)
	}
	if settlement, err := authority.discardResumeState(context.Background(), reference); settlement.Kind != 0 ||
		!errors.Is(err, transfer.ErrInvalidOutputBinding) {
		t.Fatalf("unowned discard capability = (%+v, %v)", settlement, err)
	}
}

func TestOutputV3PrivateTreePreviewEnforcesBudgetsBeforeTraversal(t *testing.T) {
	injected := errors.New("private tree enumeration failed")
	if preview, err := previewPrivateTree(&outputV3ResumePreviewDirectory{namesErr: injected}); !errors.Is(err, injected) || preview.allocatedBytes != 0 || preview.fileRecords != 0 ||
		preview.entries != 0 || len(preview.attention) != 0 {
		t.Fatalf("faulted private tree preview = (%+v, %v)", preview, err)
	}

	for _, test := range []struct {
		name    string
		depth   int
		entries int
		names   []string
	}{
		{name: "depth", depth: resumestate.MaxStateNestingDepth + 1},
		{name: "preexisting entry budget", entries: resumeTreeEntryLimit + 1},
		{name: "next entry budget", entries: resumeTreeEntryLimit, names: []string{"next"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			preview := privateTreePreview{entries: test.entries}
			err := previewPrivateDirectory(
				&outputV3ResumePreviewDirectory{names: test.names}, "", test.depth, &preview,
			)
			if !errors.Is(err, errOutputInspectionLimit) {
				t.Fatalf("private tree budget error = %v", err)
			}
		})
	}
}

func TestOutputV3PrivateTreePreviewPreservesUnsafeEntryEvidence(t *testing.T) {
	injected := errors.New("private entry inspection failed")
	emptyChild := func(closeErr error) outputV3Directory {
		return &outputV3ResumePreviewDirectory{closeErr: closeErr}
	}

	for _, test := range []struct {
		name          string
		directory     *outputV3ResumePreviewDirectory
		prefix        string
		allocated     uint64
		wantAllocated uint64
		wantRecords   uint64
		wantAttention int
		cause         error
	}{
		{
			name: "open fixed entry",
			directory: &outputV3ResumePreviewDirectory{
				names: []string{"entry"}, openEntryErr: injected,
			},
			cause: injected,
		},
		{
			name: "strict private open becomes attention",
			directory: &outputV3ResumePreviewDirectory{
				names: []string{"entry"}, entry: previewEntry(outputV3EntryRegularFile, 3),
				strictErr: injected, matches: true,
			},
			wantAllocated: 3, wantAttention: 1,
		},
		{
			name: "strict private close becomes attention",
			directory: &outputV3ResumePreviewDirectory{
				names: []string{"entry"}, entry: previewEntry(outputV3EntryRegularFile, 4),
				strictCloseErr: injected, matches: true,
			},
			wantAllocated: 4, wantAttention: 1,
		},
		{
			name: "fixed entry identity mismatch",
			directory: &outputV3ResumePreviewDirectory{
				names: []string{"entry"}, entry: previewEntry(outputV3EntryRegularFile, 1),
			},
			cause: errOutputV3Unsafe,
		},
		{
			name: "fixed entry comparison failure",
			directory: &outputV3ResumePreviewDirectory{
				names: []string{"entry"}, entry: previewEntry(outputV3EntryRegularFile, 1),
				matchErr: injected,
			},
			cause: injected,
		},
		{
			name: "allocated size failure",
			directory: &outputV3ResumePreviewDirectory{
				names: []string{"entry"}, entry: &outputV3ResumePreviewEntry{
					kind: outputV3EntryRegularFile, allocatedErr: injected,
				}, matches: true,
			},
			cause: injected,
		},
		{
			name: "entry close failure",
			directory: &outputV3ResumePreviewDirectory{
				names: []string{"entry"}, entry: &outputV3ResumePreviewEntry{
					kind: outputV3EntryRegularFile, closeErr: injected,
				}, matches: true,
			},
			cause: injected,
		},
		{
			name: "allocated byte overflow",
			directory: &outputV3ResumePreviewDirectory{
				names: []string{"entry"}, entry: previewEntry(outputV3EntryRegularFile, 1), matches: true,
			},
			allocated: math.MaxUint64, cause: errOutputV3Unsafe,
		},
		{
			name: "file record accounting",
			directory: &outputV3ResumePreviewDirectory{
				names: []string{"record.state"}, entry: previewEntry(outputV3EntryRegularFile, 5), matches: true,
			},
			prefix: resumestate.FilesDirectoryName + "/aa", wantAllocated: 5, wantRecords: 1,
		},
		{
			name: "public fallback keeps unsafe-directory attention",
			directory: &outputV3ResumePreviewDirectory{
				names: []string{"directory"}, entry: previewEntry(outputV3EntryDirectory, 0),
				privateChildErr: injected, publicChild: emptyChild(nil), matches: true,
			},
			wantAttention: 1,
		},
		{
			name: "directory identity changed after recursive preview",
			directory: &outputV3ResumePreviewDirectory{
				names: []string{"directory"}, entry: previewEntry(outputV3EntryDirectory, 0),
				privateChild: emptyChild(nil),
			},
			cause: errOutputV3Unsafe,
		},
		{
			name: "directory revalidation failed after recursive preview",
			directory: &outputV3ResumePreviewDirectory{
				names: []string{"directory"}, entry: previewEntry(outputV3EntryDirectory, 0),
				privateChild: emptyChild(nil), matchErr: injected,
			},
			cause: injected,
		},
		{
			name: "unopenable directory",
			directory: &outputV3ResumePreviewDirectory{
				names: []string{"directory"}, entry: previewEntry(outputV3EntryDirectory, 0),
				privateChildErr: injected, publicChildErr: injected,
			},
			cause: injected,
		},
		{
			name: "child close failure",
			directory: &outputV3ResumePreviewDirectory{
				names: []string{"directory"}, entry: previewEntry(outputV3EntryDirectory, 0),
				privateChild: emptyChild(injected), matches: true,
			},
			cause: injected,
		},
		{
			name: "opaque entry becomes attention",
			directory: &outputV3ResumePreviewDirectory{
				names: []string{"opaque"}, entry: previewEntry(outputV3EntryOther, 0),
			},
			wantAttention: 1,
		},
		{
			name: "opaque entry close failure",
			directory: &outputV3ResumePreviewDirectory{
				names: []string{"opaque"}, entry: &outputV3ResumePreviewEntry{
					kind: outputV3EntryOther, closeErr: injected,
				},
			},
			cause: injected,
		},
		{
			name: "vanished fixed entry",
			directory: &outputV3ResumePreviewDirectory{
				names: []string{"missing"}, entry: previewEntry(outputV3EntryAbsent, 0),
			},
			cause: errOutputV3Unsafe,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			preview := privateTreePreview{allocatedBytes: test.allocated}
			err := previewPrivateDirectory(test.directory, test.prefix, 0, &preview)
			if test.cause != nil {
				if !errors.Is(err, test.cause) {
					t.Fatalf("private entry preview error = %v", err)
				}
				return
			}
			if err != nil || preview.allocatedBytes != test.wantAllocated ||
				preview.fileRecords != test.wantRecords || len(preview.attention) != test.wantAttention {
				t.Fatalf("private entry preview = (%+v, %v)", preview, err)
			}
		})
	}
}

func TestOutputV3PrivateEntryRemovalRequiresLiveAuthority(t *testing.T) {
	injected := errors.New("private removal authority failed")
	for _, test := range []struct {
		name         string
		directory    *outputV3ResumePreviewDirectory
		verifyErr    error
		depth        int
		cause        error
		wantRemovals int
	}{
		{
			name:      "already absent at open",
			directory: &outputV3ResumePreviewDirectory{openEntryErr: fs.ErrNotExist},
		},
		{
			name:      "entry open failure",
			directory: &outputV3ResumePreviewDirectory{openEntryErr: injected},
			cause:     injected,
		},
		{
			name:      "already absent after pin",
			directory: &outputV3ResumePreviewDirectory{entry: previewEntry(outputV3EntryAbsent, 0)},
		},
		{
			name:      "unknown pinned entry kind",
			directory: &outputV3ResumePreviewDirectory{entry: previewEntry(outputV3EntryKind(0xff), 0)},
			cause:     errOutputV3Unsafe,
		},
		{
			name:      "regular entry authority changed",
			directory: &outputV3ResumePreviewDirectory{entry: previewEntry(outputV3EntryRegularFile, 0)},
			verifyErr: injected, cause: injected,
		},
		{
			name:         "regular entry removal",
			directory:    &outputV3ResumePreviewDirectory{entry: previewEntry(outputV3EntryRegularFile, 0)},
			wantRemovals: 1,
		},
		{
			name: "directory cannot be pinned",
			directory: &outputV3ResumePreviewDirectory{
				entry: previewEntry(outputV3EntryDirectory, 0), publicChildErr: injected,
			},
			cause: injected,
		},
		{
			name: "directory depth budget",
			directory: &outputV3ResumePreviewDirectory{
				entry: previewEntry(outputV3EntryDirectory, 0), publicChild: &outputV3ResumePreviewDirectory{},
				matches: true,
			},
			depth: resumestate.MaxStateNestingDepth, cause: errOutputInspectionLimit,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			verify := func() error { return test.verifyErr }
			err := removePrivateEntry(test.directory, "entry", test.depth, verify)
			if test.cause != nil {
				if !errors.Is(err, test.cause) {
					t.Fatalf("private entry removal error = %v", err)
				}
				return
			}
			if err != nil || test.directory.removeCalls != test.wantRemovals {
				t.Fatalf("private entry removal = (calls=%d, %v)", test.directory.removeCalls, err)
			}
		})
	}
}

type outputV3ResumePreviewDirectory struct {
	outputV3Directory
	names           []string
	namesErr        error
	entry           outputV3EntryRef
	openEntryErr    error
	strictErr       error
	strictCloseErr  error
	matches         bool
	matchErr        error
	privateChild    outputV3Directory
	privateChildErr error
	publicChild     outputV3Directory
	publicChildErr  error
	removeErr       error
	removeCalls     int
	removeFileErr   error
	removeFileCalls int
	syncErr         error
	closeErr        error
}

func (directory *outputV3ResumePreviewDirectory) Names(int) ([]string, error) {
	if directory.namesErr != nil {
		return nil, directory.namesErr
	}
	return append([]string(nil), directory.names...), nil
}

func (directory *outputV3ResumePreviewDirectory) OpenEntry(string) (outputV3EntryRef, error) {
	if directory.openEntryErr != nil {
		return nil, directory.openEntryErr
	}
	return directory.entry, nil
}

func (directory *outputV3ResumePreviewDirectory) OpenFile(
	string,
	bool,
	bool,
) (outputV3File, error) {
	if directory.strictErr != nil {
		return nil, directory.strictErr
	}
	return &outputV3ResumePreviewFile{closeErr: directory.strictCloseErr}, nil
}

func (directory *outputV3ResumePreviewDirectory) EntryMatches(
	_ string,
	_ outputV3EntryRef,
) (bool, error) {
	return directory.matches, directory.matchErr
}

func (directory *outputV3ResumePreviewDirectory) OpenPinnedDirectory(
	_ outputV3EntryRef,
	private bool,
) (outputV3Directory, error) {
	if private {
		return directory.privateChild, directory.privateChildErr
	}
	return directory.publicChild, directory.publicChildErr
}

func (directory *outputV3ResumePreviewDirectory) RemoveEntry(_ string, _ outputV3EntryRef) error {
	directory.removeCalls++
	return directory.removeErr
}

func (directory *outputV3ResumePreviewDirectory) RemoveFile(_ string, _ outputV3File) error {
	directory.removeFileCalls++
	return directory.removeFileErr
}

func (directory *outputV3ResumePreviewDirectory) Sync() error { return directory.syncErr }

func (directory *outputV3ResumePreviewDirectory) Close() error { return directory.closeErr }

type outputV3ResumePreviewEntry struct {
	kind         outputV3EntryKind
	allocated    uint64
	allocatedErr error
	closeErr     error
}

func (entry *outputV3ResumePreviewEntry) Kind() outputV3EntryKind { return entry.kind }
func (entry *outputV3ResumePreviewEntry) AllocatedSize() (uint64, error) {
	return entry.allocated, entry.allocatedErr
}
func (entry *outputV3ResumePreviewEntry) Close() error { return entry.closeErr }

type outputV3ResumePreviewFile struct {
	outputV3File
	closeErr error
}

func (file *outputV3ResumePreviewFile) Close() error { return file.closeErr }

func previewEntry(kind outputV3EntryKind, allocated uint64) outputV3EntryRef {
	return &outputV3ResumePreviewEntry{kind: kind, allocated: allocated}
}
