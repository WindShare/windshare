package receivecontract

import (
	"bytes"
	"strings"
	"unicode/utf8"

	"github.com/windshare/windshare/core/catalog"
)

const (
	artifactSpecDomain     = "windshare/artifact-spec/v1"
	resultRootLayoutDomain = "windshare/result-root-layout/v1"

	DefaultResultRootName   = "windshare"
	DefaultArchiveName      = "windshare.zip"
	PartialSelectionSuffix  = "-selection"
	ArchiveExtension        = ".zip"
	ResultNamePolicy        = "windshare/result-name/v1-unicode-15.0.0"
	MaxResultComponentBytes = 255
	CollisionSuffixHexChars = 10
)

type ArtifactKind uint8

const (
	ArtifactOriginalFile ArtifactKind = iota + 1
	ArtifactDirectoryTree
	ArtifactZipArchive
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

type ZipEncoding uint8

const ZipEncodingStore ZipEncoding = 1

type ArtifactCompleteness uint8

const ArtifactCompleteOnly ArtifactCompleteness = 1

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
	encoded     []byte
}

type ZipArchiveSpec struct {
	Layout        ResultRootLayout
	SuggestedName string
	Encoding      ZipEncoding
	Completeness  ArtifactCompleteness
}

type ArtifactSpec struct {
	kind      ArtifactKind
	original  OriginalFileSpec
	directory DirectoryTreeLayout
	zip       ZipArchiveSpec
	encoded   []byte
	digest    ArtifactDigest
}

