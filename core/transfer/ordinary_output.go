package transfer

import (
	"bytes"
	"errors"
	"sort"

	"github.com/windshare/windshare/core/catalog"
	"github.com/windshare/windshare/core/content"
	"github.com/windshare/windshare/core/transfer/ordinaryoutput"
	"github.com/windshare/windshare/core/transfer/receivecontract"
)

// MaterializationRootRelativePath names an object below the output authority's
// already-reserved root. The empty value is a valid directory-root coordinate;
// keeping it typed prevents catalog and logical paths from crossing this boundary.
type MaterializationRootRelativePath struct {
	value string
	valid bool
}

func NewMaterializationRootRelativePath(value string) (MaterializationRootRelativePath, error) {
	if value == "" {
		return MaterializationRootRelativePath{valid: true}, nil
	}
	canonical, err := catalog.CanonicalPath(value)
	if err != nil || canonical != value {
		return MaterializationRootRelativePath{}, errors.Join(ErrInvalidOutputBinding, err)
	}
	return MaterializationRootRelativePath{value: canonical, valid: true}, nil
}

func (path MaterializationRootRelativePath) String() string { return path.value }
func (path MaterializationRootRelativePath) IsRoot() bool   { return path.valid && path.value == "" }
func (path MaterializationRootRelativePath) Valid() bool    { return path.valid }

// MaterializationDirectory contains only destination-root-relative admission
// coordinates. Sender authentication remains in AuthenticatedSourceDirectory.
type MaterializationDirectory struct {
	directory  catalog.DirectoryID
	generation catalog.DirectoryGeneration
	path       MaterializationRootRelativePath
	parent     DirectoryAdmission
	modified   catalog.ModifiedTime
}

func NewMaterializationDirectory(
	directory catalog.DirectoryID,
	generation catalog.DirectoryGeneration,
	path MaterializationRootRelativePath,
	parent DirectoryAdmission,
	modified catalog.ModifiedTime,
) (MaterializationDirectory, error) {
	value := MaterializationDirectory{
		directory: directory, generation: generation, path: path, parent: parent, modified: modified,
	}
	if directory.IsZero() || generation.IsZero() || !path.Valid() {
		return MaterializationDirectory{}, ErrInvalidDirectoryAdmission
	}
	return value, nil
}

func (directory MaterializationDirectory) DirectoryID() catalog.DirectoryID {
	return directory.directory
}
func (directory MaterializationDirectory) Generation() catalog.DirectoryGeneration {
	return directory.generation
}
func (directory MaterializationDirectory) Path() MaterializationRootRelativePath {
	return directory.path
}
func (directory MaterializationDirectory) ParentAdmission() DirectoryAdmission {
	return directory.parent
}
func (directory MaterializationDirectory) ModifiedTime() catalog.ModifiedTime {
	return directory.modified
}
func (directory MaterializationDirectory) Valid() bool {
	return !directory.directory.IsZero() && !directory.generation.IsZero() && directory.path.Valid()
}

type MaterializationFileParentKind uint8

const (
	MaterializationFileParentReference MaterializationFileParentKind = iota + 1
	MaterializationFileParentDirectory
)

// MaterializationFileParent makes a single-file authentication reference
// structurally different from a file whose destination ancestry is admitted.
type MaterializationFileParent struct {
	kind            MaterializationFileParentKind
	directory       catalog.DirectoryID
	generation      catalog.DirectoryGeneration
	sourcePath      ordinaryoutput.SourceCatalogPath
	admission       DirectoryAdmission
	materialization MaterializedDirectoryClaim
}

func NewReferenceMaterializationFileParent(
	directory catalog.DirectoryID,
	generation catalog.DirectoryGeneration,
	sourcePath ordinaryoutput.SourceCatalogPath,
) (MaterializationFileParent, error) {
	parent := MaterializationFileParent{
		kind:      MaterializationFileParentReference,
		directory: directory, generation: generation, sourcePath: sourcePath,
	}
	if !parent.valid() {
		return MaterializationFileParent{}, ErrInvalidOutputBinding
	}
	return parent, nil
}

func NewDirectoryMaterializationFileParent(
	directory catalog.DirectoryID,
	generation catalog.DirectoryGeneration,
	sourcePath ordinaryoutput.SourceCatalogPath,
	admission DirectoryAdmission,
	materialization MaterializedDirectoryClaim,
) (MaterializationFileParent, error) {
	parent := MaterializationFileParent{
		kind:      MaterializationFileParentDirectory,
		directory: directory, generation: generation, sourcePath: sourcePath,
		admission: admission, materialization: materialization,
	}
	if !parent.valid() {
		return MaterializationFileParent{}, ErrInvalidOutputBinding
	}
	return parent, nil
}

