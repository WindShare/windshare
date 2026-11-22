package receivecontract

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"io"
	"strings"
	"unicode/utf8"

	"github.com/windshare/windshare/core/catalog"
)

const (
	artifactSpecDomain     = "windshare/artifact-spec/v1"
	resultRootLayoutDomain = "windshare/result-root-layout/v1"

	DefaultResultRootName   = "windshare"
	PartialSelectionSuffix  = "-selection"
	ResultNamePolicy        = "windshare/result-name/v2-unicode-15.0.0"
	MaxResultComponentBytes = catalog.MaxNameBytes
	CollisionSuffixHexChars = 10
)

type ArtifactKind uint8

const (
	ArtifactDirectoryTree ArtifactKind = 1
)

type DirectoryTreeLayoutKind uint8

const (
	DirectoryTreeSingleFile DirectoryTreeLayoutKind = iota + 1
	DirectoryTreeResultRoot
	DirectoryTreeCatalogRoot
)

type ResultRootClass uint8

const (
	ResultRootCompleteDirectory ResultRootClass = iota + 1
	ResultRootDirectorySelection
	ResultRootSyntheticSelection
)

type ResultRootAnchorKind uint8

const (
	ResultRootDirectoryAnchor ResultRootAnchorKind = iota + 1
	ResultRootSyntheticAnchor
)

type OriginalFileSpec struct {
	FileID        catalog.FileID
	SourcePath    string
	SuggestedName string
}

type DirectoryTreeLayout struct {
	kind       DirectoryTreeLayoutKind
	singleFile OriginalFileSpec
	resultRoot ResultRootLayout
}

type ResultRootLayout struct {
	class       ResultRootClass
	anchorKind  ResultRootAnchorKind
	directoryID catalog.DirectoryID
	sourcePath  string
	name        string
	valid       bool
}

type ArtifactSpec struct {
	kind      ArtifactKind
	directory DirectoryTreeLayout
	digest    ArtifactDigest
	valid     bool
}

func NewSingleFileDirectoryTree(file catalog.FileID, sourcePath, outputName string) (ArtifactSpec, error) {
	canonicalPath, leaf, err := canonicalSourcePath(sourcePath)
	if err != nil || file.IsZero() || outputName != leaf || canonicalComponent(outputName) != nil {
		return ArtifactSpec{}, ErrInvalidReceiveContract
	}
	layout := DirectoryTreeLayout{
		kind:       DirectoryTreeSingleFile,
		singleFile: OriginalFileSpec{FileID: file, SourcePath: canonicalPath, SuggestedName: outputName},
	}
	return newDirectoryTreeArtifact(layout)
}

func NewResultRootDirectoryTree(layout ResultRootLayout) (ArtifactSpec, error) {
	if !layout.valid {
		return ArtifactSpec{}, ErrInvalidReceiveContract
	}
	return newDirectoryTreeArtifact(DirectoryTreeLayout{kind: DirectoryTreeResultRoot, resultRoot: layout})
}

func NewCatalogRootDirectoryTree() ArtifactSpec {
	result, err := newDirectoryTreeArtifact(DirectoryTreeLayout{kind: DirectoryTreeCatalogRoot})
	if err != nil {
		panic(err)
	}
	return result
}

func newDirectoryTreeArtifact(layout DirectoryTreeLayout) (ArtifactSpec, error) {
	if !layout.valid() {
		return ArtifactSpec{}, ErrInvalidReceiveContract
	}
	sum := digest(canonicalRecord(
		artifactSpecDomain,
		[]byte{byte(ArtifactDirectoryTree)},
		frame(layout.digestBytes()),
	))
	return ArtifactSpec{
		kind:      ArtifactDirectoryTree,
		directory: layout,
		digest:    ArtifactDigest(sum),
		valid:     true,
	}, nil
}

func NewCompleteDirectoryResultRoot(directory catalog.DirectoryID, sourcePath string) (ResultRootLayout, error) {
	return newDirectoryResultRoot(ResultRootCompleteDirectory, directory, sourcePath)
}

func NewDirectorySelectionResultRoot(directory catalog.DirectoryID, sourcePath string) (ResultRootLayout, error) {
	return newDirectoryResultRoot(ResultRootDirectorySelection, directory, sourcePath)
}

