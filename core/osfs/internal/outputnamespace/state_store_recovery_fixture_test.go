package outputnamespace

import (
	"bytes"
	"errors"
	"io"
	"slices"
	"testing"

	"github.com/windshare/windshare/core/osfs/internal/outputcap"
	"github.com/windshare/windshare/core/osfs/internal/resumestate"
	"github.com/windshare/windshare/core/transfer"
)

func writeStateStoreHeaderTemporary(
	t *testing.T,
	directory outputcap.Directory,
	name string,
	encoded []byte,
) {
	t.Helper()
	temporary, err := directory.CreateFile(name, true, int64(len(encoded)))
	if err != nil {
		t.Fatal(err)
	}
	written, writeErr := temporary.WriteAt(encoded, 0)
	if writeErr == nil && written != len(encoded) {
		writeErr = errors.New("short temporary write")
	}
	if err := errors.Join(writeErr, temporary.Sync(), directory.Sync(), temporary.Close()); err != nil {
		t.Fatal(err)
	}
}

func stateStoreHeaderImages(t *testing.T) (
	RecordImage,
	RecordImage,
	RecordImage,
) {
	t.Helper()
	selection := v3RecoverySelection(t, false, 0)
	root, err := resumestate.NewOutputRootBinding(
		resumestate.CertificationWindowsNTFSProcessRestart,
		[]byte("state-store-test-volume"),
		[]byte("state-store-test-root"),
	)
	if err != nil {
		t.Fatal(err)
	}
	sessionID, err := transfer.OutputSessionIDFromBytes(bytes.Repeat([]byte{0x61}, transfer.OutputSessionIdentityBytes))
	if err != nil {
		t.Fatal(err)
	}
	header, err := resumestate.NewHeader(resumestate.HeaderSpec{
		Backend: filesystemOutputBackendID, SessionID: sessionID, Selection: selection, OutputRoot: root,
		OutputAncestry: v3RecoveryAncestryBinding(t, root, selection),
	})
	if err != nil {
		t.Fatal(err)
	}
	control, err := resumestate.NewControl(resumestate.ControlSpec{
		Backend: filesystemOutputBackendID, OutputRoot: root,
		Certification: resumestate.CertificationWindowsNTFSProcessRestart,
		Durability:    transfer.DurabilityProcessRestart,
		Generation:    1,
	})
	if err != nil {
		t.Fatal(err)
	}
	namespace, err := resumestate.BindSessionNamespaceAuthority(
		control,
		header,
		resumestate.ResumeNamespaceName(selection.ResumeIntent()),
		resumestate.SessionDirectoryName(sessionID),
	)
	if err != nil {
		t.Fatal(err)
	}
	next, err := namespace.WithLifecycle(resumestate.SessionPausing)
	if err != nil {
		t.Fatal(err)
	}
	divergent, err := namespace.WithLifecycle(resumestate.SessionCompleting)
	if err != nil {
		t.Fatal(err)
	}
	encode := func(header resumestate.Header) RecordImage {
		encoded, encodeErr := resumestate.EncodeHeader(header)
		if encodeErr != nil {
			t.Fatal(encodeErr)
		}
		return RecordImage{encoded: encoded, generation: header.StateGeneration()}
	}
	return encode(namespace.Header()), encode(next.Header()), encode(divergent.Header())
}

func stateStoreReplacementFixture(
	t *testing.T,
	initial []byte,
) (outputcap.Platform, outputcap.Directory) {
	t.Helper()
	root := v3RecoveryRoot(t)
	platform, err := openOutputV3Platform(root, false)
	if err != nil {
		t.Fatal(err)
	}
	directory, err := platform.Root().CreateDirectory("state-replacement", true)
	if err != nil {
		_ = platform.Close()
		t.Fatal(err)
	}
	store := Store{random: bytes.NewReader(bytes.Repeat([]byte{0xa2}, 256))}
	if _, err := store.CreateRecord(
		directory, resumestate.HeaderRecordName, initial, resumestate.MaxSessionHeaderBytes,
	); err != nil {
		_ = directory.Close()
		_ = platform.Close()
		t.Fatal(err)
	}
	return platform, directory
}

