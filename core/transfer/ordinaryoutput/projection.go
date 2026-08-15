package ordinaryoutput

import (
	"errors"
	"strings"

	"github.com/windshare/windshare/core/catalog"
	"github.com/windshare/windshare/core/transfer/receivecontract"
)

var (
	ErrInvalidSourceCatalogPath   = errors.New("ordinary output source catalog path is invalid")
	ErrInvalidArtifactPath        = errors.New("ordinary output artifact path is invalid")
	ErrInvalidAuthenticatedSource = errors.New("ordinary output authenticated source node is invalid")
	ErrInvalidArtifactProjector   = errors.New("ordinary output artifact path projector is invalid")
)

// SourceCatalogPath is an authenticated sender-side coordinate. The synthetic
// root is represented only by EmptySourceCatalogPath.
type SourceCatalogPath struct {
	value string
	valid bool
}

func EmptySourceCatalogPath() SourceCatalogPath {
	return SourceCatalogPath{valid: true}
}

func NewSourceCatalogPath(value string) (SourceCatalogPath, error) {
	if value == "" {
		return SourceCatalogPath{}, ErrInvalidSourceCatalogPath
	}
	canonical, err := catalog.CanonicalPath(value)
	if err != nil || canonical != value {
		return SourceCatalogPath{}, errors.Join(ErrInvalidSourceCatalogPath, err)
	}
	return SourceCatalogPath{value: canonical, valid: true}, nil
}

func (path SourceCatalogPath) String() string { return path.value }
func (path SourceCatalogPath) IsRoot() bool   { return path.valid && path.value == "" }
func (path SourceCatalogPath) Valid() bool    { return path.valid }

// ArtifactPath is a logical output coordinate. It never contains the
// collision-adjusted physical reservation name.
type ArtifactPath struct {
	value string
	valid bool
}

func NewArtifactPath(value string) (ArtifactPath, error) {
	if value == "" {
		return ArtifactPath{}, ErrInvalidArtifactPath
	}
	canonical, err := catalog.CanonicalPath(value)
	if err != nil || canonical != value || artifactPathHasOversizedComponent(value) {
		return ArtifactPath{}, errors.Join(ErrInvalidArtifactPath, err)
	}
	return ArtifactPath{value: canonical, valid: true}, nil
}

func (path ArtifactPath) String() string { return path.value }
func (path ArtifactPath) Valid() bool    { return path.valid }

type SourceNodeRole uint8

const (
	SourceNodeSelected SourceNodeRole = iota + 1
	SourceNodeConnectsSelection
)

func (role SourceNodeRole) Valid() bool {
	return role == SourceNodeSelected || role == SourceNodeConnectsSelection
}

// AuthenticatedSourceNode keeps sender identity and source ancestry independent
// from the projected artifact coordinate.
type AuthenticatedSourceNode struct {
	kind       catalog.NodeKind
	nodeID     catalog.NodeID
	sourcePath SourceCatalogPath
	role       SourceNodeRole
}

func newAuthenticatedSourceNode(
	kind catalog.NodeKind,
	nodeID catalog.NodeID,
	sourcePath SourceCatalogPath,
	role SourceNodeRole,
) (AuthenticatedSourceNode, error) {
	if !sourcePath.Valid() || !role.Valid() || nodeID.IsZero() ||
		(kind != catalog.NodeKindDirectory && kind != catalog.NodeKindFile) ||
		(sourcePath.IsRoot() && kind != catalog.NodeKindDirectory) {
		return AuthenticatedSourceNode{}, ErrInvalidAuthenticatedSource
	}
	return AuthenticatedSourceNode{
		kind: kind, nodeID: nodeID, sourcePath: sourcePath, role: role,
	}, nil
}

func NewAuthenticatedDirectorySourceNode(
	directory catalog.DirectoryID,
	sourcePath SourceCatalogPath,
	role SourceNodeRole,
) (AuthenticatedSourceNode, error) {
	if directory.IsZero() {
		return AuthenticatedSourceNode{}, ErrInvalidAuthenticatedSource
	}
	return newAuthenticatedSourceNode(
		catalog.NodeKindDirectory, directory.NodeID(), sourcePath, role,
	)
}

func NewAuthenticatedFileSourceNode(
	file catalog.FileID,
	sourcePath SourceCatalogPath,
	role SourceNodeRole,
) (AuthenticatedSourceNode, error) {
	if file.IsZero() {
		return AuthenticatedSourceNode{}, ErrInvalidAuthenticatedSource
	}
	return newAuthenticatedSourceNode(
		catalog.NodeKindFile, file.NodeID(), sourcePath, role,
	)
}