func NewSyntheticSelectionResultRoot() ResultRootLayout {
	result, err := newResultRootLayout(
		ResultRootSyntheticSelection, ResultRootSyntheticAnchor,
		catalog.DirectoryID{}, "", DefaultResultRootName,
	)
	if err != nil {
		panic(err)
	}
	return result
}

func newDirectoryResultRoot(class ResultRootClass, directory catalog.DirectoryID, sourcePath string) (ResultRootLayout, error) {
	canonicalPath, leaf, err := canonicalSourcePath(sourcePath)
	if err != nil || directory.IsZero() {
		return ResultRootLayout{}, ErrInvalidReceiveContract
	}
	name := leaf
	if class == ResultRootDirectorySelection {
		name, err = AppendProtectedSuffix(leaf, PartialSelectionSuffix)
	}
	if err != nil {
		return ResultRootLayout{}, err
	}
	return newResultRootLayout(class, ResultRootDirectoryAnchor, directory, canonicalPath, name)
}

func newResultRootLayout(
	class ResultRootClass,
	anchorKind ResultRootAnchorKind,
	directory catalog.DirectoryID,
	sourcePath string,
	name string,
) (ResultRootLayout, error) {
	if canonicalComponent(name) != nil {
		return ResultRootLayout{}, ErrInvalidReceiveContract
	}
	switch {
	case class == ResultRootSyntheticSelection && anchorKind == ResultRootSyntheticAnchor &&
		directory.IsZero() && sourcePath == "" && name == DefaultResultRootName:
	case (class == ResultRootCompleteDirectory || class == ResultRootDirectorySelection) &&
		anchorKind == ResultRootDirectoryAnchor && !directory.IsZero():
		canonicalPath, leaf, err := canonicalSourcePath(sourcePath)
		if err != nil || canonicalPath != sourcePath {
			return ResultRootLayout{}, ErrInvalidReceiveContract
		}
		expected := leaf
		if class == ResultRootDirectorySelection {
			expected, err = AppendProtectedSuffix(leaf, PartialSelectionSuffix)
		}
		if err != nil || name != expected {
			return ResultRootLayout{}, ErrInvalidReceiveContract
		}
	default:
		return ResultRootLayout{}, ErrInvalidReceiveContract
	}
	return ResultRootLayout{
		class: class, anchorKind: anchorKind, directoryID: directory,
		sourcePath: sourcePath, name: name, valid: true,
	}, nil
}

func AppendProtectedSuffix(base, suffix string) (string, error) {
	if canonicalComponent(base) != nil || suffix == "" || !utf8.ValidString(suffix) {
		return "", ErrInvalidReceiveContract
	}
	maximumBaseBytes := MaxResultComponentBytes - len([]byte(suffix))
	if maximumBaseBytes <= 0 {
		return "", ErrInvalidReceiveContract
	}
	truncated := resultNamePrefix(base, maximumBaseBytes)
	if truncated == "" {
		return "", ErrInvalidReceiveContract
	}
	result := truncated + suffix
	if canonicalComponent(result) != nil {
		return "", ErrInvalidReceiveContract
	}
	return result, nil
}

// Result names are already valid UTF-8; truncation must not split a scalar.
func resultNamePrefix(value string, maximumBytes int) string {
	if maximumBytes <= 0 {
		return ""
	}
	if len(value) <= maximumBytes {
		return value
	}
	end := maximumBytes
	for end > 0 && !utf8.RuneStart(value[end]) {
		end--
	}
	return value[:end]
}

func canonicalSourcePath(value string) (string, string, error) {
	canonical, err := catalog.CanonicalPath(value)
	if err != nil || canonical != value {
		return "", "", ErrInvalidReceiveContract
	}
	leaf := canonical
	if _, cutLeaf, found := strings.CutLast(canonical, "/"); found {
		leaf = cutLeaf
	}
	if canonicalComponent(leaf) != nil {
		return "", "", ErrInvalidReceiveContract
	}
	return canonical, leaf, nil
}

func canonicalComponent(value string) error {
	canonical, err := catalog.CanonicalName(value)
	if err != nil || canonical != value || len([]byte(value)) > MaxResultComponentBytes {
		return ErrInvalidReceiveContract
	}
	return nil
}

func canonicalPathBytes(path string) []byte {
	segments := strings.Split(path, "/")
	encoded := uint64Bytes(uint64(len(segments)))
	for _, segment := range segments {
		encoded = append(encoded, frame([]byte(segment))...)
	}
	return encoded
}