func stateStoreEmptyDirectoryFixture(t *testing.T) (outputcap.Platform, outputcap.Directory) {
	t.Helper()
	root := v3RecoveryRoot(t)
	platform, err := openOutputV3Platform(root, false)
	if err != nil {
		t.Fatal(err)
	}
	directory, err := platform.Root().CreateDirectory("state-store", true)
	if err != nil {
		_ = platform.Close()
		t.Fatal(err)
	}
	return platform, directory
}

func closeStateStoreFixture(t *testing.T, platform outputcap.Platform, directory outputcap.Directory) {
	t.Helper()
	if err := errors.Join(directory.Close(), platform.Close()); err != nil {
		t.Error(err)
	}
}

type stateStoreFaultPoint uint8

const (
	stateStoreFaultNone stateStoreFaultPoint = iota
	stateStoreFaultCreate
	stateStoreFaultWrite
	stateStoreFaultFileSync
	stateStoreFaultTemporaryReopen
	stateStoreFaultTemporaryRead
	stateStoreFaultCurrentReopen
	stateStoreFaultReplaceBeforeMutation
	stateStoreFaultReplaceAfterMutation
	stateStoreFaultParentSync
	stateStoreFaultInstalledReopen
	stateStoreFaultInstalledDivergent
	stateStoreFaultCreateCollision
	stateStoreFaultShortWrite
	stateStoreFaultLink
	stateStoreFaultCreateParentSync
	stateStoreFaultRemove
	stateStoreFaultCreateFinalSync
	stateStoreFaultCurrentClose
	stateStoreFaultInstalledClose
	stateStoreFaultCreateTargetClose
	stateStoreFaultCreateFixedClose
	stateStoreFaultCreateTemporaryClose
)

var errStateStoreInjected = errors.New("injected state-store crash cut")

type stateStoreFaultDirectory struct {
	outputcap.Directory
	fault            stateStoreFaultPoint
	target           string
	temporary        string
	replaceAttempted bool
	linkAttempted    bool
	linkSyncs        int
	divergent        []byte
}

func (directory *stateStoreFaultDirectory) CreateFile(
	name string,
	private bool,
	size int64,
) (outputcap.File, error) {
	if directory.fault == stateStoreFaultCreateCollision {
		return nil, outputcap.ErrNamespaceCollision
	}
	if directory.fault == stateStoreFaultCreate {
		return nil, errStateStoreInjected
	}
	file, err := directory.Directory.CreateFile(name, private, size)
	if err != nil {
		return nil, err
	}
	directory.temporary = name
	return &stateStoreFaultFile{File: file, owner: directory, name: name}, nil
}

func (directory *stateStoreFaultDirectory) OpenFile(
	name string,
	private bool,
	writable bool,
) (outputcap.File, error) {
	if name == directory.temporary && directory.fault == stateStoreFaultTemporaryReopen {
		return nil, errStateStoreInjected
	}
	if name == directory.target && !directory.replaceAttempted && directory.fault == stateStoreFaultCurrentReopen {
		return nil, errStateStoreInjected
	}
	if name == directory.target && directory.replaceAttempted && directory.fault == stateStoreFaultInstalledReopen {
		return nil, errStateStoreInjected
	}
	file, err := directory.Directory.OpenFile(name, private, writable)
	if err != nil {
		return nil, err
	}
	wrapped := &stateStoreFaultFile{File: file, owner: directory, name: name, reopened: true}
	if name == directory.target && directory.replaceAttempted && directory.fault == stateStoreFaultInstalledDivergent {
		wrapped.readOverride = bytes.Clone(directory.divergent)
	}
	return wrapped, nil
}