func (node AuthenticatedSourceNode) Kind() catalog.NodeKind        { return node.kind }
func (node AuthenticatedSourceNode) NodeID() catalog.NodeID        { return node.nodeID }
func (node AuthenticatedSourceNode) SourcePath() SourceCatalogPath { return node.sourcePath }
func (node AuthenticatedSourceNode) Role() SourceNodeRole          { return node.role }

func (node AuthenticatedSourceNode) valid() bool {
	if !node.sourcePath.Valid() || !node.role.Valid() || node.nodeID.IsZero() {
		return false
	}
	switch node.kind {
	case catalog.NodeKindDirectory:
		return true
	case catalog.NodeKindFile:
		return !node.sourcePath.IsRoot()
	default:
		return false
	}
}

type ArtifactProjectionKind uint8

const (
	ArtifactTraverseOnly ArtifactProjectionKind = iota + 1
	ArtifactMaterialize
	ArtifactReject
)

func (kind ArtifactProjectionKind) Valid() bool {
	return kind >= ArtifactTraverseOnly && kind <= ArtifactReject
}

type ArtifactRejectReason uint8

const (
	ArtifactRejectNone ArtifactRejectReason = iota
	ArtifactRejectInvalidSource
	ArtifactRejectWrongRole
	ArtifactRejectWrongKind
	ArtifactRejectWrongIdentity
	ArtifactRejectUnrelatedSource
)

func (reason ArtifactRejectReason) Valid() bool {
	return reason >= ArtifactRejectInvalidSource && reason <= ArtifactRejectUnrelatedSource
}

type ArtifactPathProjection struct {
	kind   ArtifactProjectionKind
	path   ArtifactPath
	reason ArtifactRejectReason
}

func (projection ArtifactPathProjection) Kind() ArtifactProjectionKind {
	return projection.kind
}

func (projection ArtifactPathProjection) ArtifactPath() (ArtifactPath, bool) {
	return projection.path, projection.kind == ArtifactMaterialize && projection.path.Valid()
}

func (projection ArtifactPathProjection) RejectReason() ArtifactRejectReason {
	if projection.kind != ArtifactReject {
		return ArtifactRejectNone
	}
	return projection.reason
}

// TraverseOnlyProjection constructs the absence of destination authority for a
// source directory that is needed solely to authenticate selected ancestry.
func TraverseOnlyProjection() ArtifactPathProjection { return traverseOnlyProjection() }

// MaterializeArtifactProjection constructs one already-validated logical
// artifact claim. Production transfer code normally obtains this from Project;
// the constructor keeps executor tests explicit without exposing struct fields.
func MaterializeArtifactProjection(path ArtifactPath) (ArtifactPathProjection, error) {
	if !path.Valid() {
		return ArtifactPathProjection{}, ErrInvalidArtifactPath
	}
	return materializeProjection(path.String()), nil
}

type projectionLayoutKind uint8

const (
	projectionSingleFile projectionLayoutKind = iota + 1
	projectionRealResultRoot
	projectionSyntheticResultRoot
	projectionCatalogRoot
)

// ProjectionLayout is a validated, non-encoded view lowered from a canonical
// artifact. It contains only the frozen identity/path/name needed by projection.
type projectionLayout struct {
	kind          projectionLayoutKind
	syntheticRoot catalog.DirectoryID
	anchorID      catalog.NodeID
	sourcePath    SourceCatalogPath
	preferredName string
}

func newSingleFileProjectionLayout(
	syntheticRoot catalog.DirectoryID,
	file catalog.FileID,
	sourcePath SourceCatalogPath,
	preferredName string,
) (projectionLayout, error) {
	if syntheticRoot.IsZero() || file.IsZero() || file.NodeID() == syntheticRoot.NodeID() ||
		!sourcePath.Valid() || sourcePath.IsRoot() || !canonicalArtifactComponent(preferredName) {
		return projectionLayout{}, ErrInvalidArtifactProjector
	}
	return projectionLayout{
		kind: projectionSingleFile, syntheticRoot: syntheticRoot, anchorID: file.NodeID(),
		sourcePath: sourcePath, preferredName: preferredName,
	}, nil
}

