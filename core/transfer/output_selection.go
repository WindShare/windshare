package transfer

import (
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"slices"
	"sort"
	"strings"

	"github.com/windshare/windshare/core/catalog"
)

const SelectionIdentityBytes = sha256.Size

var ErrInvalidOutputSelection = errors.New("transfer output selection is invalid")

type SelectionIdentity [SelectionIdentityBytes]byte

func SelectionIdentityFromBytes(raw []byte) (SelectionIdentity, error) {
	if len(raw) != SelectionIdentityBytes {
		return SelectionIdentity{}, ErrInvalidOutputSelection
	}
	var identity SelectionIdentity
	copy(identity[:], raw)
	if identity.IsZero() {
		return SelectionIdentity{}, ErrInvalidOutputSelection
	}
	return identity, nil
}

func (identity SelectionIdentity) Bytes() []byte { return append([]byte(nil), identity[:]...) }
func (identity SelectionIdentity) IsZero() bool  { return identity == SelectionIdentity{} }

// OutputSelectionDirectory binds both the authenticated generation and metadata
// because either change alters the exact output plan represented by resume intent.
type OutputSelectionDirectory struct {
	Path         string
	DirectoryID  catalog.DirectoryID
	Generation   catalog.DirectoryGeneration
	ModifiedTime catalog.ModifiedTime
}

// OutputSelectionFile binds a canonical output locator to the catalog generation
// that authenticated the file entry. The later revision lease remains a distinct
// binding because output admission must finish before OpenRevision is allowed.
type OutputSelectionFile struct {
	Path              string
	FileID            catalog.FileID
	ParentDirectoryID catalog.DirectoryID
	ParentGeneration  catalog.DirectoryGeneration
	ExpectedSize      uint64
	ModifiedTime      catalog.ModifiedTime
}

type OutputSelection struct {
	identity       SelectionIdentity
	resumeIntent   ResumeIntent
	canonical      CanonicalSelectionV1
	share          catalog.ShareInstance
	root           catalog.DirectoryID
	rootGeneration catalog.DirectoryGeneration
	plan           outputSelectionPlan
}

type outputSelectionPlan interface {
	DirectoryCount() uint64
	FileCount() uint64
	VisitRecords(func(selectionPlanRecord) error) error
}

type memoryOutputSelectionPlan struct {
	records     []selectionPlanRecord
	directories uint64
	files       uint64
}

func (plan *memoryOutputSelectionPlan) DirectoryCount() uint64 { return plan.directories }
func (plan *memoryOutputSelectionPlan) FileCount() uint64      { return plan.files }
func (plan *memoryOutputSelectionPlan) VisitRecords(visit func(selectionPlanRecord) error) error {
	for _, record := range plan.records {
		if err := visit(record); err != nil {
			return err
		}
	}
	return nil
}

func NewOutputSelection(
	share catalog.ShareInstance,
	root catalog.DirectoryID,
	rootGeneration catalog.DirectoryGeneration,
	directories []OutputSelectionDirectory,
	files []OutputSelectionFile,
) (OutputSelection, error) {
	if share.IsZero() || root.IsZero() || rootGeneration.IsZero() ||
		uint64(len(directories))+uint64(len(files)) > maximumSelectionClaims {
		return OutputSelection{}, ErrInvalidOutputSelection
	}
	plan, err := buildMemoryOutputSelectionPlan(root, rootGeneration, directories, files)
	if err != nil {
		return OutputSelection{}, err
	}
	return newOutputSelectionFromPlan(share, root, rootGeneration, plan)
}

type outputSelectionClaimSet struct {
	paths map[string]struct{}
	nodes map[catalog.NodeID]struct{}
}

func newOutputSelectionClaimSet(root catalog.DirectoryID, capacity int) outputSelectionClaimSet {
	return outputSelectionClaimSet{
		paths: make(map[string]struct{}, capacity),
		nodes: map[catalog.NodeID]struct{}{root.NodeID(): {}},
	}
}