func (directory *stateStoreFaultDirectory) LinkFileNoReplace(source outputcap.File, name string) (outputcap.File, error) {
	if directory.fault == stateStoreFaultLink {
		return nil, errStateStoreInjected
	}
	if wrapped, ok := source.(*stateStoreFaultFile); ok {
		source = wrapped.File
	}
	linked, err := directory.Directory.LinkFileNoReplace(source, name)
	if err != nil {
		return nil, err
	}
	directory.linkAttempted = true
	return &stateStoreFaultFile{File: linked, owner: directory, name: name, linkedTarget: true}, nil
}

func (directory *stateStoreFaultDirectory) ReplacePrivateFile(source outputcap.File, name string) error {
	directory.replaceAttempted = true
	if directory.fault == stateStoreFaultReplaceBeforeMutation {
		return errStateStoreInjected
	}
	if wrapped, ok := source.(*stateStoreFaultFile); ok {
		source = wrapped.File
	}
	err := directory.Directory.ReplacePrivateFile(source, name)
	if err == nil && directory.fault == stateStoreFaultReplaceAfterMutation {
		return errStateStoreInjected
	}
	return err
}

func (directory *stateStoreFaultDirectory) RemoveFile(name string, expected outputcap.File) error {
	if directory.linkAttempted && name == directory.temporary && directory.fault == stateStoreFaultRemove {
		return errStateStoreInjected
	}
	if wrapped, ok := expected.(*stateStoreFaultFile); ok {
		expected = wrapped.File
	}
	return directory.Directory.RemoveFile(name, expected)
}

func (directory *stateStoreFaultDirectory) Sync() error {
	if directory.replaceAttempted && directory.fault == stateStoreFaultParentSync {
		return errStateStoreInjected
	}
	if directory.linkAttempted {
		directory.linkSyncs++
		if directory.fault == stateStoreFaultCreateParentSync && directory.linkSyncs == 1 {
			return errStateStoreInjected
		}
		if directory.fault == stateStoreFaultCreateFinalSync && directory.linkSyncs == 2 {
			return errStateStoreInjected
		}
	}
	return directory.Directory.Sync()
}

type stateStoreFaultFile struct {
	outputcap.File
	owner        *stateStoreFaultDirectory
	name         string
	reopened     bool
	linkedTarget bool
	readOverride []byte
}

func (file *stateStoreFaultFile) WriteAt(value []byte, offset int64) (int, error) {
	if file.name == file.owner.temporary && !file.reopened && file.owner.fault == stateStoreFaultWrite {
		return 0, errStateStoreInjected
	}
	if file.name == file.owner.temporary && !file.reopened && file.owner.fault == stateStoreFaultShortWrite {
		return len(value) - 1, nil
	}
	return file.File.WriteAt(value, offset)
}

func (file *stateStoreFaultFile) Sync() error {
	if file.name == file.owner.temporary && !file.reopened && file.owner.fault == stateStoreFaultFileSync {
		return errStateStoreInjected
	}
	return file.File.Sync()
}

func (file *stateStoreFaultFile) ReadAt(value []byte, offset int64) (int, error) {
	if file.readOverride != nil {
		if offset != 0 {
			return 0, errors.New("state-store override only supports offset zero")
		}
		return copy(value, file.readOverride), nil
	}
	if file.name == file.owner.temporary && file.reopened && file.owner.fault == stateStoreFaultTemporaryRead {
		return 0, errStateStoreInjected
	}
	return file.File.ReadAt(value, offset)
}

func (file *stateStoreFaultFile) Size() (uint64, error) {
	if file.readOverride != nil {
		return uint64(len(file.readOverride)), nil
	}
	return file.File.Size()
}

func (file *stateStoreFaultFile) SameFile(other outputcap.File) (bool, error) {
	if wrapped, ok := other.(*stateStoreFaultFile); ok {
		other = wrapped.File
	}
	return file.File.SameFile(other)
}

