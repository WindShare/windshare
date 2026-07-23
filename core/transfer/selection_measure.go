package transfer

import (
	"context"
	"errors"

	"github.com/windshare/windshare/core/catalog"
)

var ErrInvalidSelectionMeasurement = errors.New("selection measurement configuration is invalid")

type SelectionMeasurementConfig struct {
	ShareInstance catalog.ShareInstance
	SyntheticRoot catalog.DirectoryID
	Rules         SelectionRules
	Catalog       CatalogReader
}

// MeasureSelection discovers only enough authenticated catalog state to make
// the connectivity admission decision. Large is absorbing, so continuing the
// walk after either exclusive threshold would add latency without changing the
// policy outcome. Small remains reserved for a complete, failure-free walk.
func MeasureSelection(ctx context.Context, config SelectionMeasurementConfig) (SelectionMeasure, error) {
	return measureSelection(ctx, config, nil)
}

func measureSelection(
	ctx context.Context,
	config SelectionMeasurementConfig,
	observe func(SelectionMeasure),
) (SelectionMeasure, error) {
	if config.ShareInstance.IsZero() || config.SyntheticRoot.IsZero() ||
		!config.Rules.validSnapshot() || config.Catalog == nil {
		return SelectionMeasure{}, ErrInvalidSelectionMeasurement
	}
	if err := ctx.Err(); err != nil {
		return SelectionMeasure{}, err
	}
	if !config.Rules.HasSelection() {
		measure := SelectionMeasure{DiscoveryTerminalSuccess: true}
		notifySelectionMeasure(observe, measure)
		return measure, nil
	}

	measurer := selectionMeasurer{
		share: config.ShareInstance, rules: config.Rules,
		catalog: config.Catalog, observe: observe,
		claims:             newSelectionIdentityClaims(config.SyntheticRoot),
		matchedPaths:       make(map[string]struct{}),
		matchedDirectories: make(map[catalog.DirectoryID]struct{}), matchedFiles: make(map[catalog.FileID]struct{}),
	}
	rootSelected := config.Rules.DirectorySelectedAt(config.SyntheticRoot, "", config.Rules.DefaultSelected())
	decision, err := measurer.walkDirectory(ctx, config.SyntheticRoot, "", rootSelected)
	if err != nil {
		notifySelectionMeasure(observe, measurer.measure)
		return measurer.measure, err
	}
	if decision == continueSelectionTraversal && !measurer.discoveryFailed {
		if missing := config.Rules.missingTargetsError(
			measurer.matchedPaths, measurer.matchedDirectories, measurer.matchedFiles,
		); missing != nil {
			notifySelectionMeasure(observe, measurer.measure)
			return measurer.measure, missing
		}
		measurer.measure.DiscoveryTerminalSuccess = true
	}
	notifySelectionMeasure(observe, measurer.measure)
	return measurer.measure, nil
}

func notifySelectionMeasure(observe func(SelectionMeasure), measure SelectionMeasure) {
	if observe != nil {
		observe(measure)
	}
}

type selectionMeasurer struct {
	share   catalog.ShareInstance
	rules   SelectionRules
	catalog CatalogReader
	observe func(SelectionMeasure)

	measure            SelectionMeasure
	discoveryFailed    bool
	claims             *selectionIdentityClaims
	matchedPaths       map[string]struct{}
	matchedDirectories map[catalog.DirectoryID]struct{}
	matchedFiles       map[catalog.FileID]struct{}
}

type directoryAcquisitionStatus uint8

const (
	directoryAcquisitionUnavailable directoryAcquisitionStatus = iota
	directoryAcquisitionReady
)

type selectionTraversalDecision uint8

const (
	continueSelectionTraversal selectionTraversalDecision = iota
	stopSelectionTraversal
)

type directoryWalkState struct {
	path     string
	selected bool
	snapshot catalog.DirectorySnapshot
}

func (m *selectionMeasurer) walkDirectory(
	ctx context.Context,
	directory catalog.DirectoryID,
	path string,
	selected bool,
) (selectionTraversalDecision, error) {
	snapshot, status, err := m.acquireDirectory(ctx, directory)
	if err != nil {
		return stopSelectionTraversal, err
	}
	if status == directoryAcquisitionUnavailable {
		return continueSelectionTraversal, nil
	}
	walk, err := m.startDirectoryWalk(snapshot, directory, path, selected)
	if err != nil {
		return stopSelectionTraversal, err
	}
	decision, err := m.measureDirectoryFiles(ctx, walk)
	if err != nil || decision == stopSelectionTraversal {
		return decision, err
	}
	return m.walkDirectoryChildren(ctx, walk)
}

func (m *selectionMeasurer) acquireDirectory(
	ctx context.Context,
	directory catalog.DirectoryID,
) (catalog.DirectorySnapshot, directoryAcquisitionStatus, error) {
	if err := ctx.Err(); err != nil {
		return catalog.DirectorySnapshot{}, directoryAcquisitionUnavailable, err
	}
	snapshot, release, err := m.catalog.AcquireDirectory(ctx, directory)
	if release == nil {
		return catalog.DirectorySnapshot{}, directoryAcquisitionUnavailable,
			errors.Join(NewJobDependencyContractError(ErrCatalogLeaseContract), err)
	}
	release()
	if err != nil {
		if isJobTerminalError(err) || !isDirectoryDiscoveryFailure(err) {
			return catalog.DirectorySnapshot{}, directoryAcquisitionUnavailable, err
		}
		// A scoped directory failure makes completeness Unknown, but independent
		// siblings can still prove that the selection is Large.
		m.discoveryFailed = true
		return catalog.DirectorySnapshot{}, directoryAcquisitionUnavailable, nil
	}
	return snapshot, directoryAcquisitionReady, nil
}