func (claims *outputSelectionClaimSet) add(path string, node catalog.NodeID) error {
	if !validSelectionPath(path) || node.IsZero() {
		return ErrInvalidOutputSelection
	}
	if _, duplicate := claims.nodes[node]; duplicate {
		return ErrInvalidOutputSelection
	}
	if _, duplicate := claims.paths[path]; duplicate {
		return ErrInvalidOutputSelection
	}
	claims.nodes[node] = struct{}{}
	claims.paths[path] = struct{}{}
	return nil
}

func buildMemoryOutputSelectionPlan(
	root catalog.DirectoryID,
	rootGeneration catalog.DirectoryGeneration,
	directories []OutputSelectionDirectory,
	files []OutputSelectionFile,
) (*memoryOutputSelectionPlan, error) {
	ownedDirectories := slices.Clone(directories)
	ownedFiles := slices.Clone(files)
	sort.Slice(ownedDirectories, func(left, right int) bool {
		return ownedDirectories[left].Path < ownedDirectories[right].Path
	})
	sort.Slice(ownedFiles, func(left, right int) bool {
		return ownedFiles[left].Path < ownedFiles[right].Path
	})
	claims := newOutputSelectionClaimSet(root, len(ownedDirectories)+len(ownedFiles))
	directoryByPath, err := indexOutputSelectionDirectories(ownedDirectories, &claims)
	if err != nil {
		return nil, err
	}
	if err := validateOutputSelectionFiles(
		root, rootGeneration, ownedFiles, directoryByPath, &claims,
	); err != nil {
		return nil, err
	}
	if err := validateOutputSelectionDirectoryParents(ownedDirectories, directoryByPath); err != nil {
		return nil, err
	}
	return &memoryOutputSelectionPlan{
		records:     buildOutputSelectionRecords(ownedDirectories, ownedFiles),
		directories: uint64(len(ownedDirectories)), files: uint64(len(ownedFiles)),
	}, nil
}

func indexOutputSelectionDirectories(
	directories []OutputSelectionDirectory,
	claims *outputSelectionClaimSet,
) (map[string]OutputSelectionDirectory, error) {
	directoryByPath := make(map[string]OutputSelectionDirectory, len(directories))
	for _, directory := range directories {
		if directory.Generation.IsZero() {
			return nil, ErrInvalidOutputSelection
		}
		if err := claims.add(directory.Path, directory.DirectoryID.NodeID()); err != nil {
			return nil, err
		}
		directoryByPath[directory.Path] = directory
	}
	return directoryByPath, nil
}

func validateOutputSelectionFiles(
	root catalog.DirectoryID,
	rootGeneration catalog.DirectoryGeneration,
	files []OutputSelectionFile,
	directoryByPath map[string]OutputSelectionDirectory,
	claims *outputSelectionClaimSet,
) error {
	for _, file := range files {
		if file.ParentDirectoryID.IsZero() || file.ParentGeneration.IsZero() ||
			file.ExpectedSize > catalog.MaxFileSize {
			return ErrInvalidOutputSelection
		}
		if err := claims.add(file.Path, file.FileID.NodeID()); err != nil {
			return err
		}
		if err := validateOutputSelectionFileParent(
			root, rootGeneration, file, directoryByPath,
		); err != nil {
			return err
		}
	}
	return nil
}

func validateOutputSelectionFileParent(
	root catalog.DirectoryID,
	rootGeneration catalog.DirectoryGeneration,
	file OutputSelectionFile,
	directoryByPath map[string]OutputSelectionDirectory,
) error {
	parentPath := selectionParentPath(file.Path)
	if parentPath == "" {
		if file.ParentDirectoryID != root || file.ParentGeneration != rootGeneration {
			return ErrInvalidOutputSelection
		}
		return nil
	}
	parent, ok := directoryByPath[parentPath]
	if !ok || parent.DirectoryID != file.ParentDirectoryID || parent.Generation != file.ParentGeneration {
		return ErrInvalidOutputSelection
	}
	return nil
}