func (file *stateStoreFaultFile) Close() error {
	injected := false
	switch file.owner.fault {
	case stateStoreFaultCurrentClose:
		injected = file.name == file.owner.target && file.reopened && !file.owner.replaceAttempted
	case stateStoreFaultInstalledClose:
		injected = file.name == file.owner.target && file.reopened && file.owner.replaceAttempted
	case stateStoreFaultCreateTargetClose:
		injected = file.name == file.owner.target && file.linkedTarget
	case stateStoreFaultCreateFixedClose:
		injected = file.name == file.owner.target && file.reopened && file.owner.linkAttempted
	case stateStoreFaultCreateTemporaryClose:
		injected = file.name == file.owner.temporary && !file.reopened && !file.linkedTarget && file.owner.linkAttempted
	}
	if injected {
		return errors.Join(file.File.Close(), errStateStoreInjected)
	}
	return file.File.Close()
}

type stateStoreReconcileFaultDirectory struct {
	outputcap.Directory
	listedMissing    string
	namesOverride    []string
	namesErr         error
	classifyOverride bool
	classifyName     string
	classifyKind     outputcap.EntryKind
	classifyErr      error
	openName         string
	openErr          error
	removeErr        error
	syncErr          error
}

func (directory *stateStoreReconcileFaultDirectory) NamesWithPrefix(prefix string, limit int) ([]string, error) {
	if directory.namesErr != nil {
		return nil, directory.namesErr
	}
	if directory.namesOverride != nil {
		return slices.Clone(directory.namesOverride), nil
	}
	if directory.listedMissing != "" {
		return []string{directory.listedMissing}, nil
	}
	return directory.Directory.NamesWithPrefix(prefix, limit)
}

func (directory *stateStoreReconcileFaultDirectory) ClassifyExactEntry(
	name string,
) (outputcap.EntryKind, bool, error) {
	if directory.classifyOverride && name == directory.classifyName {
		return directory.classifyKind, true, directory.classifyErr
	}
	if name == directory.listedMissing {
		return outputcap.EntryAbsent, true, nil
	}
	return directory.Directory.ClassifyExactEntry(name)
}

func (directory *stateStoreReconcileFaultDirectory) OpenFile(
	name string,
	private bool,
	writable bool,
) (outputcap.File, error) {
	if name == directory.openName && directory.openErr != nil {
		return nil, directory.openErr
	}
	return directory.Directory.OpenFile(name, private, writable)
}

func (directory *stateStoreReconcileFaultDirectory) RemoveFile(name string, expected outputcap.File) error {
	if directory.removeErr != nil {
		return directory.removeErr
	}
	return directory.Directory.RemoveFile(name, expected)
}

func (directory *stateStoreReconcileFaultDirectory) Sync() error {
	if directory.syncErr != nil {
		return directory.syncErr
	}
	return directory.Directory.Sync()
}

type stateStoreReadFile struct {
	outputcap.File
	size    uint64
	sizeErr error
	data    []byte
	readErr error
}

func (file *stateStoreReadFile) Size() (uint64, error) { return file.size, file.sizeErr }

func (file *stateStoreReadFile) ReadAt(target []byte, _ int64) (int, error) {
	return copy(target, file.data), file.readErr
}

type stateStoreDirectoryCreationFault struct {
	outputcap.Directory
	classifyOverride bool
	classifyKind     outputcap.EntryKind
	classifyErr      error
	createCollision  bool
	createErr        error
	childSyncErr     error
	parentSyncErr    error
}

func (directory *stateStoreDirectoryCreationFault) ClassifyExactEntry(
	name string,
) (outputcap.EntryKind, bool, error) {
	if directory.classifyOverride {
		return directory.classifyKind, false, directory.classifyErr
	}
	return directory.Directory.ClassifyExactEntry(name)
}

func (directory *stateStoreDirectoryCreationFault) CreateDirectory(
	name string,
	private bool,
) (outputcap.Directory, error) {
	if directory.createErr != nil {
		return nil, directory.createErr
	}
	created, err := directory.Directory.CreateDirectory(name, private)
	if err != nil {
		return nil, err
	}
	if directory.createCollision {
		if err := errors.Join(created.Sync(), directory.Directory.Sync(), created.Close()); err != nil {
			return nil, err
		}
		return nil, outputcap.ErrNamespaceCollision
	}
	if directory.childSyncErr != nil {
		return &stateStoreDirectorySyncFault{
			Directory: created,
			syncErr:   directory.childSyncErr,
		}, nil
	}
	return created, nil
}

