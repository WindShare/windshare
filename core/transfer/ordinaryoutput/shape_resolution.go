package ordinaryoutput

import (
	"context"
	"errors"
	"sort"
	"strings"

	"github.com/windshare/windshare/core/catalog"
	"github.com/windshare/windshare/core/transfer/catalogwalk"
)

type resolver struct {
	catalog   ShapeCatalog
	selection Selection
	budget    ShapeProbeBudget
	meter     *catalogwalk.Meter
	tracer    ShapeTracer
	claims    map[catalog.NodeID]struct{}
	requests  uint32
}

func ResolveShape(
	ctx context.Context,
	source ShapeCatalog,
	selection Selection,
	budget ShapeProbeBudget,
	tracer ShapeTracer,
) (ShapeDecision, error) {
	if ctx == nil || source == nil || !selection.valid() || !budget.Valid() {
		return ShapeDecision{}, ErrInvalidShapeResolution
	}
	limits, ok := catalogwalk.NewLimits(
		budget.AuthenticatedPages(),
		budget.Entries(),
		budget.AuthenticatedMetadataBytes(),
	)
	if !ok {
		return ShapeDecision{}, ErrInvalidShapeResolution
	}
	meter, ok := catalogwalk.NewMeter(limits)
	if !ok {
		return ShapeDecision{}, ErrInvalidShapeResolution
	}
	state := &resolver{
		catalog: source, selection: selection, budget: budget, meter: meter, tracer: tracer,
		claims: map[catalog.NodeID]struct{}{selection.syntheticRoot.NodeID(): {}},
	}
	state.trace(ShapeTrace{Stage: ShapeProbeStarted})
	var decision ShapeDecision
	var err error
	switch selection.mode {
	case SelectionWholeShare:
		decision, err = state.resolveWholeShare(ctx)
	case SelectionCatalogPaths:
		decision, err = state.resolveCatalogPaths(ctx)
	case SelectionOpaqueNodes:
		decision, err = state.resolveOpaqueNodes(ctx)
	default:
		err = ErrInvalidShapeResolution
	}
	if err != nil {
		return ShapeDecision{}, err
	}
	stage := ShapeProofFrozen
	if decision.Kind() == ShapeSyntheticSelection {
		stage = ShapeSyntheticFallback
	}
	state.trace(ShapeTrace{
		Stage: stage, Kind: decision.Kind(), Fallback: decision.FallbackReason(),
	})
	return decision, nil
}

func (state *resolver) resolveWholeShare(ctx context.Context) (ShapeDecision, error) {
	var root catalog.Entry
	result, fallback, err := state.walkDirectory(
		ctx,
		state.selection.syntheticRoot,
		func(entry catalog.Entry) error {
			if root.NodeID().IsZero() {
				root = entry
			}
			return nil
		},
	)
	if err != nil {
		return ShapeDecision{}, err
	}
	if fallback != ShapeFallbackNone {
		return syntheticShape(fallback)
	}
	if !result.Complete {
		return syntheticShape(ShapeFallbackIncompleteGeneration)
	}
	if result.Directory.EntryCount() != 1 || root.NodeID().IsZero() {
		return syntheticShape(ShapeFallbackMultipleRoots)
	}
	if file, ok := root.FileID(); ok {
		return NewSingleFileShape(file, root.Name())
	}
	if directory, ok := root.DirectoryID(); ok {
		return NewCompleteDirectoryShape(directory, root.Name())
	}
	return ShapeDecision{}, ErrInvalidShapeResolution
}

func (state *resolver) resolveCatalogPaths(ctx context.Context) (ShapeDecision, error) {
	trie := newTargetTrie(state.selection.pathTargets)
	var proof selectionRegionProof
	fallback, err := state.resolvePathTrieChildren(
		ctx,
		state.selection.syntheticRoot,
		"",
		trie,
		1,
		nil,
		&proof,
	)
	if err != nil {
		return ShapeDecision{}, err
	}
	if fallback != ShapeFallbackNone {
		return syntheticShape(fallback)
	}
	return proof.decision()
}