func (parent MaterializationFileParent) Kind() MaterializationFileParentKind { return parent.kind }
func (parent MaterializationFileParent) DirectoryID() catalog.DirectoryID    { return parent.directory }
func (parent MaterializationFileParent) Generation() catalog.DirectoryGeneration {
	return parent.generation
}
func (parent MaterializationFileParent) SourcePath() ordinaryoutput.SourceCatalogPath {
	return parent.sourcePath
}
func (parent MaterializationFileParent) Admission() DirectoryAdmission { return parent.admission }
func (parent MaterializationFileParent) Materialization() MaterializedDirectoryClaim {
	return parent.materialization
}

func (parent MaterializationFileParent) valid() bool {
	if parent.directory.IsZero() || parent.generation.IsZero() || !parent.sourcePath.Valid() {
		return false
	}
	switch parent.kind {
	case MaterializationFileParentReference:
		return parent.admission.IsZero() && !parent.materialization.Valid()
	case MaterializationFileParentDirectory:
		materializationMatches := parent.materialization.Valid() &&
			parent.materialization.Admission().Equal(parent.admission)
		catalogRootReference := !parent.materialization.Valid() &&
			parent.admission.Layout() == DirectoryAdmissionTreeCatalogRoot && parent.admission.Path() == ""
		return !parent.admission.IsZero() && (materializationMatches || catalogRootReference) &&
			parent.admission.DirectoryID() == parent.directory &&
			parent.admission.Generation() == parent.generation
	default:
		return false
	}
}

func (selection SelectionSpec) OrdinaryOutputSelection() (ordinaryoutput.Selection, error) {
	if selection.IsZero() {
		return ordinaryoutput.Selection{}, ErrInvalidSelectionRules
	}
	var digest [SelectionSpecDigestBytes]byte
	copy(digest[:], selection.digest[:])
	rules := selection.rules
	switch {
	case rules.mode == SelectionByNodeID && rules.defaultSelected &&
		len(rules.directories) == 0 && len(rules.files) == 0:
		return ordinaryoutput.NewWholeShareSelection(
			selection.share,
			selection.root,
			digest,
		)
	case rules.mode == SelectionByCatalogPath:
		return ordinaryoutput.NewCatalogPathSelection(
			selection.share,
			selection.root,
			digest,
			rules.pathTargets,
		)
	case rules.mode == SelectionByNodeID:
		targets := make([]ordinaryoutput.OpaqueSelectionTarget, 0, len(rules.directories)+len(rules.files))
		for directory, selected := range rules.directories {
			target, err := ordinaryoutput.NewOpaqueDirectoryTarget(directory, selected)
			if err != nil {
				return ordinaryoutput.Selection{}, errors.Join(ErrInvalidSelectionRules, err)
			}
			targets = append(targets, target)
		}
		for file, selected := range rules.files {
			target, err := ordinaryoutput.NewOpaqueFileTarget(file, selected)
			if err != nil {
				return ordinaryoutput.Selection{}, errors.Join(ErrInvalidSelectionRules, err)
			}
			targets = append(targets, target)
		}
		sort.Slice(targets, func(left, right int) bool {
			if comparison := bytes.Compare(targets[left].NodeID().Bytes(), targets[right].NodeID().Bytes()); comparison != 0 {
				return comparison < 0
			}
			return targets[left].Kind() < targets[right].Kind()
		})
		return ordinaryoutput.NewOpaqueNodeSelection(
			selection.share,
			selection.root,
			digest,
			rules.defaultSelected,
			targets,
		)
	default:
		return ordinaryoutput.Selection{}, ErrInvalidSelectionRules
	}
}

