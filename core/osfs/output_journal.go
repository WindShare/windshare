package osfs

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"math"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"sync"

	"github.com/windshare/windshare/core/catalog"
	"github.com/windshare/windshare/core/content"
	"github.com/windshare/windshare/core/transfer"
)

const (
	outputJournalSchema     = uint32(2)
	MaxOutputJournalBytes   = 64 << 20
	outputJournalTempPrefix = ".wsresume-output-tmp-"
)

var outputJournalMagic = [8]byte{'W', 'S', 'O', 'U', 'T', 'P', 'U', 'T'}

type checkpointPhase uint8

const (
	checkpointDataWrite checkpointPhase = iota + 1
	checkpointDataFlush
	checkpointJournalWrite
	checkpointJournalFlush
	checkpointJournalInstall
	checkpointReopenVerify
)

type checkpointHook func(checkpointPhase) error

type outputJournalFile struct {
	Path           string      `json:"path"`
	Stage          string      `json:"stage"`
	FileID         string      `json:"fileId"`
	Revision       string      `json:"revision"`
	ExactSize      uint64      `json:"exactSize"`
	LocatorDigest  string      `json:"locatorDigest"`
	ObjectIdentity string      `json:"objectIdentity"`
	Generation     uint64      `json:"generation"`
	Ranges         [][2]uint64 `json:"ranges"`
	Published      bool        `json:"published"`
}

type outputJournalDocument struct {
	Schema        uint32              `json:"schema"`
	Backend       string              `json:"backend"`
	OutputSession string              `json:"outputSession"`
	ShareInstance string              `json:"shareInstance"`
	ResumeIntent  string              `json:"resumeIntent"`
	RootLocator   string              `json:"rootLocator"`
	RootIdentity  string              `json:"rootIdentity"`
	Files         []outputJournalFile `json:"files"`
}

type outputRootBinding struct {
	locator  string
	identity string
}

func bindOutputRoot(rootPath string, root *os.Root) (outputRootBinding, error) {
	if root == nil {
		return outputRootBinding{}, transfer.ErrInvalidOutputBinding
	}
	directory, err := root.Open(".")
	if err != nil {
		return outputRootBinding{}, err
	}
	platformErr := validateOutputRootPlatform(directory)
	locator, locatorErr := outputRootLocator(rootPath, directory)
	identity, identityErr := outputObjectIdentity(directory)
	closeErr := directory.Close()
	if platformErr != nil || locatorErr != nil || identityErr != nil || closeErr != nil {
		return outputRootBinding{}, errors.Join(platformErr, locatorErr, identityErr, closeErr)
	}
	digest := sha256.Sum256(append([]byte("windshare/output-root-path/v1\x00"), []byte(locator)...))
	return outputRootBinding{locator: encodeOutputBytes(digest[:]), identity: encodeOutputBytes(identity.Bytes())}, nil
}

func newOutputJournal(backend transfer.OutputBackendID, session transfer.OutputSessionID, share catalog.ShareInstance, intent OutputResumeIntent, root outputRootBinding) outputJournalDocument {
	return outputJournalDocument{
		Schema: outputJournalSchema, Backend: string(backend), OutputSession: encodeOutputBytes(session.Bytes()),
		ShareInstance: encodeOutputBytes(share.Bytes()), ResumeIntent: encodeOutputBytes(intent.Bytes()),
		RootLocator: root.locator, RootIdentity: root.identity,
		Files: make([]outputJournalFile, 0),
	}
}

func (d outputJournalDocument) clone() outputJournalDocument {
	clone := d
	clone.Files = make([]outputJournalFile, len(d.Files))
	for index, file := range d.Files {
		clone.Files[index] = file
		clone.Files[index].Ranges = slices.Clone(file.Ranges)
	}
	return clone
}

func (d *outputJournalDocument) file(path string) (*outputJournalFile, bool) {
	index, found := slices.BinarySearchFunc(d.Files, path, func(file outputJournalFile, target string) int {
		if file.Path < target {
			return -1
		}
		if file.Path > target {
			return 1
		}
		return 0
	})
	if !found {
		return nil, false
	}
	return &d.Files[index], true
}

func (d *outputJournalDocument) put(file outputJournalFile) {
	index, found := slices.BinarySearchFunc(d.Files, file.Path, func(current outputJournalFile, target string) int {
		if current.Path < target {
			return -1
		}
		if current.Path > target {
			return 1
		}
		return 0
	})
	if found {
		d.Files[index] = file
		return
	}
	d.Files = slices.Insert(d.Files, index, file)
}