func (state *resolver) resolvePathTrieChildren(
	ctx context.Context,
	parent catalog.DirectoryID,
	parentPath string,
	target *targetTrie,
	depth uint32,
	ancestors []realDirectoryAnchor,
	proof *selectionRegionProof,
) (ShapeFallbackReason, error) {
	if depth > state.budget.Depth() {
		return ShapeFallbackDepthBudget, nil
	}
	matched := make(map[string]catalog.Entry, len(target.children))
	result, fallback, err := state.walkDirectory(ctx, parent, func(entry catalog.Entry) error {
		if _, requested := target.children[entry.Name()]; requested {
			matched[entry.Name()] = entry
		}
		return nil
	})
	if err != nil || fallback != ShapeFallbackNone {
		return fallback, err
	}
	if !result.Complete {
		return ShapeFallbackIncompleteGeneration, nil
	}

	for _, childName := range target.childNames() {
		entry, found := matched[childName]
		if !found {
			return ShapeFallbackUnresolvedTarget, nil
		}
		childTarget := target.children[childName]
		path := childName
		if parentPath != "" {
			path = parentPath + "/" + childName
		}
		if childTarget.terminal {
			proof.addRegion(entry, path, ancestors)
			if entry.Kind() == catalog.NodeKindDirectory {
				// A selected directory already selects its entire subtree, so
				// descendant path targets add no semantic result root.
				continue
			}
			if len(childTarget.children) != 0 {
				return ShapeFallbackUnresolvedTarget, nil
			}
			continue
		}
		directory, ok := entry.DirectoryID()
		if !ok {
			return ShapeFallbackUnresolvedTarget, nil
		}
		nextAncestors := appendDirectoryAnchor(ancestors, directory, path)
		fallback, err := state.resolvePathTrieChildren(
			ctx,
			directory,
			path,
			childTarget,
			depth+1,
			nextAncestors,
			proof,
		)
		if err != nil || fallback != ShapeFallbackNone {
			return fallback, err
		}
	}
	return ShapeFallbackNone, nil
}

func (state *resolver) resolveOpaqueNodes(ctx context.Context) (ShapeDecision, error) {
	targets := make(map[catalog.NodeID]OpaqueSelectionTarget, len(state.selection.opaqueTargets))
	for _, target := range state.selection.opaqueTargets {
		targets[target.nodeID] = target
	}
	search := opaqueSearch{
		resolver: state, targets: targets, remaining: len(targets),
		active: make(map[catalog.DirectoryID]struct{}),
	}
	rootSelected := state.selection.defaultSelected
	if target, targeted := targets[state.selection.syntheticRoot.NodeID()]; targeted {
		if target.kind != catalog.NodeKindDirectory {
			return syntheticShape(ShapeFallbackOpaqueAncestryUnprovable)
		}
		rootSelected = target.selected
		search.remaining--
		delete(search.targets, state.selection.syntheticRoot.NodeID())
		if !rootSelected {
			search.excluded = append(search.excluded, "")
		}
	}
	fallback, err := search.directory(
		ctx,
		state.selection.syntheticRoot,
		"",
		0,
		rootSelected,
		false,
		nil,
	)
	if err != nil {
		return ShapeDecision{}, err
	}
	if fallback != ShapeFallbackNone {
		return syntheticShape(fallback)
	}
	if search.remaining != 0 {
		return syntheticShape(ShapeFallbackUnresolvedTarget)
	}
	search.proof.applyExclusions(search.excluded)
	return search.proof.decision()
}

type opaqueSearch struct {
	resolver  *resolver
	targets   map[catalog.NodeID]OpaqueSelectionTarget
	remaining int
	active    map[catalog.DirectoryID]struct{}
	proof     selectionRegionProof
	excluded  []string
}

type directoryChild struct {
	directory    catalog.DirectoryID
	name         string
	selected     bool
	insideRegion bool
}

func (search *opaqueSearch) directory(
	ctx context.Context,
	directory catalog.DirectoryID,
	path string,
	depth uint32,
	inheritedSelected bool,
	insideRegion bool,
	ancestors []realDirectoryAnchor,
) (ShapeFallbackReason, error) {
	if depth > search.resolver.budget.Depth() {
		return ShapeFallbackDepthBudget, nil
	}
	if _, cycle := search.active[directory]; cycle {
		return ShapeFallbackNone, catalogwalk.ErrTerminalGenerationIntegrity
	}
	search.active[directory] = struct{}{}
	defer delete(search.active, directory)

	children := make([]directoryChild, 0)
	result, fallback, err := search.resolver.walkDirectory(ctx, directory, func(entry catalog.Entry) error {
		child, isDirectory, err := search.observeEntry(
			entry, path, inheritedSelected, insideRegion, ancestors,
		)
		if err != nil {
			return err
		}
		if isDirectory {
			children = append(children, child)
		}
		return nil
	})
	if err != nil {
		if errors.Is(err, ErrInvalidShapeResolution) {
			return ShapeFallbackOpaqueAncestryUnprovable, nil
		}
		return ShapeFallbackNone, err
	}
	if fallback != ShapeFallbackNone {
		return fallback, nil
	}
	if !result.Complete {
		return ShapeFallbackIncompleteGeneration, nil
	}
	if search.remaining == 0 {
		return ShapeFallbackNone, nil
	}
	for _, child := range children {
		childPath := child.name
		if path != "" {
			childPath = path + "/" + child.name
		}
		nextAncestors := appendDirectoryAnchor(ancestors, child.directory, childPath)
		childInsideRegion := child.insideRegion
		fallback, err := search.directory(
			ctx,
			child.directory,
			childPath,
			depth+1,
			child.selected,
			childInsideRegion,
			nextAncestors,
		)
		if err != nil || fallback != ShapeFallbackNone {
			return fallback, err
		}
		if search.remaining == 0 {
			return ShapeFallbackNone, nil
		}
	}
	return ShapeFallbackNone, nil
}