// MaterializeOrdinaryOutputShape is the only lowering boundary from the
// construction-only proof into the canonical receive contract.
func MaterializeOrdinaryOutputShape(
	decision ordinaryoutput.ShapeDecision,
) (receivecontract.ArtifactSpec, error) {
	if !decision.Valid() {
		return receivecontract.ArtifactSpec{}, ordinaryoutput.ErrInvalidShapeResolution
	}
	switch decision.Kind() {
	case ordinaryoutput.ShapeSingleFile:
		return receivecontract.NewSingleFileDirectoryTree(
			decision.FileID(),
			decision.SourcePath(),
			decision.PreferredName(),
		)
	case ordinaryoutput.ShapeCompleteDirectory:
		layout, err := receivecontract.NewCompleteDirectoryResultRoot(
			decision.DirectoryID(),
			decision.SourcePath(),
		)
		if err != nil {
			return receivecontract.ArtifactSpec{}, err
		}
		return receivecontract.NewResultRootDirectoryTree(layout)
	case ordinaryoutput.ShapePartialDirectory:
		layout, err := receivecontract.NewDirectorySelectionResultRoot(
			decision.DirectoryID(),
			decision.SourcePath(),
		)
		if err != nil {
			return receivecontract.ArtifactSpec{}, err
		}
		return receivecontract.NewResultRootDirectoryTree(layout)
	case ordinaryoutput.ShapeSyntheticSelection:
		return receivecontract.NewResultRootDirectoryTree(
			receivecontract.NewSyntheticSelectionResultRoot(),
		)
	default:
		return receivecontract.ArtifactSpec{}, ordinaryoutput.ErrInvalidShapeResolution
	}
}

// DirectoryMaterializationRequest keeps authenticated ancestry distinct from
// logical output authority. Its fields are closed so only the frozen projector
// can mint the projection consumed by DirectTreeSession.
type DirectoryMaterializationRequest struct {
	source                AuthenticatedSourceDirectory
	role                  ordinaryoutput.SourceNodeRole
	projection            ordinaryoutput.ArtifactPathProjection
	directory             MaterializationDirectory
	materialized          bool
	parentMaterialization MaterializedDirectoryClaim
}

func (request DirectoryMaterializationRequest) Source() AuthenticatedSourceDirectory {
	return request.source
}

func (request DirectoryMaterializationRequest) Role() ordinaryoutput.SourceNodeRole {
	return request.role
}

func (request DirectoryMaterializationRequest) Projection() ordinaryoutput.ArtifactPathProjection {
	return request.projection
}

func (request DirectoryMaterializationRequest) Directory() (MaterializationDirectory, bool) {
	return request.directory, request.materialized && request.directory.Valid()
}

func (request DirectoryMaterializationRequest) ParentMaterialization() MaterializedDirectoryClaim {
	return request.parentMaterialization
}

// NewDirectoryMaterializationRequest applies the frozen projector at the only
// transfer-to-output boundary. Traverse-only requests carry no artifact claim,
// and rejected source values never become output requests.
func NewDirectoryMaterializationRequest(
	intent ReceiveIntent,
	source AuthenticatedSourceDirectory,
	role ordinaryoutput.SourceNodeRole,
	parent MaterializedDirectoryClaim,
) (DirectoryMaterializationRequest, error) {
	projector, coordinateErr := newDirectTreeCoordinateProjector(intent)
	if coordinateErr != nil {
		return DirectoryMaterializationRequest{}, coordinateErr
	}
	request, projection, err := projectDirectoryMaterializationRequest(projector, source, role, parent)
	if err != nil {
		return DirectoryMaterializationRequest{}, err
	}
	if projection.Kind() == ordinaryoutput.ArtifactReject {
		return DirectoryMaterializationRequest{}, ordinaryoutput.ErrInvalidAuthenticatedSource
	}
	return request, nil
}

func projectDirectoryMaterializationRequest(
	projector directTreeCoordinateProjector,
	source AuthenticatedSourceDirectory,
	role ordinaryoutput.SourceNodeRole,
	parent MaterializedDirectoryClaim,
) (DirectoryMaterializationRequest, ordinaryoutput.ArtifactPathProjection, error) {
	if !source.SourcePath.Valid() {
		return DirectoryMaterializationRequest{}, ordinaryoutput.ArtifactPathProjection{},
			ordinaryoutput.ErrInvalidSourceCatalogPath
	}
	projection, directory, materialized, err := projector.projectDirectory(source, role)
	if err != nil {
		return DirectoryMaterializationRequest{}, ordinaryoutput.ArtifactPathProjection{}, err
	}
	if projection.Kind() == ordinaryoutput.ArtifactReject {
		return DirectoryMaterializationRequest{}, projection, nil
	}
	return DirectoryMaterializationRequest{
		source: source, role: role, projection: projection, directory: directory,
		materialized: materialized, parentMaterialization: parent,
	}, projection, nil
}