func newRealResultRootProjectionLayout(
	syntheticRoot catalog.DirectoryID,
	directory catalog.DirectoryID,
	sourcePath SourceCatalogPath,
	preferredName string,
) (projectionLayout, error) {
	if syntheticRoot.IsZero() || directory.IsZero() || directory == syntheticRoot ||
		!sourcePath.Valid() || sourcePath.IsRoot() || !canonicalArtifactComponent(preferredName) ||
		(preferredName != sourceLeaf(sourcePath.String()) &&
			preferredName != sourceLeaf(sourcePath.String())+receivecontract.PartialSelectionSuffix) {
		return projectionLayout{}, ErrInvalidArtifactProjector
	}
	return projectionLayout{
		kind: projectionRealResultRoot, syntheticRoot: syntheticRoot, anchorID: directory.NodeID(),
		sourcePath: sourcePath, preferredName: preferredName,
	}, nil
}

func newSyntheticResultRootProjectionLayout(
	syntheticRoot catalog.DirectoryID,
	preferredName string,
) (projectionLayout, error) {
	if syntheticRoot.IsZero() || !canonicalArtifactComponent(preferredName) {
		return projectionLayout{}, ErrInvalidArtifactProjector
	}
	return projectionLayout{
		kind: projectionSyntheticResultRoot, syntheticRoot: syntheticRoot,
		anchorID: syntheticRoot.NodeID(), sourcePath: EmptySourceCatalogPath(),
		preferredName: preferredName,
	}, nil
}

func newCatalogRootProjectionLayout(syntheticRoot catalog.DirectoryID) (projectionLayout, error) {
	if syntheticRoot.IsZero() {
		return projectionLayout{}, ErrInvalidArtifactProjector
	}
	return projectionLayout{
		kind: projectionCatalogRoot, syntheticRoot: syntheticRoot,
		anchorID: syntheticRoot.NodeID(), sourcePath: EmptySourceCatalogPath(),
	}, nil
}

func (layout projectionLayout) valid() bool {
	if layout.syntheticRoot.IsZero() || layout.anchorID.IsZero() || !layout.sourcePath.Valid() {
		return false
	}
	switch layout.kind {
	case projectionSingleFile:
		return !layout.sourcePath.IsRoot() && layout.anchorID != layout.syntheticRoot.NodeID() &&
			canonicalArtifactComponent(layout.preferredName)
	case projectionRealResultRoot:
		leaf := sourceLeaf(layout.sourcePath.String())
		return !layout.sourcePath.IsRoot() && layout.anchorID != layout.syntheticRoot.NodeID() &&
			canonicalArtifactComponent(layout.preferredName) &&
			(layout.preferredName == leaf ||
				layout.preferredName == leaf+receivecontract.PartialSelectionSuffix)
	case projectionSyntheticResultRoot:
		return layout.sourcePath.IsRoot() && layout.anchorID == layout.syntheticRoot.NodeID() &&
			canonicalArtifactComponent(layout.preferredName)
	case projectionCatalogRoot:
		return layout.sourcePath.IsRoot() && layout.anchorID == layout.syntheticRoot.NodeID() &&
			layout.preferredName == ""
	default:
		return false
	}
}

// ArtifactPathProjector is immutable after construction. Project has no state,
// I/O, or destination authority.
type ArtifactPathProjector struct {
	layout projectionLayout
}

func newArtifactPathProjector(layout projectionLayout) (ArtifactPathProjector, error) {
	if !layout.valid() {
		return ArtifactPathProjector{}, ErrInvalidArtifactProjector
	}
	return ArtifactPathProjector{layout: layout}, nil
}