func (layout ResultRootLayout) digestBytes() []byte {
	anchor := []byte{byte(layout.anchorKind)}
	if layout.anchorKind == ResultRootDirectoryAnchor {
		anchor = append(anchor, frame(layout.directoryID.Bytes())...)
		anchor = append(anchor, frame(canonicalPathBytes(layout.sourcePath))...)
	}
	return canonicalRecord(resultRootLayoutDomain,
		frame([]byte{byte(layout.class)}), frame(anchor), frame([]byte(layout.name)),
	)
}

func (layout DirectoryTreeLayout) digestBytes() []byte {
	encoded := []byte{byte(layout.kind)}
	switch layout.kind {
	case DirectoryTreeSingleFile:
		encoded = append(encoded, frame(layout.singleFile.FileID.Bytes())...)
		encoded = append(encoded, frame(canonicalPathBytes(layout.singleFile.SourcePath))...)
		encoded = append(encoded, frame([]byte(layout.singleFile.SuggestedName))...)
	case DirectoryTreeResultRoot:
		encoded = append(encoded, frame(layout.resultRoot.digestBytes())...)
	case DirectoryTreeCatalogRoot:
	}
	return encoded
}

func (layout DirectoryTreeLayout) valid() bool {
	switch layout.kind {
	case DirectoryTreeSingleFile:
		path, leaf, err := canonicalSourcePath(layout.singleFile.SourcePath)
		return err == nil && path == layout.singleFile.SourcePath && !layout.singleFile.FileID.IsZero() &&
			leaf == layout.singleFile.SuggestedName
	case DirectoryTreeResultRoot:
		return layout.resultRoot.valid
	case DirectoryTreeCatalogRoot:
		return layout.singleFile == (OriginalFileSpec{}) && !layout.resultRoot.valid
	default:
		return false
	}
}

func (artifact ArtifactSpec) Kind() ArtifactKind                 { return artifact.kind }
func (artifact ArtifactSpec) Digest() ArtifactDigest             { return artifact.digest }
func (artifact ArtifactSpec) IsZero() bool                       { return !artifact.valid }
func (layout DirectoryTreeLayout) Kind() DirectoryTreeLayoutKind { return layout.kind }
func (layout ResultRootLayout) Class() ResultRootClass           { return layout.class }
func (layout ResultRootLayout) AnchorKind() ResultRootAnchorKind { return layout.anchorKind }
func (layout ResultRootLayout) DirectoryID() catalog.DirectoryID { return layout.directoryID }
func (layout ResultRootLayout) SourcePath() string               { return layout.sourcePath }
func (layout ResultRootLayout) Name() string                     { return layout.name }

func (artifact ArtifactSpec) DirectoryTree() (DirectoryTreeLayout, bool) {
	return artifact.directory, artifact.valid && artifact.kind == ArtifactDirectoryTree
}

func (layout DirectoryTreeLayout) SingleFile() (OriginalFileSpec, bool) {
	return layout.singleFile, layout.valid() && layout.kind == DirectoryTreeSingleFile
}

func (layout DirectoryTreeLayout) ResultRoot() (ResultRootLayout, bool) {
	return layout.resultRoot, layout.valid() && layout.kind == DirectoryTreeResultRoot
}

type artifactSpecJSON struct {
	Layout     string          `json:"layout"`
	SingleFile *singleFileJSON `json:"single_file,omitempty"`
	ResultRoot *resultRootJSON `json:"result_root,omitempty"`
}

type singleFileJSON struct {
	FileID        string `json:"file_id"`
	SourcePath    string `json:"source_path"`
	SuggestedName string `json:"suggested_name"`
}

type resultRootJSON struct {
	Class       string `json:"class"`
	DirectoryID string `json:"directory_id,omitempty"`
	SourcePath  string `json:"source_path,omitempty"`
}

