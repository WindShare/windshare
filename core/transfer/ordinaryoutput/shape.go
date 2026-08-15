package ordinaryoutput

import (
	"context"
	"errors"
	"sort"

	"github.com/windshare/windshare/core/catalog"
	"github.com/windshare/windshare/core/session/protocolsession"
	"github.com/windshare/windshare/core/transfer/receivecontract"
)

var ErrInvalidShapeResolution = errors.New("ordinary output shape resolution is invalid")

type ShapeKind uint8

const (
	ShapeSingleFile ShapeKind = iota + 1
	ShapeCompleteDirectory
	ShapePartialDirectory
	ShapeSyntheticSelection
)

func (kind ShapeKind) Valid() bool {
	return kind >= ShapeSingleFile && kind <= ShapeSyntheticSelection
}

type ShapeFallbackReason uint8

const (
	ShapeFallbackNone ShapeFallbackReason = iota
	ShapeFallbackMultipleRoots
	ShapeFallbackSyntheticNearestAncestor
	ShapeFallbackOpaqueAncestryUnprovable
	ShapeFallbackPageBudget
	ShapeFallbackEntryBudget
	ShapeFallbackMetadataBudget
	ShapeFallbackDepthBudget
	ShapeFallbackDirectoryRequestBudget
	ShapeFallbackUnsupportedRuleProof
	ShapeFallbackUnresolvedTarget
	ShapeFallbackIncompleteGeneration
)

func (reason ShapeFallbackReason) Valid() bool {
	return reason >= ShapeFallbackNone && reason <= ShapeFallbackIncompleteGeneration
}

func (reason ShapeFallbackReason) validSynthetic() bool {
	return reason > ShapeFallbackNone && reason.Valid()
}

// ShapeDecision contains only the immutable scalar result of construction-time
// proof. No generation, page, target list, manifest, or content fact survives.
type ShapeDecision struct {
	kind          ShapeKind
	file          catalog.FileID
	directory     catalog.DirectoryID
	sourcePath    string
	preferredName string
	fallback      ShapeFallbackReason
}

func NewSingleFileShape(file catalog.FileID, sourcePath string) (ShapeDecision, error) {
	artifact, err := receivecontract.NewSingleFileDirectoryTree(
		file,
		sourcePath,
		sourceLeaf(sourcePath),
	)
	if err != nil {
		return ShapeDecision{}, errors.Join(ErrInvalidShapeResolution, err)
	}
	tree, _ := artifact.DirectoryTree()
	single, _ := tree.SingleFile()
	return ShapeDecision{
		kind: ShapeSingleFile, file: file, sourcePath: single.SourcePath,
		preferredName: single.SuggestedName,
	}, nil
}

func NewCompleteDirectoryShape(
	directory catalog.DirectoryID,
	sourcePath string,
) (ShapeDecision, error) {
	layout, err := receivecontract.NewCompleteDirectoryResultRoot(directory, sourcePath)
	if err != nil {
		return ShapeDecision{}, errors.Join(ErrInvalidShapeResolution, err)
	}
	return ShapeDecision{
		kind: ShapeCompleteDirectory, directory: layout.DirectoryID(),
		sourcePath: layout.SourcePath(), preferredName: layout.Name(),
	}, nil
}

func NewPartialDirectoryShape(
	directory catalog.DirectoryID,
	sourcePath string,
) (ShapeDecision, error) {
	layout, err := receivecontract.NewDirectorySelectionResultRoot(directory, sourcePath)
	if err != nil {
		return ShapeDecision{}, errors.Join(ErrInvalidShapeResolution, err)
	}
	return ShapeDecision{
		kind: ShapePartialDirectory, directory: layout.DirectoryID(),
		sourcePath: layout.SourcePath(), preferredName: layout.Name(),
	}, nil
}

func NewSyntheticSelectionShape(reason ShapeFallbackReason) (ShapeDecision, error) {
	if !reason.validSynthetic() {
		return ShapeDecision{}, ErrInvalidShapeResolution
	}
	layout := receivecontract.NewSyntheticSelectionResultRoot()
	return ShapeDecision{
		kind: ShapeSyntheticSelection, preferredName: layout.Name(), fallback: reason,
	}, nil
}