func validateOutputSelectionDirectoryParents(
	directories []OutputSelectionDirectory,
	directoryByPath map[string]OutputSelectionDirectory,
) error {
	for _, directory := range directories {
		parentPath := selectionParentPath(directory.Path)
		if parentPath == "" {
			continue
		}
		if _, ok := directoryByPath[parentPath]; !ok {
			return ErrInvalidOutputSelection
		}
	}
	return nil
}

func buildOutputSelectionRecords(
	directories []OutputSelectionDirectory,
	files []OutputSelectionFile,
) []selectionPlanRecord {
	records := make([]selectionPlanRecord, 0, len(directories)+len(files))
	for _, directory := range directories {
		records = append(records, selectionPlanRecord{
			kind: selectionPlanDirectoryKind, active: true, path: directory.Path,
			directory: plannedDirectory{
				directory: directory.DirectoryID, generation: directory.Generation,
				path: directory.Path, modified: directory.ModifiedTime,
			},
		})
	}
	for _, file := range files {
		records = append(records, selectionPlanRecord{
			kind: selectionPlanFileKind, active: true, path: file.Path,
			file: plannedFile{
				file: file.FileID, path: file.Path, expectedSize: file.ExpectedSize,
				modified: file.ModifiedTime, parentDirectory: file.ParentDirectoryID,
				parentGeneration: file.ParentGeneration,
			},
		})
	}
	sort.Slice(records, func(left, right int) bool {
		if records[left].path != records[right].path {
			return records[left].path < records[right].path
		}
		return records[left].kind < records[right].kind
	})
	return records
}

func newOutputSelectionFromPlan(
	share catalog.ShareInstance,
	root catalog.DirectoryID,
	rootGeneration catalog.DirectoryGeneration,
	plan outputSelectionPlan,
) (OutputSelection, error) {
	if share.IsZero() || root.IsZero() || rootGeneration.IsZero() || plan == nil ||
		plan.DirectoryCount()+plan.FileCount() > maximumSelectionClaims {
		return OutputSelection{}, ErrInvalidOutputSelection
	}
	if err := validateOutputSelectionPlan(root, rootGeneration, plan); err != nil {
		return OutputSelection{}, err
	}
	selection := OutputSelection{
		share: share, root: root, rootGeneration: rootGeneration, plan: plan,
	}
	identity, err := hashOutputSelection(selection)
	if err != nil {
		return OutputSelection{}, err
	}
	selection.identity = identity
	return selection, nil
}

type outputSelectionDirectoryAuthority struct {
	path       string
	directory  catalog.DirectoryID
	generation catalog.DirectoryGeneration
}

type outputSelectionPlanValidation struct {
	root           catalog.DirectoryID
	rootGeneration catalog.DirectoryGeneration
	directories    uint64
	files          uint64
	previousPath   string
	ancestry       []outputSelectionDirectoryAuthority
}

func validateOutputSelectionPlan(
	root catalog.DirectoryID,
	rootGeneration catalog.DirectoryGeneration,
	plan outputSelectionPlan,
) error {
	validation := outputSelectionPlanValidation{root: root, rootGeneration: rootGeneration}
	if err := plan.VisitRecords(validation.validateRecord); err != nil {
		return err
	}
	if validation.directories != plan.DirectoryCount() || validation.files != plan.FileCount() {
		return ErrInvalidOutputSelection
	}
	return nil
}

func (validation *outputSelectionPlanValidation) validateRecord(record selectionPlanRecord) error {
	parentPath, err := validation.acceptRecordPath(record)
	if err != nil {
		return err
	}
	switch record.kind {
	case selectionPlanDirectoryKind:
		return validation.validateDirectory(record)
	case selectionPlanFileKind:
		return validation.validateFile(record, parentPath)
	default:
		return ErrInvalidOutputSelection
	}
}