// NewArtifactPathProjector validates the canonical artifact once. There is no
// constructor that accepts a free-form preferred name, so receivecontract
// remains the only naming authority.
func NewArtifactPathProjector(
	syntheticRoot catalog.DirectoryID,
	artifact receivecontract.ArtifactSpec,
) (ArtifactPathProjector, error) {
	if syntheticRoot.IsZero() || artifact.IsZero() {
		return ArtifactPathProjector{}, ErrInvalidArtifactProjector
	}
	tree, ok := artifact.DirectoryTree()
	if !ok {
		return ArtifactPathProjector{}, ErrInvalidArtifactProjector
	}
	var layout projectionLayout
	var err error
	switch tree.Kind() {
	case receivecontract.DirectoryTreeSingleFile:
		single, _ := tree.SingleFile()
		sourcePath, pathErr := NewSourceCatalogPath(single.SourcePath)
		if pathErr != nil {
			return ArtifactPathProjector{}, pathErr
		}
		layout, err = newSingleFileProjectionLayout(
			syntheticRoot, single.FileID, sourcePath, single.SuggestedName,
		)
	case receivecontract.DirectoryTreeResultRoot:
		root, _ := tree.ResultRoot()
		switch root.AnchorKind() {
		case receivecontract.ResultRootDirectoryAnchor:
			sourcePath, pathErr := NewSourceCatalogPath(root.SourcePath())
			if pathErr != nil {
				return ArtifactPathProjector{}, pathErr
			}
			layout, err = newRealResultRootProjectionLayout(
				syntheticRoot, root.DirectoryID(), sourcePath, root.Name(),
			)
		case receivecontract.ResultRootSyntheticAnchor:
			layout, err = newSyntheticResultRootProjectionLayout(syntheticRoot, root.Name())
		default:
			err = ErrInvalidArtifactProjector
		}
	case receivecontract.DirectoryTreeCatalogRoot:
		layout, err = newCatalogRootProjectionLayout(syntheticRoot)
	default:
		err = ErrInvalidArtifactProjector
	}
	if err != nil {
		return ArtifactPathProjector{}, err
	}
	return newArtifactPathProjector(layout)
}

func (projector ArtifactPathProjector) Project(node AuthenticatedSourceNode) ArtifactPathProjection {
	if !projector.layout.valid() || !node.valid() {
		return rejectProjection(ArtifactRejectInvalidSource)
	}
	switch projector.layout.kind {
	case projectionSingleFile:
		return projector.projectSingleFile(node)
	case projectionRealResultRoot:
		return projector.projectRealResultRoot(node)
	case projectionSyntheticResultRoot:
		return projector.projectSyntheticResultRoot(node)
	case projectionCatalogRoot:
		return projector.projectCatalogRoot(node)
	default:
		return rejectProjection(ArtifactRejectInvalidSource)
	}
}

func (projector ArtifactPathProjector) projectSingleFile(
	node AuthenticatedSourceNode,
) ArtifactPathProjection {
	if node.sourcePath.IsRoot() {
		if node.kind == catalog.NodeKindDirectory &&
			node.nodeID == projector.layout.syntheticRoot.NodeID() &&
			node.role == SourceNodeConnectsSelection {
			return traverseOnlyProjection()
		}
		return rejectProjection(ArtifactRejectWrongIdentity)
	}
	relation := compareSourcePaths(node.sourcePath.String(), projector.layout.sourcePath.String())
	if node.kind == catalog.NodeKindDirectory {
		if relation == sourcePathStrictAncestor && node.role == SourceNodeConnectsSelection {
			return traverseOnlyProjection()
		}
		return rejectProjection(ArtifactRejectUnrelatedSource)
	}
	if node.kind != catalog.NodeKindFile {
		return rejectProjection(ArtifactRejectWrongKind)
	}
	if relation != sourcePathEqual {
		return rejectProjection(ArtifactRejectUnrelatedSource)
	}
	if node.nodeID != projector.layout.anchorID {
		return rejectProjection(ArtifactRejectWrongIdentity)
	}
	if node.role != SourceNodeSelected {
		return rejectProjection(ArtifactRejectWrongRole)
	}
	return materializeProjection(projector.layout.preferredName)
}

func (projector ArtifactPathProjector) projectRealResultRoot(
	node AuthenticatedSourceNode,
) ArtifactPathProjection {
	if node.sourcePath.IsRoot() {
		if node.kind == catalog.NodeKindDirectory &&
			node.nodeID == projector.layout.syntheticRoot.NodeID() &&
			node.role == SourceNodeConnectsSelection {
			return traverseOnlyProjection()
		}
		return rejectProjection(ArtifactRejectWrongIdentity)
	}
	relation := compareSourcePaths(node.sourcePath.String(), projector.layout.sourcePath.String())
	switch relation {
	case sourcePathStrictAncestor:
		if node.kind == catalog.NodeKindDirectory && node.role == SourceNodeConnectsSelection {
			return traverseOnlyProjection()
		}
		return rejectProjection(ArtifactRejectWrongRole)
	case sourcePathEqual:
		if node.kind != catalog.NodeKindDirectory {
			return rejectProjection(ArtifactRejectWrongKind)
		}
		if node.nodeID != projector.layout.anchorID {
			return rejectProjection(ArtifactRejectWrongIdentity)
		}
	case sourcePathDescendant:
		// Descendant identity is authenticated by the source admission chain;
		// only the frozen anchor itself needs an exact ID check here.
	default:
		return rejectProjection(ArtifactRejectUnrelatedSource)
	}
	if node.role != SourceNodeSelected && node.role != SourceNodeConnectsSelection {
		return rejectProjection(ArtifactRejectWrongRole)
	}
	suffix := ""
	if relation == sourcePathDescendant {
		suffix = strings.TrimPrefix(
			node.sourcePath.String(),
			projector.layout.sourcePath.String()+"/",
		)
	}
	return materializeProjection(joinArtifactPath(projector.layout.preferredName, suffix))
}