func NewOriginalFileArtifact(file catalog.FileID, sourcePath, suggestedName string) (ArtifactSpec, error) {
	canonicalPath, leaf, err := canonicalSourcePath(sourcePath)
	if err != nil || file.IsZero() || suggestedName != leaf || canonicalComponent(suggestedName) != nil {
		return ArtifactSpec{}, ErrInvalidReceiveContract
	}
	original := OriginalFileSpec{FileID: file, SourcePath: canonicalPath, SuggestedName: suggestedName}
	encoded := canonicalRecord(artifactSpecDomain,
		[]byte{byte(ArtifactOriginalFile)},
		frame(file.Bytes()),
		frame(canonicalPathBytes(canonicalPath)),
		frame([]byte(suggestedName)),
	)
	return artifact(ArtifactOriginalFile, original, DirectoryTreeLayout{}, ZipArchiveSpec{}, encoded), nil
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
	if !layout.valid() {
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

func NewZipArchiveArtifact(layout ResultRootLayout) (ArtifactSpec, error) {
	if !layout.valid() {
		return ArtifactSpec{}, ErrInvalidReceiveContract
	}
	name, err := AppendProtectedSuffix(layout.Name(), ArchiveExtension)
	if err != nil {
		return ArtifactSpec{}, err
	}
	zip := ZipArchiveSpec{
		Layout: layout, SuggestedName: name,
		Encoding: ZipEncodingStore, Completeness: ArtifactCompleteOnly,
	}
	encoded := canonicalRecord(artifactSpecDomain,
		[]byte{byte(ArtifactZipArchive)},
		frame(layout.CanonicalBytes()),
		frame([]byte(name)),
		frame([]byte{byte(ZipEncodingStore)}),
		frame([]byte{byte(ArtifactCompleteOnly)}),
	)
	return artifact(ArtifactZipArchive, OriginalFileSpec{}, DirectoryTreeLayout{}, zip, encoded), nil
}

func newDirectoryTreeArtifact(layout DirectoryTreeLayout) (ArtifactSpec, error) {
	if !layout.valid() {
		return ArtifactSpec{}, ErrInvalidReceiveContract
	}
	encoded := canonicalRecord(artifactSpecDomain,
		[]byte{byte(ArtifactDirectoryTree)}, frame(layout.canonicalBytes()),
	)
	return artifact(ArtifactDirectoryTree, OriginalFileSpec{}, layout, ZipArchiveSpec{}, encoded), nil
}

func artifact(kind ArtifactKind, original OriginalFileSpec, directory DirectoryTreeLayout, zip ZipArchiveSpec, encoded []byte) ArtifactSpec {
	sum := digest(encoded)
	return ArtifactSpec{
		kind: kind, original: original, directory: directory, zip: zip,
		encoded: clone(encoded), digest: ArtifactDigest(sum),
	}
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
	anchor := []byte{byte(anchorKind)}
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
		anchor = append(anchor, frame(directory.Bytes())...)
		anchor = append(anchor, frame(canonicalPathBytes(sourcePath))...)
	default:
		return ResultRootLayout{}, ErrInvalidReceiveContract
	}
	encoded := canonicalRecord(resultRootLayoutDomain,
		frame([]byte{byte(class)}), frame(anchor), frame([]byte(name)),
	)
	return ResultRootLayout{
		class: class, anchorKind: anchorKind, directoryID: directory,
		sourcePath: sourcePath, name: name, encoded: encoded,
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
	truncated := base
	for len([]byte(truncated)) > maximumBaseBytes {
		_, width := utf8.DecodeLastRuneInString(truncated)
		if width == 0 {
			return "", ErrInvalidReceiveContract
		}
		truncated = truncated[:len(truncated)-width]
	}
	if truncated == "" {
		return "", ErrInvalidReceiveContract
	}
	result := truncated + suffix
	if canonicalComponent(result) != nil {
		return "", ErrInvalidReceiveContract
	}
	return result, nil
}

func canonicalSourcePath(value string) (string, string, error) {
	canonical, err := catalog.CanonicalPath(value)
	if err != nil || canonical != value {
		return "", "", ErrInvalidReceiveContract
	}
	leaf := canonical
	if separator := strings.LastIndexByte(canonical, '/'); separator >= 0 {
		leaf = canonical[separator+1:]
	}
	if canonicalComponent(leaf) != nil {
		return "", "", ErrInvalidReceiveContract
	}
	return canonical, leaf, nil
}

func canonicalComponent(value string) error {
	canonical, err := catalog.CanonicalName(value)
	if err != nil || canonical != value || len([]byte(value)) > MaxResultComponentBytes ||
		strings.HasPrefix(strings.ToLower(value), ".wsresume") {
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

func (layout DirectoryTreeLayout) canonicalBytes() []byte {
	encoded := []byte{byte(layout.kind)}
	switch layout.kind {
	case DirectoryTreeSingleFile:
		encoded = append(encoded, frame(layout.singleFile.FileID.Bytes())...)
		encoded = append(encoded, frame(canonicalPathBytes(layout.singleFile.SourcePath))...)
		encoded = append(encoded, frame([]byte(layout.singleFile.SuggestedName))...)
	case DirectoryTreeResultRoot:
		encoded = append(encoded, frame(layout.resultRoot.CanonicalBytes())...)
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
		return layout.resultRoot.valid()
	case DirectoryTreeCatalogRoot:
		return layout.singleFile == (OriginalFileSpec{}) && !layout.resultRoot.valid()
	default:
		return false
	}
}

func (layout ResultRootLayout) valid() bool {
	if len(layout.encoded) == 0 {
		return false
	}
	rebuilt, err := newResultRootLayout(
		layout.class, layout.anchorKind, layout.directoryID, layout.sourcePath, layout.name,
	)
	return err == nil && bytes.Equal(layout.encoded, rebuilt.encoded)
}

func (artifact ArtifactSpec) valid() bool {
	if artifact.digest.IsZero() || len(artifact.encoded) == 0 {
		return false
	}
	return ArtifactDigest(digest(artifact.encoded)) == artifact.digest
}

func (artifact ArtifactSpec) Kind() ArtifactKind                 { return artifact.kind }
func (artifact ArtifactSpec) Digest() ArtifactDigest             { return artifact.digest }
func (artifact ArtifactSpec) CanonicalBytes() []byte             { return clone(artifact.encoded) }
func (artifact ArtifactSpec) IsZero() bool                       { return !artifact.valid() }
func (layout DirectoryTreeLayout) Kind() DirectoryTreeLayoutKind { return layout.kind }
func (layout ResultRootLayout) Class() ResultRootClass           { return layout.class }
func (layout ResultRootLayout) AnchorKind() ResultRootAnchorKind { return layout.anchorKind }
func (layout ResultRootLayout) DirectoryID() catalog.DirectoryID { return layout.directoryID }
func (layout ResultRootLayout) SourcePath() string               { return layout.sourcePath }
func (layout ResultRootLayout) Name() string                     { return layout.name }
func (layout ResultRootLayout) CanonicalBytes() []byte           { return clone(layout.encoded) }

func (artifact ArtifactSpec) OriginalFile() (OriginalFileSpec, bool) {
	return artifact.original, artifact.valid() && artifact.kind == ArtifactOriginalFile
}

func (artifact ArtifactSpec) DirectoryTree() (DirectoryTreeLayout, bool) {
	return artifact.directory, artifact.valid() && artifact.kind == ArtifactDirectoryTree
}

func (artifact ArtifactSpec) ZipArchive() (ZipArchiveSpec, bool) {
	return artifact.zip, artifact.valid() && artifact.kind == ArtifactZipArchive
}

func (layout DirectoryTreeLayout) SingleFile() (OriginalFileSpec, bool) {
	return layout.singleFile, layout.valid() && layout.kind == DirectoryTreeSingleFile
}

func (layout DirectoryTreeLayout) ResultRoot() (ResultRootLayout, bool) {
	return layout.resultRoot, layout.valid() && layout.kind == DirectoryTreeResultRoot
}