func (validation *outputSelectionPlanValidation) acceptRecordPath(
	record selectionPlanRecord,
) (string, error) {
	if !record.active || !validSelectionPath(record.path) ||
		(validation.previousPath != "" && record.path <= validation.previousPath) {
		return "", ErrInvalidOutputSelection
	}
	validation.previousPath = record.path
	parentPath := selectionParentPath(record.path)
	for len(validation.ancestry) > 0 &&
		validation.ancestry[len(validation.ancestry)-1].path != parentPath {
		validation.ancestry = validation.ancestry[:len(validation.ancestry)-1]
	}
	if parentPath != "" && len(validation.ancestry) == 0 {
		return "", ErrInvalidOutputSelection
	}
	return parentPath, nil
}

func (validation *outputSelectionPlanValidation) validateDirectory(
	record selectionPlanRecord,
) error {
	if record.directory.path != record.path || record.directory.directory.IsZero() ||
		record.directory.generation.IsZero() {
		return ErrInvalidOutputSelection
	}
	validation.directories++
	validation.ancestry = append(validation.ancestry, outputSelectionDirectoryAuthority{
		path: record.path, directory: record.directory.directory,
		generation: record.directory.generation,
	})
	return nil
}

func (validation *outputSelectionPlanValidation) validateFile(
	record selectionPlanRecord,
	parentPath string,
) error {
	if record.file.path != record.path || record.file.file.IsZero() ||
		record.file.parentDirectory.IsZero() || record.file.parentGeneration.IsZero() ||
		record.file.expectedSize > catalog.MaxFileSize {
		return ErrInvalidOutputSelection
	}
	if err := validation.validateFileParent(record.file, parentPath); err != nil {
		return err
	}
	validation.files++
	return nil
}

func (validation *outputSelectionPlanValidation) validateFileParent(
	file plannedFile,
	parentPath string,
) error {
	if parentPath == "" {
		if file.parentDirectory != validation.root || file.parentGeneration != validation.rootGeneration {
			return ErrInvalidOutputSelection
		}
		return nil
	}
	parent := validation.ancestry[len(validation.ancestry)-1]
	if file.parentDirectory != parent.directory || file.parentGeneration != parent.generation {
		return ErrInvalidOutputSelection
	}
	return nil
}

func (selection OutputSelection) Identity() SelectionIdentity { return selection.identity }
func (selection OutputSelection) ResumeIntent() ResumeIntent  { return selection.resumeIntent }
func (selection OutputSelection) CanonicalSelection() CanonicalSelectionV1 {
	return selection.canonical
}
func (selection OutputSelection) ShareInstance() catalog.ShareInstance { return selection.share }
func (selection OutputSelection) SyntheticRoot() catalog.DirectoryID   { return selection.root }
func (selection OutputSelection) RootGeneration() catalog.DirectoryGeneration {
	return selection.rootGeneration
}
func (selection OutputSelection) Directories() []OutputSelectionDirectory {
	result := make([]OutputSelectionDirectory, 0, boundedSelectionCapacity(selection.DirectoryCount()))
	if err := selection.VisitDirectories(func(directory OutputSelectionDirectory) error {
		result = append(result, directory)
		return nil
	}); err != nil {
		panic(err)
	}
	return result
}
func (selection OutputSelection) Files() []OutputSelectionFile {
	result := make([]OutputSelectionFile, 0, boundedSelectionCapacity(selection.FileCount()))
	if err := selection.VisitFiles(func(file OutputSelectionFile) error {
		result = append(result, file)
		return nil
	}); err != nil {
		panic(err)
	}
	return result
}

func (selection OutputSelection) DirectoryCount() uint64 {
	if selection.plan == nil {
		return 0
	}
	return selection.plan.DirectoryCount()
}

func (selection OutputSelection) FileCount() uint64 {
	if selection.plan == nil {
		return 0
	}
	return selection.plan.FileCount()
}

func (selection OutputSelection) VisitDirectories(
	visit func(OutputSelectionDirectory) error,
) error {
	if selection.plan == nil || visit == nil {
		return ErrInvalidOutputSelection
	}
	return selection.plan.VisitRecords(func(record selectionPlanRecord) error {
		if record.kind != selectionPlanDirectoryKind {
			return nil
		}
		return visit(OutputSelectionDirectory{
			Path: record.path, DirectoryID: record.directory.directory,
			Generation: record.directory.generation, ModifiedTime: record.directory.modified,
		})
	})
}