func (artifact ArtifactSpec) MarshalJSON() ([]byte, error) {
	if artifact.IsZero() {
		return nil, ErrInvalidReceiveContract
	}
	var payload artifactSpecJSON
	switch artifact.directory.kind {
	case DirectoryTreeSingleFile:
		single := artifact.directory.singleFile
		payload.Layout = "single_file"
		payload.SingleFile = &singleFileJSON{
			FileID:        hex.EncodeToString(single.FileID.Bytes()),
			SourcePath:    single.SourcePath,
			SuggestedName: single.SuggestedName,
		}
	case DirectoryTreeResultRoot:
		root := artifact.directory.resultRoot
		payload.Layout = "result_root"
		var className string
		switch root.class {
		case ResultRootCompleteDirectory:
			className = "complete_directory"
		case ResultRootDirectorySelection:
			className = "directory_selection"
		case ResultRootSyntheticSelection:
			className = "synthetic_selection"
		default:
			return nil, ErrInvalidReceiveContract
		}
		dto := &resultRootJSON{Class: className, SourcePath: root.sourcePath}
		if !root.directoryID.IsZero() {
			dto.DirectoryID = hex.EncodeToString(root.directoryID.Bytes())
		}
		payload.ResultRoot = dto
	case DirectoryTreeCatalogRoot:
		payload.Layout = "catalog_root"
	default:
		return nil, ErrInvalidReceiveContract
	}
	return json.Marshal(payload)
}

func (artifact *ArtifactSpec) UnmarshalJSON(data []byte) error {
	var payload artifactSpecJSON
	if err := decodeStrictJSON(data, &payload); err != nil {
		return ErrInvalidReceiveContract
	}
	var rebuilt ArtifactSpec
	var err error
	switch payload.Layout {
	case "single_file":
		rebuilt, err = unmarshalSingleFileArtifact(payload)
	case "result_root":
		rebuilt, err = unmarshalResultRootArtifact(payload)
	case "catalog_root":
		if payload.SingleFile != nil || payload.ResultRoot != nil {
			return ErrInvalidReceiveContract
		}
		rebuilt = NewCatalogRootDirectoryTree()
	default:
		return ErrInvalidReceiveContract
	}
	if err != nil {
		return err
	}
	*artifact = rebuilt
	return nil
}

func unmarshalSingleFileArtifact(payload artifactSpecJSON) (ArtifactSpec, error) {
	if payload.SingleFile == nil || payload.ResultRoot != nil {
		return ArtifactSpec{}, ErrInvalidReceiveContract
	}
	rawID, err := hex.DecodeString(payload.SingleFile.FileID)
	if err != nil {
		return ArtifactSpec{}, ErrInvalidReceiveContract
	}
	fileID, err := catalog.FileIDFromBytes(rawID)
	if err != nil {
		return ArtifactSpec{}, ErrInvalidReceiveContract
	}
	return NewSingleFileDirectoryTree(fileID, payload.SingleFile.SourcePath, payload.SingleFile.SuggestedName)
}

func unmarshalResultRootArtifact(payload artifactSpecJSON) (ArtifactSpec, error) {
	if payload.ResultRoot == nil || payload.SingleFile != nil {
		return ArtifactSpec{}, ErrInvalidReceiveContract
	}
	layout, err := unmarshalResultRootLayout(payload.ResultRoot)
	if err != nil {
		return ArtifactSpec{}, err
	}
	return NewResultRootDirectoryTree(layout)
}

func unmarshalResultRootLayout(dto *resultRootJSON) (ResultRootLayout, error) {
	switch dto.Class {
	case "complete_directory", "directory_selection":
		rawID, err := hex.DecodeString(dto.DirectoryID)
		if err != nil {
			return ResultRootLayout{}, ErrInvalidReceiveContract
		}
		dirID, err := catalog.DirectoryIDFromBytes(rawID)
		if err != nil {
			return ResultRootLayout{}, ErrInvalidReceiveContract
		}
		if dto.Class == "complete_directory" {
			return NewCompleteDirectoryResultRoot(dirID, dto.SourcePath)
		}
		return NewDirectorySelectionResultRoot(dirID, dto.SourcePath)
	case "synthetic_selection":
		if dto.DirectoryID != "" || dto.SourcePath != "" {
			return ResultRootLayout{}, ErrInvalidReceiveContract
		}
		return NewSyntheticSelectionResultRoot(), nil
	default:
		return ResultRootLayout{}, ErrInvalidReceiveContract
	}
}

func decodeStrictJSON(data []byte, dst any) error {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		return err
	}
	var trailing any
	if err := dec.Decode(&trailing); err != io.EOF {
		return ErrInvalidReceiveContract
	}
	return nil
}