func (projector ArtifactPathProjector) projectSyntheticResultRoot(
	node AuthenticatedSourceNode,
) ArtifactPathProjection {
	if node.sourcePath.IsRoot() {
		if node.kind != catalog.NodeKindDirectory {
			return rejectProjection(ArtifactRejectWrongKind)
		}
		if node.nodeID != projector.layout.syntheticRoot.NodeID() {
			return rejectProjection(ArtifactRejectWrongIdentity)
		}
	} else if node.nodeID == projector.layout.syntheticRoot.NodeID() {
		return rejectProjection(ArtifactRejectWrongIdentity)
	}
	if node.role != SourceNodeSelected && node.role != SourceNodeConnectsSelection {
		return rejectProjection(ArtifactRejectWrongRole)
	}
	return materializeProjection(
		joinArtifactPath(projector.layout.preferredName, node.sourcePath.String()),
	)
}

func (projector ArtifactPathProjector) projectCatalogRoot(
	node AuthenticatedSourceNode,
) ArtifactPathProjection {
	if node.sourcePath.IsRoot() {
		if node.kind == catalog.NodeKindDirectory &&
			node.nodeID == projector.layout.syntheticRoot.NodeID() &&
			node.role == SourceNodeConnectsSelection {
			return traverseOnlyProjection()
		}
		return rejectProjection(ArtifactRejectWrongIdentity)
	}
	if node.nodeID == projector.layout.syntheticRoot.NodeID() {
		return rejectProjection(ArtifactRejectWrongIdentity)
	}
	if node.role != SourceNodeSelected && node.role != SourceNodeConnectsSelection {
		return rejectProjection(ArtifactRejectWrongRole)
	}
	return materializeProjection(node.sourcePath.String())
}

type sourcePathRelation uint8

const (
	sourcePathUnrelated sourcePathRelation = iota
	sourcePathStrictAncestor
	sourcePathEqual
	sourcePathDescendant
)

func compareSourcePaths(candidate, anchor string) sourcePathRelation {
	switch {
	case candidate == anchor:
		return sourcePathEqual
	case candidate == "" || strings.HasPrefix(anchor, candidate+"/"):
		return sourcePathStrictAncestor
	case strings.HasPrefix(candidate, anchor+"/"):
		return sourcePathDescendant
	default:
		return sourcePathUnrelated
	}
}

func sourceLeaf(path string) string {
	if separator := strings.LastIndexByte(path, '/'); separator >= 0 {
		return path[separator+1:]
	}
	return path
}

func artifactPathHasOversizedComponent(value string) bool {
	for component := range strings.SplitSeq(value, "/") {
		if len([]byte(component)) > receivecontract.MaxResultComponentBytes {
			return true
		}
	}
	return false
}

func canonicalArtifactComponent(value string) bool {
	if value == "" || len([]byte(value)) > receivecontract.MaxResultComponentBytes ||
		strings.ContainsRune(value, '/') {
		return false
	}
	canonical, err := catalog.CanonicalPath(value)
	return err == nil && canonical == value &&
		!strings.HasPrefix(strings.ToLower(value), ".wsresume")
}

func joinArtifactPath(root, suffix string) string {
	if suffix == "" {
		return root
	}
	return root + "/" + suffix
}

func traverseOnlyProjection() ArtifactPathProjection {
	return ArtifactPathProjection{kind: ArtifactTraverseOnly}
}

func materializeProjection(path string) ArtifactPathProjection {
	artifact, err := NewArtifactPath(path)
	if err != nil {
		return rejectProjection(ArtifactRejectInvalidSource)
	}
	return ArtifactPathProjection{kind: ArtifactMaterialize, path: artifact}
}

func rejectProjection(reason ArtifactRejectReason) ArtifactPathProjection {
	return ArtifactPathProjection{kind: ArtifactReject, reason: reason}
}
