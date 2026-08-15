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

func (request DirectoryMaterializationRequest) ParentMaterialization() MaterializedDirectoryClaim {
	return request.parentMaterialization
}

// NewDirectoryMaterializationRequest applies the frozen projector at the only
// transfer-to-output boundary. Traverse-only requests carry no artifact claim,
// and rejected source values never become output requests.
func NewDirectoryMaterializationRequest(
	projector ordinaryoutput.ArtifactPathProjector,
	source AuthenticatedSourceDirectory,
	role ordinaryoutput.SourceNodeRole,
	parent MaterializedDirectoryClaim,
) (DirectoryMaterializationRequest, error) {
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
	projector ordinaryoutput.ArtifactPathProjector,
	source AuthenticatedSourceDirectory,
	role ordinaryoutput.SourceNodeRole,
	parent MaterializedDirectoryClaim,
) (DirectoryMaterializationRequest, ordinaryoutput.ArtifactPathProjection, error) {
	sourcePath := source.SourcePath
	if !sourcePath.Valid() {
		return DirectoryMaterializationRequest{}, ordinaryoutput.ArtifactPathProjection{},
			ordinaryoutput.ErrInvalidSourceCatalogPath
	}
	node, err := OrdinaryOutputSourceNode(
		catalog.NodeKindDirectory, source.DirectoryID, catalog.FileID{}, sourcePath, role,
	)
	if err != nil {
		return DirectoryMaterializationRequest{}, ordinaryoutput.ArtifactPathProjection{}, err
	}
	source.SourcePath = sourcePath
	projection := projector.Project(node)
	if projection.Kind() == ordinaryoutput.ArtifactReject {
		return DirectoryMaterializationRequest{}, projection, nil
	}
	return DirectoryMaterializationRequest{
		source: source, role: role, projection: projection, parentMaterialization: parent,
	}, projection, nil
}

func (request DirectoryMaterializationRequest) matchesProjector(
	projector ordinaryoutput.ArtifactPathProjector,
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
	projector, err := OrdinaryOutputArtifactPathProjector(intent)
	return err == nil && DirectoryMaterializationMatchesProjector(projector, request)
}

func DirectoryMaterializationMatchesProjector(
	projector ordinaryoutput.ArtifactPathProjector,
	request DirectoryMaterializationRequest,
) bool {
	return request.matchesProjector(projector)
}

// MaterializedDirectoryClaim can only be derived from the exact projected
// directory request that produced its authenticated admission.
type MaterializedDirectoryClaim struct {
	admission    DirectoryAdmission
	artifactPath ordinaryoutput.ArtifactPath
}

func NewMaterializedDirectoryClaim(
	admission DirectoryAdmission,
	request DirectoryMaterializationRequest,
) (MaterializedDirectoryClaim, error) {
	artifactPath, materialized := request.projection.ArtifactPath()
	source := request.source
	var expectedParentToken []byte
	if !source.ParentAdmission.IsZero() {
		expectedParentToken = source.ParentAdmission.Bytes()
	}
	if !materialized || admission.IsZero() || admission.DirectoryID() != source.DirectoryID ||
		admission.Generation() != source.Generation || admission.Path() != source.SourcePath.String() ||
		admission.ModifiedTime() != source.ModifiedTime ||
		!bytes.Equal(admission.ParentToken(), expectedParentToken) {
		return MaterializedDirectoryClaim{}, ErrInvalidOutputBinding
	}
	return MaterializedDirectoryClaim{admission: admission, artifactPath: artifactPath}, nil
}

func (claim MaterializedDirectoryClaim) Admission() DirectoryAdmission { return claim.admission }

func (claim MaterializedDirectoryClaim) ArtifactPath() ordinaryoutput.ArtifactPath {
	return claim.artifactPath
}

func (claim MaterializedDirectoryClaim) Valid() bool {
	return !claim.admission.IsZero() && claim.artifactPath.Valid()
}

