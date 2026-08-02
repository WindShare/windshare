package outputruntime

import (
	"fmt"
	"io"
	"io/fs"
	"slices"
	"testing"

	"github.com/windshare/windshare/core/content"
	"github.com/windshare/windshare/core/osfs/internal/outputcap"
	"github.com/windshare/windshare/core/osfs/internal/resumestate"
	"github.com/windshare/windshare/core/transfer"
)

const outputV3RestartScaleFileCount = 128

func TestOutputV3RestartPreflightIndexesLargePublishedStateSet(t *testing.T) {
	root := v3RecoveryRoot(t)
	paths := make([]string, outputV3RestartScaleFileCount)
	for index := range paths {
		paths[index] = fmt.Sprintf("file-%03d.bin", index)
	}
	selection := v3RecoverySelectionPaths(t, paths, 1)
	opened := v3RecoveryOpen(t, v3RecoveryAuthority(t, root, nil), root, selection)
	t.Cleanup(func() { v3RecoveryCloseSession(t, opened.Session) })

	stateRoot := newOutputV3ScaleDirectoryNode()
	objects := &v3RecoveryObjectIDs{}
	for index := range paths {
		file := v3RecoveryOutputFileAt(t, opened.Session, selection, index)
		object, err := objects.NewOutputObjectID()
		if err != nil {
			t.Fatal(err)
		}
		published := outputV3ScalePublishedRecord(t, opened.Session.state, file, object)
		encoded, err := resumestate.EncodeFileRecord(published)
		if err != nil {
			t.Fatal(err)
		}
		name := resumestate.FileRecordName(published.Record().LocatorDigest())
		stateRoot.addFile(name.Shard(), name.Name(), encoded)
	}

	// Namespace scale is independent of native handle latency. The native single-
	// file tests own platform witness semantics; this in-memory capability keeps the
	// 128-record global preflight and reducer coverage in the fast core gate.
	opened.Session.mu.Lock()
	nativeFiles := opened.Session.filesDir
	opened.Session.filesDir = &outputV3ScaleDirectory{node: stateRoot}
	opened.Session.mu.Unlock()
	if err := nativeFiles.Close(); err != nil {
		t.Fatalf("close replaced native file-state directory: %v", err)
	}

	snapshot, err := scanOutputV3FileNamespace(opened.Session)
	if err != nil {
		t.Fatal(err)
	}
	if len(snapshot.records) != outputV3RestartScaleFileCount || len(snapshot.attention) != 0 ||
		len(snapshot.duplicateObjects) != 0 {
		t.Fatalf("restart preflight = records %d, attention %d, duplicates %d",
			len(snapshot.records), len(snapshot.attention), len(snapshot.duplicateObjects))
	}
	if err := opened.Session.adoptFileNamespaceSnapshot(snapshot); err != nil {
		t.Fatal(err)
	}
	if len(opened.Session.objectClaims) != outputV3RestartScaleFileCount {
		t.Fatalf("adopted object claims = %d, want %d", len(opened.Session.objectClaims), outputV3RestartScaleFileCount)
	}

	observation := resumestate.FileObservation{
		Anchor: resumestate.AnchorVerified, Stage: resumestate.EntryMissing,
		Final: resumestate.EntrySameAsAnchor, Metadata: resumestate.MetadataMatches,
	}
	for index, scanned := range snapshot.records {
		decision, err := resumestate.ReduceFileRecovery(scanned.bound, observation)
		if err != nil || decision.Settlement() != resumestate.RecoveryPublished {
			t.Fatalf("published recovery decision %d = (%v, %v)", index, decision.Settlement(), err)
		}
	}
}