func (d *outputJournalDocument) remove(path string) bool {
	index, found := slices.BinarySearchFunc(d.Files, path, func(file outputJournalFile, target string) int {
		if file.Path < target {
			return -1
		}
		if file.Path > target {
			return 1
		}
		return 0
	})
	if !found {
		return false
	}
	d.Files = slices.Delete(d.Files, index, index+1)
	return true
}

func journalFileFromBinding(binding transfer.OutputFileBinding, stage string, generation uint64, ranges content.RangeSet, published bool) outputJournalFile {
	encodedRanges := make([][2]uint64, 0, ranges.Len())
	for _, current := range ranges.Ranges() {
		encodedRanges = append(encodedRanges, [2]uint64{current.Offset, current.End})
	}
	return outputJournalFile{
		Path: binding.Locator().CanonicalPath(), Stage: stage, FileID: encodeOutputBytes(binding.FileID().Bytes()),
		Revision: encodeOutputBytes(binding.FileRevision().Bytes()), ExactSize: binding.ExactSize(),
		LocatorDigest:  encodeLocatorDigest(binding.Locator()),
		ObjectIdentity: encodeOutputBytes(binding.ObjectIdentity().Bytes()), Generation: generation,
		Ranges: encodedRanges, Published: published,
	}
}

func (f outputJournalFile) ranges() (content.RangeSet, error) {
	ranges := make([]content.Range, len(f.Ranges))
	for index, pair := range f.Ranges {
		ranges[index] = content.Range{Offset: pair[0], End: pair[1]}
	}
	return content.NewRangeSet(ranges)
}

func (f outputJournalFile) matches(binding transfer.OutputFileBinding) bool {
	return f.Path == binding.Locator().CanonicalPath() && f.FileID == encodeOutputBytes(binding.FileID().Bytes()) &&
		f.Revision == encodeOutputBytes(binding.FileRevision().Bytes()) && f.ExactSize == binding.ExactSize() &&
		f.LocatorDigest == encodeLocatorDigest(binding.Locator()) &&
		f.ObjectIdentity == encodeOutputBytes(binding.ObjectIdentity().Bytes())
}

func encodeLocatorDigest(locator transfer.OutputLocator) string {
	digest := locator.Digest()
	return encodeOutputBytes(digest[:])
}

func encodeOutputJournal(document outputJournalDocument) ([]byte, error) {
	if err := validateOutputJournal(document); err != nil {
		return nil, err
	}
	payload, err := json.Marshal(document)
	if err != nil {
		return nil, fmt.Errorf("encode output journal: %w", err)
	}
	total := len(outputJournalMagic) + len(payload) + sha256.Size
	if total > MaxOutputJournalBytes {
		return nil, ErrOutputJournalCorrupt
	}
	encoded := make([]byte, 0, total)
	encoded = append(encoded, outputJournalMagic[:]...)
	encoded = append(encoded, payload...)
	checksum := sha256.Sum256(encoded)
	encoded = append(encoded, checksum[:]...)
	return encoded, nil
}

func decodeOutputJournal(encoded []byte) (outputJournalDocument, error) {
	if len(encoded) < len(outputJournalMagic)+sha256.Size || len(encoded) > MaxOutputJournalBytes ||
		!bytes.Equal(encoded[:len(outputJournalMagic)], outputJournalMagic[:]) {
		return outputJournalDocument{}, ErrOutputJournalCorrupt
	}
	payloadEnd := len(encoded) - sha256.Size
	checksum := sha256.Sum256(encoded[:payloadEnd])
	if !bytes.Equal(checksum[:], encoded[payloadEnd:]) {
		return outputJournalDocument{}, ErrOutputJournalCorrupt
	}
	payload := encoded[len(outputJournalMagic):payloadEnd]
	decoder := json.NewDecoder(bytes.NewReader(payload))
	decoder.DisallowUnknownFields()
	var document outputJournalDocument
	if err := decoder.Decode(&document); err != nil || decoder.Decode(&struct{}{}) != io.EOF {
		return outputJournalDocument{}, ErrOutputJournalCorrupt
	}
	if err := validateOutputJournal(document); err != nil {
		return outputJournalDocument{}, err
	}
	canonical, err := json.Marshal(document)
	if err != nil || !bytes.Equal(canonical, payload) {
		return outputJournalDocument{}, ErrOutputJournalCorrupt
	}
	return document, nil
}

func validateOutputJournal(document outputJournalDocument) error {
	if err := validateOutputJournalHeader(document); err != nil {
		return err
	}
	seenStages := make(map[string]struct{}, len(document.Files))
	previousPath := ""
	for _, file := range document.Files {
		if err := validateOutputJournalFile(file, previousPath, seenStages); err != nil {
			return err
		}
		previousPath = file.Path
	}
	return nil
}