// MaterializationFile is a closed projected request. The logical artifact path
// and checkpoint locator are derived together so neither output nor recovery can
// reinterpret a source path as a second mapping authority. Physical destination
// coordinates are intentionally added only by the executor's private claim.
type MaterializationFile struct {
	sourcePath            ordinaryoutput.SourceCatalogPath
	artifactPath          ordinaryoutput.ArtifactPath
	descriptor            content.FileRevisionDescriptor
	target                FileMaterializationTarget
	sourceParentAdmission DirectoryAdmission
	parentMaterialization MaterializedDirectoryClaim
}

func NewMaterializationFile(
	projector ordinaryoutput.ArtifactPathProjector,
	sourcePath ordinaryoutput.SourceCatalogPath,
	descriptor content.FileRevisionDescriptor,
	session OutputSessionID,
	sourceParent DirectoryAdmission,
	parentMaterialization MaterializedDirectoryClaim,
) (MaterializationFile, error) {
	node, err := OrdinaryOutputSourceNode(
		catalog.NodeKindFile, catalog.DirectoryID{}, descriptor.FileID(), sourcePath,
		ordinaryoutput.SourceNodeSelected,
	)
	if err != nil || sourceParent.IsZero() {
		return MaterializationFile{}, ErrInvalidOutputBinding
	}
	projection := projector.Project(node)
	artifactPath, materialized := projection.ArtifactPath()
	if !materialized {
		return MaterializationFile{}, ErrInvalidOutputBinding
	}
	locator, err := NewPathMaterializationLocator(artifactPath.String())
	if err != nil {
		return MaterializationFile{}, err
	}
	target, err := NewFileMaterializationTarget(session, descriptor, locator)
	if err != nil {
		return MaterializationFile{}, err
	}
	return MaterializationFile{
		sourcePath: sourcePath, artifactPath: artifactPath, descriptor: descriptor, target: target,
		sourceParentAdmission: sourceParent, parentMaterialization: parentMaterialization,
	}, nil
}

func (file MaterializationFile) SourcePath() ordinaryoutput.SourceCatalogPath { return file.sourcePath }
func (file MaterializationFile) ArtifactPath() ordinaryoutput.ArtifactPath    { return file.artifactPath }
func (file MaterializationFile) ExpectedSize() uint64                         { return file.descriptor.ExactSize() }
func (file MaterializationFile) Descriptor() content.FileRevisionDescriptor   { return file.descriptor }
func (file MaterializationFile) Target() FileMaterializationTarget            { return file.target }
func (file MaterializationFile) SourceParentAdmission() DirectoryAdmission {
	return file.sourceParentAdmission
}
func (file MaterializationFile) ParentMaterialization() MaterializedDirectoryClaim {
	return file.parentMaterialization
}

func (file MaterializationFile) validProjected() bool {
	if !file.sourcePath.Valid() || !file.artifactPath.Valid() || file.sourceParentAdmission.IsZero() ||
		file.descriptor.ShareInstance().IsZero() || file.descriptor.FileID().IsZero() ||
		file.descriptor.FileRevision().IsZero() || !file.target.valid() ||
		file.target.Descriptor() != file.descriptor ||
		file.target.Locator().Kind() != MaterializationPathLocator ||
		file.target.Locator().CanonicalPath() != file.artifactPath.String() {
		return false
	}
	return true
}

// MaterializationFileMatchesIntent re-projects the authenticated source against
// the session's intent and compares every logical/checkpoint coordinate.
func MaterializationFileMatchesIntent(intent ReceiveIntent, file MaterializationFile) bool {
	projector, err := OrdinaryOutputArtifactPathProjector(intent)
	return err == nil && MaterializationFileMatchesProjector(projector, file)
}

func MaterializationFileMatchesProjector(
	projector ordinaryoutput.ArtifactPathProjector,
	file MaterializationFile,
) bool {
	if !file.validProjected() {
		return false
	}
	node, err := OrdinaryOutputSourceNode(
		catalog.NodeKindFile, catalog.DirectoryID{}, file.descriptor.FileID(), file.sourcePath,
		ordinaryoutput.SourceNodeSelected,
	)
	if err != nil {
		return false
	}
	projected, materialized := projector.Project(node).ArtifactPath()
	return materialized && projected == file.artifactPath
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