func (decision ShapeDecision) Kind() ShapeKind                     { return decision.kind }
func (decision ShapeDecision) FileID() catalog.FileID              { return decision.file }
func (decision ShapeDecision) DirectoryID() catalog.DirectoryID    { return decision.directory }
func (decision ShapeDecision) SourcePath() string                  { return decision.sourcePath }
func (decision ShapeDecision) PreferredName() string               { return decision.preferredName }
func (decision ShapeDecision) FallbackReason() ShapeFallbackReason { return decision.fallback }

func (decision ShapeDecision) Valid() bool {
	var rebuilt ShapeDecision
	var err error
	switch decision.kind {
	case ShapeSingleFile:
		rebuilt, err = NewSingleFileShape(decision.file, decision.sourcePath)
	case ShapeCompleteDirectory:
		rebuilt, err = NewCompleteDirectoryShape(decision.directory, decision.sourcePath)
	case ShapePartialDirectory:
		rebuilt, err = NewPartialDirectoryShape(decision.directory, decision.sourcePath)
	case ShapeSyntheticSelection:
		rebuilt, err = NewSyntheticSelectionShape(decision.fallback)
	default:
		return false
	}
	return err == nil && decision == rebuilt
}

// SelectionMode is deliberately local: the parent transfer package lowers its
// closed canonical selection state into this immutable proof input.
type SelectionMode uint8

const (
	SelectionWholeShare SelectionMode = iota + 1
	SelectionCatalogPaths
	SelectionOpaqueNodes
)

type OpaqueSelectionTarget struct {
	kind     catalog.NodeKind
	nodeID   catalog.NodeID
	selected bool
}

func NewOpaqueDirectoryTarget(
	directory catalog.DirectoryID,
	selected bool,
) (OpaqueSelectionTarget, error) {
	if directory.IsZero() {
		return OpaqueSelectionTarget{}, ErrInvalidShapeResolution
	}
	return OpaqueSelectionTarget{
		kind: catalog.NodeKindDirectory, nodeID: directory.NodeID(), selected: selected,
	}, nil
}

func NewOpaqueFileTarget(file catalog.FileID, selected bool) (OpaqueSelectionTarget, error) {
	if file.IsZero() {
		return OpaqueSelectionTarget{}, ErrInvalidShapeResolution
	}
	return OpaqueSelectionTarget{
		kind: catalog.NodeKindFile, nodeID: file.NodeID(), selected: selected,
	}, nil
}

func (target OpaqueSelectionTarget) Kind() catalog.NodeKind { return target.kind }
func (target OpaqueSelectionTarget) NodeID() catalog.NodeID { return target.nodeID }
func (target OpaqueSelectionTarget) Selected() bool         { return target.selected }

type Selection struct {
	mode            SelectionMode
	share           catalog.ShareInstance
	syntheticRoot   catalog.DirectoryID
	digest          [32]byte
	defaultSelected bool
	pathTargets     []string
	opaqueTargets   []OpaqueSelectionTarget
}

func NewWholeShareSelection(
	share catalog.ShareInstance,
	syntheticRoot catalog.DirectoryID,
	digest [32]byte,
) (Selection, error) {
	selection := Selection{
		mode: SelectionWholeShare, share: share, syntheticRoot: syntheticRoot,
		digest: digest, defaultSelected: true,
	}
	if !selection.valid() {
		return Selection{}, ErrInvalidShapeResolution
	}
	return selection, nil
}

func NewCatalogPathSelection(
	share catalog.ShareInstance,
	syntheticRoot catalog.DirectoryID,
	digest [32]byte,
	targets []string,
) (Selection, error) {
	if len(targets) == 0 {
		return Selection{}, ErrInvalidShapeResolution
	}
	canonicalTargets := make([]string, 0, len(targets))
	seen := make(map[string]struct{}, len(targets))
	for _, target := range targets {
		path, err := NewSourceCatalogPath(target)
		if err != nil {
			return Selection{}, errors.Join(ErrInvalidShapeResolution, err)
		}
		if _, exists := seen[path.String()]; exists {
			return Selection{}, ErrInvalidShapeResolution
		}
		seen[path.String()] = struct{}{}
		canonicalTargets = append(canonicalTargets, path.String())
	}
	sort.Strings(canonicalTargets)
	selection := Selection{
		mode: SelectionCatalogPaths, share: share, syntheticRoot: syntheticRoot,
		digest: digest, pathTargets: canonicalTargets,
	}
	if !selection.valid() {
		return Selection{}, ErrInvalidShapeResolution
	}
	return selection, nil
}