func outputV3ScalePublishedRecord(
	t *testing.T,
	session resumestate.SessionAuthority,
	file transfer.OutputFile,
	object resumestate.OutputObjectID,
) resumestate.BoundFileRecord {
	t.Helper()
	resumable, err := resumestate.NewFileRecord(resumestate.FileRecordSpec{
		Session: session, Descriptor: file.Descriptor,
		CanonicalLocator: file.Path, OutputObject: object,
	})
	if err != nil {
		t.Fatal(err)
	}
	witness := resumestate.FileObservation{
		Anchor: resumestate.AnchorVerified, Stage: resumestate.EntrySameAsAnchor,
		Final: resumestate.EntryMissing,
	}
	decision, err := resumestate.ReduceFileRecovery(resumable.Bound(), witness)
	if err != nil {
		t.Fatal(err)
	}
	witnessed, err := resumestate.ApplyRecoveryDecision(resumable.Bound(), decision)
	if err != nil {
		t.Fatal(err)
	}
	resumable, err = resumestate.BindResumableFile(witnessed, file.Descriptor)
	if err != nil {
		t.Fatal(err)
	}
	ranges, err := content.NewRangeSet([]content.Range{{Offset: 0, End: file.ExpectedSize}})
	if err != nil {
		t.Fatal(err)
	}
	resumable, err = resumable.WithCheckpoint(1, ranges)
	if err != nil {
		t.Fatal(err)
	}
	publishing, err := resumestate.PreparePublication(resumable)
	if err != nil {
		t.Fatal(err)
	}
	decision, err = resumestate.ReduceFileRecovery(publishing, resumestate.FileObservation{
		Anchor: resumestate.AnchorVerified, Stage: resumestate.EntrySameAsAnchor,
		Final: resumestate.EntrySameAsAnchor, Metadata: resumestate.MetadataMatches,
		FinalParent: resumestate.FinalParentSynced,
	})
	if err != nil {
		t.Fatal(err)
	}
	published, err := resumestate.ApplyRecoveryDecision(publishing, decision)
	if err != nil {
		t.Fatal(err)
	}
	return published
}

type outputV3ScaleDirectoryNode struct {
	directories map[string]*outputV3ScaleDirectoryNode
	files       map[string][]byte
}

func newOutputV3ScaleDirectoryNode() *outputV3ScaleDirectoryNode {
	return &outputV3ScaleDirectoryNode{
		directories: make(map[string]*outputV3ScaleDirectoryNode),
		files:       make(map[string][]byte),
	}
}

func (node *outputV3ScaleDirectoryNode) addFile(shard, name string, encoded []byte) {
	directory, found := node.directories[shard]
	if !found {
		directory = newOutputV3ScaleDirectoryNode()
		node.directories[shard] = directory
	}
	directory.files[name] = slices.Clone(encoded)
}

type outputV3ScaleDirectory struct {
	outputcap.Directory
	node *outputV3ScaleDirectoryNode
}

func (directory *outputV3ScaleDirectory) Close() error { return nil }

func (directory *outputV3ScaleDirectory) Names(limit int) ([]string, error) {
	names := make([]string, 0, len(directory.node.directories)+len(directory.node.files))
	for name := range directory.node.directories {
		names = append(names, name)
	}
	for name := range directory.node.files {
		names = append(names, name)
	}
	if limit < len(names) {
		return nil, outputcap.ErrUnsafeNamespace
	}
	return names, nil
}

func (directory *outputV3ScaleDirectory) ClassifyExactEntry(name string) (outputcap.EntryKind, bool, error) {
	if _, found := directory.node.directories[name]; found {
		return outputcap.EntryDirectory, true, nil
	}
	if _, found := directory.node.files[name]; found {
		return outputcap.EntryRegularFile, true, nil
	}
	return outputcap.EntryAbsent, true, nil
}

func (directory *outputV3ScaleDirectory) OpenDirectory(name string, _ bool) (outputcap.Directory, error) {
	node, found := directory.node.directories[name]
	if !found {
		return nil, fs.ErrNotExist
	}
	return &outputV3ScaleDirectory{node: node}, nil
}

func (directory *outputV3ScaleDirectory) SameDirectory(other outputcap.Directory) (bool, error) {
	candidate, ok := other.(*outputV3ScaleDirectory)
	return ok && directory.node == candidate.node, nil
}

func (directory *outputV3ScaleDirectory) OpenFile(name string, _, _ bool) (outputcap.File, error) {
	encoded, found := directory.node.files[name]
	if !found {
		return nil, fs.ErrNotExist
	}
	return &outputV3ScaleFile{encoded: encoded}, nil
}

type outputV3ScaleFile struct {
	outputcap.File
	encoded []byte
}

func (file *outputV3ScaleFile) Close() error { return nil }
func (file *outputV3ScaleFile) Size() (uint64, error) {
	return uint64(len(file.encoded)), nil
}
func (file *outputV3ScaleFile) ReadAt(target []byte, offset int64) (int, error) {
	if offset < 0 || offset >= int64(len(file.encoded)) {
		return 0, io.EOF
	}
	read := copy(target, file.encoded[offset:])
	if read != len(target) {
		return read, io.EOF
	}
	return read, nil
}