func (directory *stateStoreDirectoryCreationFault) Sync() error {
	if directory.parentSyncErr != nil {
		return directory.parentSyncErr
	}
	return directory.Directory.Sync()
}

type stateStoreDirectorySyncFault struct {
	outputcap.Directory
	syncErr error
}

func (directory *stateStoreDirectorySyncFault) Sync() error { return directory.syncErr }

func createCandidateDirectory(t *testing.T, parent outputcap.Directory, value byte) outputcap.Directory {
	t.Helper()
	name := "candidate-fixture-" + string(rune('a'+value%26)) + string(rune('a'+value/26%26))
	directory, err := parent.CreateDirectory(name, true)
	if err != nil {
		t.Fatal(err)
	}
	return directory
}

func createNamedCandidateDirectory(
	t *testing.T,
	parent outputcap.Directory,
	name string,
) outputcap.Directory {
	t.Helper()
	directory, err := parent.CreateDirectory(name, true)
	if err != nil {
		t.Fatal(err)
	}
	if err := errors.Join(directory.Sync(), parent.Sync()); err != nil {
		t.Fatal(err)
	}
	return directory
}

func createClosedNamedCandidateDirectory(
	t *testing.T,
	parent outputcap.Directory,
	name string,
) {
	t.Helper()
	directory := createNamedCandidateDirectory(t, parent, name)
	if err := directory.Close(); err != nil {
		t.Fatal(err)
	}
}

func createCandidateFileWithPayload(
	t *testing.T,
	parent outputcap.Directory,
	name string,
	payload []byte,
) {
	t.Helper()
	file, err := parent.CreateFile(name, true, int64(len(payload)))
	if err != nil {
		t.Fatal(err)
	}
	if len(payload) != 0 {
		if written, err := file.WriteAt(payload, 0); err != nil || written != len(payload) {
			t.Fatalf("write candidate payload = (%d, %v)", written, err)
		}
	}
	if err := errors.Join(file.Sync(), parent.Sync(), file.Close()); err != nil {
		t.Fatal(err)
	}
}

func TestOutputV3StateRecordReadEnforcesExactBoundedImage(t *testing.T) {
	readErr := errors.New("read failed")
	for _, test := range []struct {
		name  string
		file  outputcap.File
		limit int
		want  []byte
	}{
		{name: "nil-file", limit: 1},
		{name: "invalid-limit", file: &stateStoreReadFile{size: 1}, limit: 0},
		{name: "size-error", file: &stateStoreReadFile{sizeErr: errStateStoreInjected}, limit: 1},
		{name: "empty", file: &stateStoreReadFile{}, limit: 1},
		{name: "oversize", file: &stateStoreReadFile{size: 2}, limit: 1},
		{name: "read-error", file: &stateStoreReadFile{size: 1, readErr: readErr}, limit: 1},
		{name: "short-read", file: &stateStoreReadFile{size: 2, data: []byte{1}}, limit: 2},
		{name: "full-read-with-eof", file: &stateStoreReadFile{size: 2, data: []byte{1, 2}, readErr: io.EOF}, limit: 2, want: []byte{1, 2}},
		{name: "exact", file: &stateStoreReadFile{size: 2, data: []byte{1, 2}}, limit: 2, want: []byte{1, 2}},
	} {
		t.Run(test.name, func(t *testing.T) {
			actual, err := ReadFile(test.file, test.limit)
			if test.want == nil {
				if err == nil {
					t.Fatalf("read = %v, want error", actual)
				}
				return
			}
			if err != nil || !bytes.Equal(actual, test.want) {
				t.Fatalf("read = %v, %v; want %v", actual, err, test.want)
			}
		})
	}
}