func NewOpaqueNodeSelection(
	share catalog.ShareInstance,
	syntheticRoot catalog.DirectoryID,
	digest [32]byte,
	defaultSelected bool,
	targets []OpaqueSelectionTarget,
) (Selection, error) {
	if len(targets) == 0 {
		return Selection{}, ErrInvalidShapeResolution
	}
	seen := make(map[catalog.NodeID]struct{}, len(targets))
	hasSelected := defaultSelected
	snapshot := make([]OpaqueSelectionTarget, len(targets))
	copy(snapshot, targets)
	for _, target := range snapshot {
		if target.nodeID.IsZero() ||
			(target.kind != catalog.NodeKindDirectory && target.kind != catalog.NodeKindFile) {
			return Selection{}, ErrInvalidShapeResolution
		}
		if _, exists := seen[target.nodeID]; exists {
			return Selection{}, ErrInvalidShapeResolution
		}
		seen[target.nodeID] = struct{}{}
		hasSelected = hasSelected || target.selected
	}
	if !hasSelected {
		return Selection{}, ErrInvalidShapeResolution
	}
	selection := Selection{
		mode: SelectionOpaqueNodes, share: share, syntheticRoot: syntheticRoot,
		digest: digest, defaultSelected: defaultSelected, opaqueTargets: snapshot,
	}
	if !selection.valid() {
		return Selection{}, ErrInvalidShapeResolution
	}
	return selection, nil
}

func (selection Selection) valid() bool {
	if selection.share.IsZero() || selection.syntheticRoot.IsZero() ||
		selection.digest == ([32]byte{}) {
		return false
	}
	switch selection.mode {
	case SelectionWholeShare:
		return selection.defaultSelected &&
			len(selection.pathTargets) == 0 && len(selection.opaqueTargets) == 0
	case SelectionCatalogPaths:
		return !selection.defaultSelected &&
			len(selection.pathTargets) > 0 && len(selection.opaqueTargets) == 0
	case SelectionOpaqueNodes:
		return len(selection.pathTargets) == 0 && len(selection.opaqueTargets) > 0
	default:
		return false
	}
}

func (selection Selection) Digest() [32]byte { return selection.digest }

type ShapeCatalog interface {
	OpenDirectoryPages(context.Context, catalog.DirectoryID) (catalog.DirectoryPageCursor, error)
}

type ShapeTraceStage uint8

const (
	ShapeProbeStarted ShapeTraceStage = iota + 1
	ShapeProofFrozen
	ShapeSyntheticFallback
)

type ShapeTrace struct {
	Stage                      ShapeTraceStage
	ProtocolSessionID          protocolsession.ProtocolSessionID
	SelectionDigest            [32]byte
	Kind                       ShapeKind
	Fallback                   ShapeFallbackReason
	DirectoryRequests          uint32
	AuthenticatedPages         uint32
	AuthenticatedEntries       uint32
	AuthenticatedMetadataBytes uint64
}

type ShapeTracer interface {
	TraceOrdinaryOutputShape(ShapeTrace)
}

type ShapeTraceFunc func(ShapeTrace)

func (trace ShapeTraceFunc) TraceOrdinaryOutputShape(event ShapeTrace) {
	if trace != nil {
		trace(event)
	}
}

type shapeSessionTracer struct {
	session protocolsession.ProtocolSessionID
	next    ShapeTracer
}

// BindShapeTracerToSession adds stable correlation at the authenticated runtime
// boundary without making session identity part of shape authority.
func BindShapeTracerToSession(
	session protocolsession.ProtocolSessionID,
	tracer ShapeTracer,
) ShapeTracer {
	if session.IsZero() || tracer == nil {
		return tracer
	}
	return shapeSessionTracer{session: session, next: tracer}
}

func (tracer shapeSessionTracer) TraceOrdinaryOutputShape(event ShapeTrace) {
	if event.ProtocolSessionID.IsZero() {
		event.ProtocolSessionID = tracer.session
	}
	tracer.next.TraceOrdinaryOutputShape(event)
}