func (request DirectoryMaterializationRequest) matchesProjector(
	projector directTreeCoordinateProjector,
) bool {
	projected, projection, err := projectDirectoryMaterializationRequest(
		projector, request.source, request.role, request.parentMaterialization,
	)
	return err == nil && projection.Kind() != ordinaryoutput.ArtifactReject && projected == request
}

// DirectoryMaterializationMatchesIntent lets an output session revalidate the
// closed request against its own immutable intent instead of trusting the
// projector instance supplied by a caller.
func DirectoryMaterializationMatchesIntent(
	intent ReceiveIntent,
	request DirectoryMaterializationRequest,
) bool {
	coordinates, err := newDirectTreeCoordinateProjector(intent)
	return err == nil && directoryMaterializationMatchesProjector(coordinates, request)
}

func directoryMaterializationMatchesProjector(
	projector directTreeCoordinateProjector,
	request DirectoryMaterializationRequest,
) bool {
	return request.matchesProjector(projector)
}

// MaterializedDirectoryClaim can only be derived from the exact projected
// directory request that produced its authenticated admission.
type MaterializedDirectoryClaim struct {
	admission DirectoryAdmission
	path      MaterializationRootRelativePath
}

func NewMaterializedDirectoryClaim(
	admission DirectoryAdmission,
	request DirectoryMaterializationRequest,
) (MaterializedDirectoryClaim, error) {
	source := request.source
	directory, materialized := request.Directory()
	_, projectedArtifact := request.projection.ArtifactPath()
	var expectedParentToken []byte
	if materialized && !directory.ParentAdmission().IsZero() {
		expectedParentToken = directory.ParentAdmission().Bytes()
	}
	if !materialized || !projectedArtifact || admission.IsZero() ||
		admission.DirectoryID() != source.DirectoryID || admission.DirectoryID() != directory.DirectoryID() ||
		admission.Generation() != source.Generation || admission.Generation() != directory.Generation() ||
		admission.Path() != directory.Path().String() ||
		admission.ModifiedTime() != source.ModifiedTime ||
		!bytes.Equal(admission.ParentToken(), expectedParentToken) {
		return MaterializedDirectoryClaim{}, ErrInvalidOutputBinding
	}
	return MaterializedDirectoryClaim{admission: admission, path: directory.Path()}, nil
}

func (claim MaterializedDirectoryClaim) Admission() DirectoryAdmission { return claim.admission }

func (claim MaterializedDirectoryClaim) Path() MaterializationRootRelativePath { return claim.path }

func (claim MaterializedDirectoryClaim) Valid() bool {
	return !claim.admission.IsZero() && claim.path.Valid() && claim.admission.Path() == claim.path.String()
}

// MaterializationFile is a closed projected request. The logical artifact path
// and checkpoint locator are derived together so neither output nor recovery can
// reinterpret a source path as a second mapping authority. Physical destination
// coordinates are intentionally added only by the executor's private claim.
type MaterializationFile struct {
	sourcePath                  ordinaryoutput.SourceCatalogPath
	artifactPath                ordinaryoutput.ArtifactPath
	materializationRelativePath MaterializationRootRelativePath
	descriptor                  content.FileRevisionDescriptor
	target                      FileMaterializationTarget
	parent                      MaterializationFileParent
}

func NewMaterializationFile(
	intent ReceiveIntent,
	sourcePath ordinaryoutput.SourceCatalogPath,
	materializationRelativePath MaterializationRootRelativePath,
	descriptor content.FileRevisionDescriptor,
	session OutputSessionID,
	parent MaterializationFileParent,
) (MaterializationFile, error) {
	projector, err := newDirectTreeCoordinateProjector(intent)
	if err != nil {
		return MaterializationFile{}, ErrInvalidOutputBinding
	}
	return newMaterializationFile(
		projector, sourcePath, materializationRelativePath, descriptor, session, parent,
	)
}

func newMaterializationFile(
	projector directTreeCoordinateProjector,
	sourcePath ordinaryoutput.SourceCatalogPath,
	materializationRelativePath MaterializationRootRelativePath,
	descriptor content.FileRevisionDescriptor,
	session OutputSessionID,
	parent MaterializationFileParent,
) (MaterializationFile, error) {
	artifactPath, projectedRelativePath, err := projector.projectFile(sourcePath, descriptor.FileID(), parent)
	if err != nil || !materializationRelativePath.Valid() || projectedRelativePath != materializationRelativePath {
		return MaterializationFile{}, ErrInvalidOutputBinding
	}
	locator, err := NewPathMaterializationLocator(materializationRelativePath.String())
	if err != nil {
		return MaterializationFile{}, err
	}
	target, err := NewFileMaterializationTarget(session, descriptor, locator)
	if err != nil {
		return MaterializationFile{}, err
	}
	return MaterializationFile{
		sourcePath: sourcePath, artifactPath: artifactPath,
		materializationRelativePath: materializationRelativePath,
		descriptor:                  descriptor, target: target, parent: parent,
	}, nil
}