func validateOutputJournalHeader(document outputJournalDocument) error {
	if document.Schema != outputJournalSchema || document.Files == nil {
		return ErrOutputJournalCorrupt
	}
	if _, err := transfer.NewOutputBackendID(document.Backend); err != nil {
		return ErrOutputJournalCorrupt
	}
	sessionBytes, err := decodeOutputBytes(document.OutputSession, transfer.OutputSessionIdentityBytes)
	if err != nil {
		return err
	}
	if _, err := transfer.OutputSessionIDFromBytes(sessionBytes); err != nil {
		return ErrOutputJournalCorrupt
	}
	shareBytes, err := decodeOutputBytes(document.ShareInstance, catalog.IdentityBytes)
	if err != nil {
		return err
	}
	share, err := catalog.ShareInstanceFromBytes(shareBytes)
	if err != nil || share.IsZero() {
		return ErrOutputJournalCorrupt
	}
	intentBytes, err := decodeOutputBytes(document.ResumeIntent, OutputResumeIntentBytes)
	if err != nil {
		return err
	}
	intent, err := OutputResumeIntentFromBytes(intentBytes)
	if err != nil || intent.IsZero() {
		return ErrOutputJournalCorrupt
	}
	rootLocator, err := decodeOutputBytes(document.RootLocator, sha256.Size)
	if err != nil || bytes.Equal(rootLocator, make([]byte, sha256.Size)) {
		return ErrOutputJournalCorrupt
	}
	rootIdentity, err := decodeOutputBytes(document.RootIdentity, transfer.OutputObjectIdentityBytes)
	if err != nil {
		return err
	}
	if _, err := transfer.OutputObjectIdentityFromBytes(rootIdentity); err != nil {
		return ErrOutputJournalCorrupt
	}
	return nil
}

func validateOutputJournalFile(file outputJournalFile, previousPath string, seenStages map[string]struct{}) error {
	canonical, err := catalog.CanonicalPath(file.Path)
	if file.Ranges == nil || err != nil || canonical != file.Path || !validOutputStageName(file.Stage) ||
		(previousPath != "" && previousPath >= file.Path) || file.ExactSize > catalog.MaxFileSize {
		return ErrOutputJournalCorrupt
	}
	if _, duplicate := seenStages[file.Stage]; duplicate {
		return ErrOutputJournalCorrupt
	}
	seenStages[file.Stage] = struct{}{}
	if err := validateOutputJournalFileIdentity(file); err != nil {
		return err
	}
	return validateOutputJournalFileRanges(file)
}

func validateOutputJournalFileIdentity(file outputJournalFile) error {
	fileBytes, err := decodeOutputBytes(file.FileID, catalog.IdentityBytes)
	if err != nil {
		return err
	}
	fileID, err := catalog.FileIDFromBytes(fileBytes)
	if err != nil || fileID.IsZero() {
		return ErrOutputJournalCorrupt
	}
	revisionBytes, err := decodeOutputBytes(file.Revision, catalog.IdentityBytes)
	if err != nil {
		return err
	}
	revision, err := content.FileRevisionFromBytes(revisionBytes)
	if err != nil || revision.IsZero() {
		return ErrOutputJournalCorrupt
	}
	locatorBytes, err := decodeOutputBytes(file.LocatorDigest, sha256.Size)
	if err != nil || bytes.Equal(locatorBytes, make([]byte, sha256.Size)) {
		return ErrOutputJournalCorrupt
	}
	identityBytes, err := decodeOutputBytes(file.ObjectIdentity, transfer.OutputObjectIdentityBytes)
	if err != nil {
		return err
	}
	if _, err := transfer.OutputObjectIdentityFromBytes(identityBytes); err != nil {
		return ErrOutputJournalCorrupt
	}
	return nil
}

func validateOutputJournalFileRanges(file outputJournalFile) error {
	ranges, err := file.ranges()
	if err != nil {
		return ErrOutputJournalCorrupt
	}
	for _, current := range ranges.Ranges() {
		if current.End > file.ExactSize {
			return ErrOutputJournalCorrupt
		}
	}
	if file.Generation == 0 && (!ranges.IsEmpty() || file.Published) {
		return ErrOutputJournalCorrupt
	}
	if !file.Published && (file.Generation == math.MaxUint64 || (file.Generation > 0 && ranges.IsEmpty())) {
		return ErrOutputJournalCorrupt
	}
	if file.Published && !transfer.RangesCoverFile(file.ExactSize, ranges) {
		return ErrOutputJournalCorrupt
	}
	return nil
}