func (search *opaqueSearch) observeEntry(
	entry catalog.Entry,
	parentPath string,
	inheritedSelected bool,
	insideRegion bool,
	ancestors []realDirectoryAnchor,
) (directoryChild, bool, error) {
	selected := inheritedSelected
	direct := false
	entryPath := entry.Name()
	if parentPath != "" {
		entryPath = parentPath + "/" + entry.Name()
	}
	if target, targeted := search.targets[entry.NodeID()]; targeted {
		if target.kind != entry.Kind() {
			return directoryChild{}, false, ErrInvalidShapeResolution
		}
		selected, direct = target.selected, true
		search.remaining--
		delete(search.targets, entry.NodeID())
		if !selected {
			search.excluded = append(search.excluded, entryPath)
		}
	}
	entryInsideRegion := insideRegion
	if selected && (parentPath == "" || !entryInsideRegion) {
		// The synthetic root cannot be an artifact anchor. A selected top-level
		// entry, or the first selected descendant, starts the real selection region.
		search.proof.addRegion(entry, entryPath, ancestors)
		entryInsideRegion = true
	}
	if direct && !selected && insideRegion && inheritedSelected {
		search.proof.partial = true
	}
	child, ok := entry.DirectoryID()
	if !ok {
		return directoryChild{}, false, nil
	}
	return directoryChild{
		directory: child, name: entry.Name(), selected: selected,
		insideRegion: entryInsideRegion,
	}, true, nil
}

type targetTrie struct {
	terminal bool
	children map[string]*targetTrie
}

func newTargetTrie(targets []string) *targetTrie {
	root := &targetTrie{children: make(map[string]*targetTrie)}
	for _, target := range targets {
		node := root
		for _, component := range strings.Split(target, "/") {
			child := node.children[component]
			if child == nil {
				child = &targetTrie{children: make(map[string]*targetTrie)}
				node.children[component] = child
			}
			node = child
		}
		node.terminal = true
	}
	return root
}