func (file MaterializationFile) SourcePath() ordinaryoutput.SourceCatalogPath { return file.sourcePath }
func (file MaterializationFile) ArtifactPath() ordinaryoutput.ArtifactPath    { return file.artifactPath }
func (file MaterializationFile) MaterializationRelativePath() MaterializationRootRelativePath {
	return file.materializationRelativePath
}
func (file MaterializationFile) ExpectedSize() uint64                       { return file.descriptor.ExactSize() }
func (file MaterializationFile) Descriptor() content.FileRevisionDescriptor { return file.descriptor }
func (file MaterializationFile) Target() FileMaterializationTarget          { return file.target }
func (file MaterializationFile) Parent() MaterializationFileParent          { return file.parent }
func (file MaterializationFile) ParentMaterialization() MaterializedDirectoryClaim {
	return file.parent.Materialization()
}

func (file MaterializationFile) validProjected() bool {
	if !file.sourcePath.Valid() || !file.artifactPath.Valid() ||
		!file.materializationRelativePath.Valid() || !file.parent.valid() ||
		file.descriptor.ShareInstance().IsZero() || file.descriptor.FileID().IsZero() ||
		file.descriptor.FileRevision().IsZero() || !file.target.valid() ||
		file.target.Descriptor() != file.descriptor ||
		file.target.Locator().Kind() != MaterializationPathLocator ||
		file.target.Locator().CanonicalPath() != file.materializationRelativePath.String() {
		return false
	}
	return true
}

// MaterializationFileMatchesIntent re-projects the authenticated source against
// the session's intent and compares every logical/checkpoint coordinate.
func MaterializationFileMatchesIntent(intent ReceiveIntent, file MaterializationFile) bool {
	projector, err := newDirectTreeCoordinateProjector(intent)
	return err == nil && materializationFileMatchesProjector(projector, file)
}

func materializationFileMatchesProjector(
	projector directTreeCoordinateProjector,
	file MaterializationFile,
) bool {
	if !file.validProjected() {
		return false
	}
	artifact, relative, err := projector.projectFile(file.sourcePath, file.descriptor.FileID(), file.parent)
	return err == nil && artifact == file.artifactPath && relative == file.materializationRelativePath
}

func OrdinaryOutputArtifactPathProjector(
	intent ReceiveIntent,
) (ordinaryoutput.ArtifactPathProjector, error) {
	if intent.IsZero() {
		return ordinaryoutput.ArtifactPathProjector{}, ordinaryoutput.ErrInvalidArtifactProjector
	}
	artifact := intent.ArtifactSpec()
	return ordinaryoutput.NewArtifactPathProjector(intent.SyntheticRoot(), artifact)
}

// OrdinaryOutputSourceNode binds the kind-specific authenticated ID before a
// path can be projected into artifact authority.
func OrdinaryOutputSourceNode(
	kind catalog.NodeKind,
	directory catalog.DirectoryID,
	file catalog.FileID,
	sourcePath ordinaryoutput.SourceCatalogPath,
	role ordinaryoutput.SourceNodeRole,
) (ordinaryoutput.AuthenticatedSourceNode, error) {
	switch kind {
	case catalog.NodeKindDirectory:
		if directory.IsZero() || !file.IsZero() {
			return ordinaryoutput.AuthenticatedSourceNode{}, ordinaryoutput.ErrInvalidAuthenticatedSource
		}
		return ordinaryoutput.NewAuthenticatedDirectorySourceNode(directory, sourcePath, role)
	case catalog.NodeKindFile:
		if file.IsZero() || !directory.IsZero() {
			return ordinaryoutput.AuthenticatedSourceNode{}, ordinaryoutput.ErrInvalidAuthenticatedSource
		}
		return ordinaryoutput.NewAuthenticatedFileSourceNode(file, sourcePath, role)
	default:
		return ordinaryoutput.AuthenticatedSourceNode{}, ordinaryoutput.ErrInvalidAuthenticatedSource
	}
}