func persistOutputJournal(root *os.Root, journalName string, document outputJournalDocument, hook checkpointHook) (outputJournalDocument, error) {
	if root == nil {
		return outputJournalDocument{}, transfer.ErrInvalidOutputBinding
	}
	encoded, err := encodeOutputJournal(document)
	if err != nil {
		return outputJournalDocument{}, err
	}
	file, tempName, err := createOutputJournalTemp(root)
	if err != nil {
		return outputJournalDocument{}, err
	}
	removeTemp := true
	defer func() {
		if removeTemp {
			_ = root.Remove(tempName)
		}
	}()
	written, writeErr := file.Write(encoded)
	if writeErr == nil && written != len(encoded) {
		writeErr = io.ErrShortWrite
	}
	if writeErr != nil {
		return outputJournalDocument{}, errors.Join(writeErr, file.Close())
	}
	if hook != nil {
		if err := hook(checkpointJournalWrite); err != nil {
			_ = file.Close()
			return outputJournalDocument{}, err
		}
	}
	if err := file.Sync(); err != nil {
		return outputJournalDocument{}, errors.Join(err, file.Close())
	}
	if hook != nil {
		if err := hook(checkpointJournalFlush); err != nil {
			_ = file.Close()
			return outputJournalDocument{}, err
		}
	}
	if err := file.Close(); err != nil {
		return outputJournalDocument{}, err
	}
	if err := installOutputJournal(root, tempName, journalName); err != nil {
		return outputJournalDocument{}, err
	}
	removeTemp = false
	if hook != nil {
		if err := hook(checkpointJournalInstall); err != nil {
			return outputJournalDocument{}, err
		}
	}
	reopened, err := readOutputJournalAt(root, journalName)
	if err != nil || !reflect.DeepEqual(reopened, document) {
		return outputJournalDocument{}, errors.Join(ErrOutputJournalCorrupt, err)
	}
	if hook != nil {
		if err := hook(checkpointReopenVerify); err != nil {
			return outputJournalDocument{}, err
		}
	}
	return reopened, nil
}

type filesystemFileTransaction struct {
	session    *FilesystemOutputSession
	file       *os.File
	binding    transfer.OutputFileBinding
	stage      string
	modified   catalog.ModifiedTime
	durable    content.RangeSet
	pending    content.RangeSet
	generation uint64
	published  bool
	closed     bool
	mu         sync.Mutex
}

func (s *FilesystemOutputSession) resumeOutputBinding(output transfer.OutputFile, locator transfer.OutputLocator, entry outputJournalFile) (transfer.OutputObjectIdentity, transfer.OutputFileBinding, error) {
	identityBytes, err := decodeOutputBytes(entry.ObjectIdentity, transfer.OutputObjectIdentityBytes)
	if err != nil {
		return transfer.OutputObjectIdentity{}, transfer.OutputFileBinding{}, err
	}
	identity, err := transfer.OutputObjectIdentityFromBytes(identityBytes)
	if err != nil {
		return transfer.OutputObjectIdentity{}, transfer.OutputFileBinding{}, err
	}
	binding, err := transfer.NewOutputFileBinding(filesystemOutputBackendID, s.session, output.Descriptor, locator, identity)
	return identity, binding, err
}

func (s *FilesystemOutputSession) resumeOpenPath(relative string, entry outputJournalFile, identity transfer.OutputObjectIdentity, ranges content.RangeSet) (string, bool, bool) {
	info, err := s.root.Lstat(relative)
	if err == nil {
		if !info.Mode().IsRegular() || s.verifyOwnedPath(relative, identity, entry.ExactSize) != nil ||
			!transfer.RangesCoverFile(entry.ExactSize, ranges) {
			return "", false, false
		}
		return relative, true, true
	}
	if !errors.Is(err, fs.ErrNotExist) || entry.Published {
		return "", false, false
	}
	return entry.Stage, false, true
}

func (s *FilesystemOutputSession) openVerifiedOutput(path string, identity transfer.OutputObjectIdentity, size uint64) (*os.File, error) {
	file, err := s.root.OpenFile(path, os.O_RDWR, filePerm)
	if err != nil {
		return nil, err
	}
	if err := s.verifyFile(file, identity, size); err != nil {
		return nil, errors.Join(err, file.Close())
	}
	return file, nil
}

func (t *filesystemFileTransaction) Binding() transfer.OutputFileBinding { return t.binding }