func (selection OutputSelection) VisitFiles(visit func(OutputSelectionFile) error) error {
	if selection.plan == nil || visit == nil {
		return ErrInvalidOutputSelection
	}
	return selection.plan.VisitRecords(func(record selectionPlanRecord) error {
		if record.kind != selectionPlanFileKind {
			return nil
		}
		return visit(OutputSelectionFile{
			Path: record.path, FileID: record.file.file,
			ParentDirectoryID: record.file.parentDirectory,
			ParentGeneration:  record.file.parentGeneration,
			ExpectedSize:      record.file.expectedSize, ModifiedTime: record.file.modified,
		})
	})
}

func boundedSelectionCapacity(count uint64) int {
	if count > uint64(^uint(0)>>1) {
		return 0
	}
	return int(count)
}

func validSelectionPath(path string) bool {
	if path == "" {
		return false
	}
	canonical, err := catalog.CanonicalPath(path)
	return err == nil && canonical == path
}

func selectionParentPath(path string) string {
	index := strings.LastIndexByte(path, '/')
	if index < 0 {
		return ""
	}
	return path[:index]
}

func hashOutputSelection(selection OutputSelection) (SelectionIdentity, error) {
	hash := sha256.New()
	writeSelectionBytes(hash, []byte("windshare/output-selection/v1"))
	writeSelectionBytes(hash, selection.share.Bytes())
	writeSelectionBytes(hash, selection.root.Bytes())
	writeSelectionBytes(hash, selection.rootGeneration.Bytes())
	var count [8]byte
	binary.BigEndian.PutUint64(count[:], selection.DirectoryCount()+selection.FileCount())
	_, _ = hash.Write(count[:])
	err := selection.plan.VisitRecords(func(record selectionPlanRecord) error {
		if record.kind == selectionPlanDirectoryKind {
			_, _ = hash.Write([]byte{1})
			writeSelectionBytes(hash, []byte(record.path))
			writeSelectionBytes(hash, record.directory.directory.Bytes())
			writeSelectionBytes(hash, record.directory.generation.Bytes())
			writeSelectionModifiedTime(hash, record.directory.modified)
			return nil
		}
		_, _ = hash.Write([]byte{2})
		writeSelectionBytes(hash, []byte(record.path))
		writeSelectionBytes(hash, record.file.file.Bytes())
		writeSelectionBytes(hash, record.file.parentDirectory.Bytes())
		writeSelectionBytes(hash, record.file.parentGeneration.Bytes())
		var size [8]byte
		binary.BigEndian.PutUint64(size[:], record.file.expectedSize)
		_, _ = hash.Write(size[:])
		writeSelectionModifiedTime(hash, record.file.modified)
		return nil
	})
	if err != nil {
		return SelectionIdentity{}, err
	}
	var identity SelectionIdentity
	copy(identity[:], hash.Sum(nil))
	return identity, nil
}

type selectionHash interface {
	Write([]byte) (int, error)
}

func writeSelectionBytes(hash selectionHash, value []byte) {
	var length [8]byte
	binary.BigEndian.PutUint64(length[:], uint64(len(value)))
	_, _ = hash.Write(length[:])
	_, _ = hash.Write(value)
}

func writeSelectionModifiedTime(hash selectionHash, modified catalog.ModifiedTime) {
	present := byte(0)
	if modified.Present() {
		present = 1
	}
	_, _ = hash.Write([]byte{present})
	var seconds [8]byte
	binary.BigEndian.PutUint64(seconds[:], uint64(modified.Seconds()))
	_, _ = hash.Write(seconds[:])
	var nanoseconds [4]byte
	binary.BigEndian.PutUint32(nanoseconds[:], modified.Nanoseconds())
	_, _ = hash.Write(nanoseconds[:])
	_, _ = hash.Write([]byte{byte(modified.Precision())})
}