func (trie *targetTrie) childNames() []string {
	names := make([]string, 0, len(trie.children))
	for name := range trie.children {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

type realDirectoryAnchor struct {
	directory catalog.DirectoryID
	path      string
}

func appendDirectoryAnchor(
	ancestors []realDirectoryAnchor,
	directory catalog.DirectoryID,
	path string,
) []realDirectoryAnchor {
	result := make([]realDirectoryAnchor, len(ancestors)+1)
	copy(result, ancestors)
	result[len(ancestors)] = realDirectoryAnchor{directory: directory, path: path}
	return result
}

type selectedRegion struct {
	kind      catalog.NodeKind
	nodeID    catalog.NodeID
	directory catalog.DirectoryID
	path      string
}

type selectionRegionProof struct {
	count   uint32
	first   selectedRegion
	common  []realDirectoryAnchor
	partial bool
}

func (proof *selectionRegionProof) applyExclusions(exclusions []string) {
	if proof.count != 1 || proof.first.kind != catalog.NodeKindDirectory {
		return
	}
	for _, path := range exclusions {
		if path == proof.first.path || strings.HasPrefix(path, proof.first.path+"/") {
			proof.partial = true
			return
		}
	}
}

func (proof *selectionRegionProof) addRegion(
	entry catalog.Entry,
	path string,
	ancestors []realDirectoryAnchor,
) {
	region := selectedRegion{kind: entry.Kind(), nodeID: entry.NodeID(), path: path}
	regionAncestors := ancestors
	if directory, ok := entry.DirectoryID(); ok {
		region.directory = directory
		regionAncestors = appendDirectoryAnchor(ancestors, directory, path)
	}
	proof.count++
	if proof.count == 1 {
		proof.first = region
		proof.common = append([]realDirectoryAnchor(nil), regionAncestors...)
		return
	}
	common := len(proof.common)
	if len(regionAncestors) < common {
		common = len(regionAncestors)
	}
	for index := 0; index < common; index++ {
		if proof.common[index] != regionAncestors[index] {
			common = index
			break
		}
	}
	proof.common = proof.common[:common]
}

func (proof selectionRegionProof) decision() (ShapeDecision, error) {
	switch {
	case proof.count == 0:
		return syntheticShape(ShapeFallbackUnsupportedRuleProof)
	case proof.count == 1 && proof.first.kind == catalog.NodeKindFile:
		return NewSingleFileShape(catalog.FileID(proof.first.nodeID), proof.first.path)
	case proof.count == 1 && proof.first.kind == catalog.NodeKindDirectory && !proof.partial:
		return NewCompleteDirectoryShape(proof.first.directory, proof.first.path)
	case proof.count == 1 && proof.first.kind == catalog.NodeKindDirectory:
		return NewPartialDirectoryShape(proof.first.directory, proof.first.path)
	case len(proof.common) == 0 || proof.common[len(proof.common)-1].path == proof.first.path:
		return syntheticShape(ShapeFallbackSyntheticNearestAncestor)
	default:
		anchor := proof.common[len(proof.common)-1]
		return NewPartialDirectoryShape(anchor.directory, anchor.path)
	}
}

func (state *resolver) walkDirectory(
	ctx context.Context,
	directory catalog.DirectoryID,
	visit func(catalog.Entry) error,
) (result catalogwalk.TerminalGeneration, fallback ShapeFallbackReason, resultErr error) {
	if state.requests >= state.budget.DirectoryRequests() {
		return catalogwalk.TerminalGeneration{}, ShapeFallbackDirectoryRequestBudget, nil
	}
	state.requests++
	cursor, err := state.catalog.OpenDirectoryPages(ctx, directory)
	if err != nil {
		return catalogwalk.TerminalGeneration{}, ShapeFallbackNone, err
	}
	if cursor == nil {
		return catalogwalk.TerminalGeneration{}, ShapeFallbackNone, ErrInvalidShapeResolution
	}
	provisionalClaims := make([]catalog.NodeID, 0)
	result, err = catalogwalk.ReadTerminalGeneration(
		ctx,
		cursor,
		state.selection.share,
		directory,
		state.meter,
		func(entry catalog.Entry) error {
			if _, duplicate := state.claims[entry.NodeID()]; duplicate {
				return catalogwalk.ErrTerminalGenerationIntegrity
			}
			state.claims[entry.NodeID()] = struct{}{}
			provisionalClaims = append(provisionalClaims, entry.NodeID())
			return visit(entry)
		},
	)
	if err != nil {
		for _, identity := range provisionalClaims {
			delete(state.claims, identity)
		}
		return catalogwalk.TerminalGeneration{}, ShapeFallbackNone, err
	}
	if result.Exhausted != catalogwalk.BudgetWithinLimits || !result.Complete {
		for _, identity := range provisionalClaims {
			delete(state.claims, identity)
		}
	}
	switch result.Exhausted {
	case catalogwalk.BudgetWithinLimits:
		return result, ShapeFallbackNone, nil
	case catalogwalk.BudgetAuthenticatedPages:
		return catalogwalk.TerminalGeneration{}, ShapeFallbackPageBudget, nil
	case catalogwalk.BudgetEntries:
		return catalogwalk.TerminalGeneration{}, ShapeFallbackEntryBudget, nil
	case catalogwalk.BudgetAuthenticatedMetadata:
		return catalogwalk.TerminalGeneration{}, ShapeFallbackMetadataBudget, nil
	default:
		return catalogwalk.TerminalGeneration{}, ShapeFallbackNone, ErrInvalidShapeResolution
	}
}

func (state *resolver) trace(event ShapeTrace) {
	if state == nil || state.tracer == nil {
		return
	}
	usage := state.meter.Usage()
	event.SelectionDigest = state.selection.digest
	event.DirectoryRequests = state.requests
	event.AuthenticatedPages = usage.AuthenticatedPages
	event.AuthenticatedEntries = usage.Entries
	event.AuthenticatedMetadataBytes = usage.AuthenticatedMetadataBytes
	// Diagnostics must never become shape authority.
	defer func() { _ = recover() }()
	state.tracer.TraceOrdinaryOutputShape(event)
}

func syntheticShape(reason ShapeFallbackReason) (ShapeDecision, error) {
	return NewSyntheticSelectionShape(reason)
}