func (t *filesystemFileTransaction) WriteRange(ctx context.Context, offset uint64, data []byte) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.closed || t.published || len(data) == 0 || offset > t.binding.ExactSize() || uint64(len(data)) > t.binding.ExactSize()-offset {
		return transfer.NewOutputSessionError(ErrOutOfRange, false)
	}
	written, err := t.file.WriteAt(data, int64(offset))
	if err == nil && written != len(data) {
		err = io.ErrShortWrite
	}
	if err != nil {
		return transfer.NewOutputSessionError(err, false)
	}
	writtenRange, _ := content.NewRangeSet([]content.Range{{Offset: offset, End: offset + uint64(len(data))}})
	t.pending, err = transfer.MergeRanges(t.pending, writtenRange)
	if err != nil {
		return transfer.NewOutputSessionError(err, false)
	}
	if t.session.hook != nil {
		return t.session.hook(checkpointDataWrite)
	}
	return nil
}

func (t *filesystemFileTransaction) Checkpoint(ctx context.Context) (transfer.VerifiedDurableRanges, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.checkpointLocked(ctx)
}

func (t *filesystemFileTransaction) checkpointLocked(ctx context.Context) (transfer.VerifiedDurableRanges, error) {
	if err := ctx.Err(); err != nil {
		return transfer.VerifiedDurableRanges{}, err
	}
	if t.closed {
		return transfer.VerifiedDurableRanges{}, transfer.NewOutputSessionError(ErrOutputSessionClosed, false)
	}
	if t.pending.IsEmpty() {
		return transfer.VerifyDurableRanges(t.binding, t.generation, t.durable)
	}
	if t.generation == math.MaxUint64 {
		return transfer.VerifiedDurableRanges{}, transfer.NewOutputSessionError(transfer.ErrOutputContract, true)
	}
	candidate, err := transfer.MergeRanges(t.durable, t.pending)
	if err != nil {
		return transfer.VerifiedDurableRanges{}, err
	}
	if err := t.file.Sync(); err != nil {
		return transfer.VerifiedDurableRanges{}, transfer.NewOutputSessionError(err, false)
	}
	if t.session.hook != nil {
		if err := t.session.hook(checkpointDataFlush); err != nil {
			return transfer.VerifiedDurableRanges{}, err
		}
	}
	nextGeneration := t.generation + 1
	if err := t.session.persistCheckpoint(t.binding, t.stage, nextGeneration, candidate, false); err != nil {
		return transfer.VerifiedDurableRanges{}, err
	}
	t.durable, t.pending, t.generation = candidate, content.RangeSet{}, nextGeneration
	return transfer.VerifyDurableRanges(t.binding, t.generation, t.durable)
}

func (t *filesystemFileTransaction) Commit(ctx context.Context) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.closed {
		return transfer.NewOutputSessionError(ErrOutputSessionClosed, false)
	}
	if t.published {
		var verifyErr error
		if t.file != nil {
			verifyErr = errors.Join(t.session.verifyFile(t.file, t.binding.ObjectIdentity(), t.binding.ExactSize()), t.file.Close())
			t.file = nil
		}
		verifyErr = errors.Join(verifyErr, t.session.verifyOwnedPath(filepath.FromSlash(t.binding.Locator().CanonicalPath()), t.binding.ObjectIdentity(), t.binding.ExactSize()))
		if verifyErr != nil {
			return transfer.NewOutputSessionError(verifyErr, true)
		}
		t.closed = true
		t.session.completeFile(t.binding.Locator().CanonicalPath(), t)
		return nil
	}
	if _, err := t.checkpointLocked(ctx); err != nil {
		return err
	}
	if !transfer.RangesCoverFile(t.binding.ExactSize(), t.durable) {
		return transfer.ErrIncompleteOutputFile
	}
	if err := t.file.Close(); err != nil {
		return err
	}
	t.file = nil
	if err := t.session.publishFile(t); err != nil {
		return transfer.NewOutputSessionError(err, true)
	}
	t.published, t.closed = true, true
	t.session.completeFile(t.binding.Locator().CanonicalPath(), t)
	return nil
}

func (t *filesystemFileTransaction) Abort(ctx context.Context, cause error) (transfer.FileAbortDisposition, error) {
	_ = ctx
	_ = cause
	t.mu.Lock()
	if t.closed {
		t.mu.Unlock()
		return transfer.FileAbortIsolated, nil
	}
	t.closed = true
	closeErr := error(nil)
	if t.file != nil {
		closeErr = t.file.Close()
		t.file = nil
	}
	t.mu.Unlock()
	if t.published {
		t.session.completeFile(t.binding.Locator().CanonicalPath(), t)
		return transfer.FileAbortIsolated, closeErr
	}
	return transfer.FileAbortIsolated, errors.Join(closeErr, t.session.abortFile(t.binding, t.stage))
}