func (m *selectionMeasurer) startDirectoryWalk(
	snapshot catalog.DirectorySnapshot,
	directory catalog.DirectoryID,
	path string,
	selected bool,
) (directoryWalkState, error) {
	if snapshot.ShareInstance() != m.share || snapshot.DirectoryID() != directory || snapshot.PageCount() == 0 {
		return directoryWalkState{}, NewSessionFailure(ErrCatalogIdentity)
	}
	if m.rules.isSelectedDirectoryTarget(directory) {
		m.matchedDirectories[directory] = struct{}{}
	}
	if snapshot.OmittedCount() != 0 {
		m.discoveryFailed = true
	}
	return directoryWalkState{
		path:     path,
		selected: selected,
		snapshot: snapshot,
	}, nil
}

func (m *selectionMeasurer) measureDirectoryFiles(
	ctx context.Context,
	walk directoryWalkState,
) (selectionTraversalDecision, error) {
	// Files are measured across the complete authenticated snapshot before any
	// descendant load because Large is absorbing and no later work can reverse it.
	for pageIndex := 0; pageIndex < walk.snapshot.PageCount(); pageIndex++ {
		page, err := selectionMeasurementPage(walk.snapshot, pageIndex)
		if err != nil {
			return stopSelectionTraversal, err
		}
		for _, entry := range page.Entries() {
			if err := ctx.Err(); err != nil {
				return stopSelectionTraversal, err
			}
			decision, err := m.measureDirectoryEntry(walk, entry)
			if err != nil || decision == stopSelectionTraversal {
				return decision, err
			}
		}
	}
	return continueSelectionTraversal, nil
}

func (m *selectionMeasurer) measureDirectoryEntry(
	walk directoryWalkState,
	entry catalog.Entry,
) (selectionTraversalDecision, error) {
	file, isFile, err := m.claimEntryTargets(entry)
	if err != nil {
		return stopSelectionTraversal, err
	}
	if !isFile {
		return continueSelectionTraversal, nil
	}
	filePath, err := appendOutputPath(walk.path, entry.Name())
	if err != nil {
		return stopSelectionTraversal, NewSessionFailure(ErrCatalogIdentity)
	}
	if m.rules.isPathTarget(filePath) {
		m.matchedPaths[filePath] = struct{}{}
	}
	if !m.rules.FileSelectedAt(file, filePath, walk.selected) {
		return continueSelectionTraversal, nil
	}
	m.measure.addDiscoveredFile(entry.ExpectedSize())
	notifySelectionMeasure(m.observe, m.measure)
	if m.measure.Class() == SelectionLarge {
		return stopSelectionTraversal, nil
	}
	return continueSelectionTraversal, nil
}

func (m *selectionMeasurer) claimEntryTargets(entry catalog.Entry) (catalog.FileID, bool, error) {
	if err := m.claims.claim(entry.NodeID()); err != nil {
		return catalog.FileID{}, false, err
	}
	if child, isDirectory := entry.DirectoryID(); isDirectory && m.rules.isSelectedDirectoryTarget(child) {
		m.matchedDirectories[child] = struct{}{}
	}
	file, isFile := entry.FileID()
	if isFile && m.rules.isSelectedFileTarget(file) {
		m.matchedFiles[file] = struct{}{}
	}
	return file, isFile, nil
}

func (m *selectionMeasurer) walkDirectoryChildren(
	ctx context.Context,
	walk directoryWalkState,
) (selectionTraversalDecision, error) {
	for pageIndex := 0; pageIndex < walk.snapshot.PageCount(); pageIndex++ {
		page, err := selectionMeasurementPage(walk.snapshot, pageIndex)
		if err != nil {
			return stopSelectionTraversal, err
		}
		for _, entry := range page.Entries() {
			if err := ctx.Err(); err != nil {
				return stopSelectionTraversal, err
			}
			decision, err := m.walkDirectoryEntry(ctx, walk, entry)
			if err != nil || decision == stopSelectionTraversal {
				return decision, err
			}
		}
	}
	return continueSelectionTraversal, nil
}

func (m *selectionMeasurer) walkDirectoryEntry(
	ctx context.Context,
	parent directoryWalkState,
	entry catalog.Entry,
) (selectionTraversalDecision, error) {
	child, isDirectory := entry.DirectoryID()
	if !isDirectory {
		return continueSelectionTraversal, nil
	}
	childPath, err := appendOutputPath(parent.path, entry.Name())
	if err != nil {
		return stopSelectionTraversal, NewSessionFailure(ErrCatalogIdentity)
	}
	if m.rules.isPathTarget(childPath) {
		m.matchedPaths[childPath] = struct{}{}
	}
	childSelected := m.rules.DirectorySelectedAt(child, childPath, parent.selected)
	if !m.rules.ShouldDiscoverDirectoryAt(child, childPath, childSelected) {
		return continueSelectionTraversal, nil
	}
	return m.walkDirectory(ctx, child, childPath, childSelected)
}

func selectionMeasurementPage(
	snapshot catalog.DirectorySnapshot,
	pageIndex int,
) (catalog.CatalogPage, error) {
	page, ok := snapshot.Page(uint32(pageIndex))
	if !ok {
		return catalog.CatalogPage{}, NewSessionFailure(ErrCatalogIdentity)
	}
	return page, nil
}
