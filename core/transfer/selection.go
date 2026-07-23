package transfer

import (
	"errors"
	"fmt"
	"math"
	"sort"
	"strings"
	"sync"

	"github.com/windshare/windshare/core/catalog"
)

const (
	SmallTransferFileLimit      = uint64(30)
	SmallTransferByteLimit      = uint64(8) << 20
	MaxSelectionRuleOverrides   = catalog.MaxRootSlots
	MaxSelectionPathTargets     = catalog.MaxRootSlots
	MaxSelectionPathTargetBytes = catalog.MaxSelectedRootNamesBytes
)

var (
	ErrInvalidSelectionRules  = errors.New("transfer selection rules are invalid")
	ErrSelectionTargetMissing = errors.New("transfer selection target was not found")
)

type SelectionOverride struct {
	DirectoryID catalog.DirectoryID
	FileID      catalog.FileID
	Selected    bool

	// Ancestors carries UI provenance into structural validation, but opaque IDs
	// do not make the chain independently trustworthy enough to prune discovery.
	Ancestors []catalog.DirectoryID
}

type SelectionMode uint8

const (
	SelectionByNodeID SelectionMode = iota + 1
	SelectionByCatalogPath
)

// SelectionRules is a frozen job selector with exactly one authority mode.
// Node-ID rules consume authenticated opaque catalog identities. Catalog-path
// targets are bounded local CLI intent matched only against authenticated
// catalog names; they are neither wire authority nor local-filesystem paths.
type SelectionRules struct {
	valid               bool
	mode                SelectionMode
	defaultSelected     bool
	directories         map[catalog.DirectoryID]bool
	files               map[catalog.FileID]bool
	pathTargets         []string
	pathTargetSet       map[string]struct{}
	hasSelection        bool
	hasSelectedOverride bool
}

func NewPathSelectionRules(paths []string) (SelectionRules, error) {
	if len(paths) == 0 || len(paths) > MaxSelectionPathTargets {
		return SelectionRules{}, ErrInvalidSelectionRules
	}
	targetSet := make(map[string]struct{}, len(paths))
	var totalBytes uint64
	for _, path := range paths {
		canonical, err := catalog.CanonicalPath(path)
		if err != nil {
			return SelectionRules{}, errors.Join(ErrInvalidSelectionRules, err)
		}
		if _, duplicate := targetSet[canonical]; duplicate {
			continue
		}
		totalBytes += uint64(len(canonical))
		if totalBytes > MaxSelectionPathTargetBytes {
			return SelectionRules{}, ErrInvalidSelectionRules
		}
		targetSet[canonical] = struct{}{}
	}
	targets := make([]string, 0, len(targetSet))
	for target := range targetSet {
		targets = append(targets, target)
	}
	sort.Strings(targets)
	return SelectionRules{
		valid: true, mode: SelectionByCatalogPath,
		directories: make(map[catalog.DirectoryID]bool), files: make(map[catalog.FileID]bool),
		pathTargets: targets, pathTargetSet: targetSet, hasSelection: true,
	}, nil
}

func NewSelectionRules(defaultSelected bool, overrides []SelectionOverride) (SelectionRules, error) {
	if len(overrides) > MaxSelectionRuleOverrides {
		return SelectionRules{}, ErrInvalidSelectionRules
	}
	rules := SelectionRules{
		valid:           true,
		mode:            SelectionByNodeID,
		defaultSelected: defaultSelected,
		directories:     make(map[catalog.DirectoryID]bool),
		files:           make(map[catalog.FileID]bool),
		hasSelection:    defaultSelected,
	}
	seen := make(map[catalog.NodeID]struct{}, len(overrides))
	for _, override := range overrides {
		hasDirectory := !override.DirectoryID.IsZero()
		hasFile := !override.FileID.IsZero()
		if hasDirectory == hasFile {
			return SelectionRules{}, ErrInvalidSelectionRules
		}
		var node catalog.NodeID
		if hasDirectory {
			node = override.DirectoryID.NodeID()
			rules.directories[override.DirectoryID] = override.Selected
		} else {
			node = override.FileID.NodeID()
			rules.files[override.FileID] = override.Selected
		}
		if _, duplicate := seen[node]; duplicate {
			return SelectionRules{}, ErrInvalidSelectionRules
		}
		seen[node] = struct{}{}
		if len(override.Ancestors) > catalog.MaxPathDepth {
			return SelectionRules{}, ErrInvalidSelectionRules
		}
		ancestorSeen := make(map[catalog.DirectoryID]struct{}, len(override.Ancestors))
		for _, ancestor := range override.Ancestors {
			if ancestor.IsZero() {
				return SelectionRules{}, ErrInvalidSelectionRules
			}
			if _, duplicate := ancestorSeen[ancestor]; duplicate {
				return SelectionRules{}, ErrInvalidSelectionRules
			}
			ancestorSeen[ancestor] = struct{}{}
		}
		if override.Selected {
			rules.hasSelection = true
			rules.hasSelectedOverride = true
		}
	}
	return rules, nil
}

func (r SelectionRules) DefaultSelected() bool { return r.defaultSelected }

func (r SelectionRules) Mode() SelectionMode { return r.mode }

func (r SelectionRules) DirectorySelected(id catalog.DirectoryID, inherited bool) bool {
	return r.DirectorySelectedAt(id, "", inherited)
}

func (r SelectionRules) DirectorySelectedAt(id catalog.DirectoryID, path string, inherited bool) bool {
	if selected, overridden := r.directories[id]; overridden {
		return selected
	}
	if _, selected := r.pathTargetSet[path]; selected {
		return true
	}
	return inherited
}

func (r SelectionRules) FileSelected(id catalog.FileID, inherited bool) bool {
	return r.FileSelectedAt(id, "", inherited)
}

func (r SelectionRules) FileSelectedAt(id catalog.FileID, path string, inherited bool) bool {
	if selected, overridden := r.files[id]; overridden {
		return selected
	}
	if _, selected := r.pathTargetSet[path]; selected {
		return true
	}
	return inherited
}

func (r SelectionRules) ShouldDiscoverDirectory(id catalog.DirectoryID, selected bool) bool {
	return r.ShouldDiscoverDirectoryAt(id, "", selected)
}

func (r SelectionRules) ShouldDiscoverDirectoryAt(id catalog.DirectoryID, path string, selected bool) bool {
	_ = id
	if selected {
		return true
	}
	if r.hasPathDescendant(path) {
		return true
	}
	// Opaque IDs cannot prove that caller-provided ancestry is complete. Treating
	// Ancestors as pruning authority would let one omitted parent silently hide an
	// explicitly selected descendant, so it remains advisory metadata only.
	return r.hasSelectedOverride
}

func (r SelectionRules) hasPathDescendant(path string) bool {
	if len(r.pathTargets) == 0 {
		return false
	}
	prefix := path
	if prefix != "" {
		prefix += "/"
	}
	index := sort.SearchStrings(r.pathTargets, prefix)
	return index < len(r.pathTargets) && strings.HasPrefix(r.pathTargets[index], prefix)
}

func (r SelectionRules) isPathTarget(path string) bool {
	_, selected := r.pathTargetSet[path]
	return selected
}

func (r SelectionRules) isSelectedDirectoryTarget(directory catalog.DirectoryID) bool {
	selected, targeted := r.directories[directory]
	return targeted && selected
}

func (r SelectionRules) isSelectedFileTarget(file catalog.FileID) bool {
	selected, targeted := r.files[file]
	return targeted && selected
}

func (r SelectionRules) missingPathTargets(matched map[string]struct{}) []string {
	missing := make([]string, 0)
	for _, target := range r.pathTargets {
		if _, found := matched[target]; !found {
			missing = append(missing, target)
		}
	}
	return missing
}

func (r SelectionRules) missingSelectedDirectoryTargets(
	matched map[catalog.DirectoryID]struct{},
) []catalog.DirectoryID {
	missing := make([]catalog.DirectoryID, 0)
	for target, selected := range r.directories {
		if _, found := matched[target]; selected && !found {
			missing = append(missing, target)
		}
	}
	sort.Slice(missing, func(left, right int) bool {
		return strings.Compare(string(missing[left].Bytes()), string(missing[right].Bytes())) < 0
	})
	return missing
}

func (r SelectionRules) missingSelectedFileTargets(matched map[catalog.FileID]struct{}) []catalog.FileID {
	missing := make([]catalog.FileID, 0)
	for target, selected := range r.files {
		if _, found := matched[target]; selected && !found {
			missing = append(missing, target)
		}
	}
	sort.Slice(missing, func(left, right int) bool {
		return strings.Compare(string(missing[left].Bytes()), string(missing[right].Bytes())) < 0
	})
	return missing
}

func (r SelectionRules) missingTargetsError(
	matchedPaths map[string]struct{},
	matchedDirectories map[catalog.DirectoryID]struct{},
	matchedFiles map[catalog.FileID]struct{},
) error {
	paths := r.missingPathTargets(matchedPaths)
	directories := r.missingSelectedDirectoryTargets(matchedDirectories)
	files := r.missingSelectedFileTargets(matchedFiles)
	if len(paths) == 0 && len(directories) == 0 && len(files) == 0 {
		return nil
	}
	labels := make([]string, 0, len(paths)+len(directories)+len(files))
	for _, path := range paths {
		labels = append(labels, "path "+path)
	}
	for _, directory := range directories {
		labels = append(labels, fmt.Sprintf("directory %x", directory.Bytes()))
	}
	for _, file := range files {
		labels = append(labels, fmt.Sprintf("file %x", file.Bytes()))
	}
	return fmt.Errorf("%w: %s", ErrSelectionTargetMissing, strings.Join(labels, ", "))
}

func (r SelectionRules) HasSelection() bool { return r.hasSelection }

func (r SelectionRules) validSnapshot() bool {
	if !r.valid {
		return false
	}
	switch r.mode {
	case SelectionByNodeID:
		return len(r.pathTargets) == 0 && len(r.pathTargetSet) == 0
	case SelectionByCatalogPath:
		return !r.defaultSelected && len(r.directories) == 0 && len(r.files) == 0 &&
			len(r.pathTargets) != 0 && !r.hasSelectedOverride
	default:
		return false
	}
}

type SelectionClass uint8

const (
	SelectionUnknown SelectionClass = iota
	SelectionSmall
	SelectionLarge
)

type SelectionMeasure struct {
	DiscoveredFiles          uint64
	DiscoveredBytes          uint64
	DiscoveryTerminalSuccess bool
	overflowed               bool
}

func (m SelectionMeasure) Class() SelectionClass {
	if m.overflowed || m.DiscoveredFiles >= SmallTransferFileLimit || m.DiscoveredBytes >= SmallTransferByteLimit {
		return SelectionLarge
	}
	if m.DiscoveryTerminalSuccess {
		return SelectionSmall
	}
	return SelectionUnknown
}

func (m *SelectionMeasure) addDiscoveredFile(size uint64) {
	if m.DiscoveredFiles == math.MaxUint64 {
		m.overflowed = true
	} else {
		m.DiscoveredFiles++
	}
	if size > math.MaxUint64-m.DiscoveredBytes {
		m.DiscoveredBytes = math.MaxUint64
		m.overflowed = true
	} else {
		m.DiscoveredBytes += size
	}
}

type selectionTracker struct {
	mu      sync.RWMutex
	measure SelectionMeasure
	failed  bool
	updates chan SelectionMeasure
	closed  bool
}

func newSelectionTracker() selectionTracker {
	updates := make(chan SelectionMeasure, 1)
	updates <- SelectionMeasure{}
	return selectionTracker{updates: updates}
}

func (t *selectionTracker) addFile(size uint64) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.measure.addDiscoveredFile(size)
	t.publishLocked()
}

func (t *selectionTracker) failDiscovery() {
	t.mu.Lock()
	t.failed = true
	t.publishLocked()
	t.mu.Unlock()
}

func (t *selectionTracker) finishDiscovery() {
	t.mu.Lock()
	t.measure.DiscoveryTerminalSuccess = !t.failed
	t.publishLocked()
	t.mu.Unlock()
}

func (t *selectionTracker) replace(measure SelectionMeasure) {
	t.mu.Lock()
	t.measure = measure
	t.publishLocked()
	t.mu.Unlock()
}

func (t *selectionTracker) closeUpdates() {
	t.mu.Lock()
	if t.updates == nil {
		t.closed = true
		t.mu.Unlock()
		return
	}
	if !t.closed {
		t.publishLocked()
		close(t.updates)
		t.closed = true
	}
	t.mu.Unlock()
}

func (t *selectionTracker) publishLocked() {
	if t.updates == nil || t.closed {
		return
	}
	select {
	case <-t.updates:
	default:
	}
	t.updates <- t.measure
}

func (t *selectionTracker) Updates() <-chan SelectionMeasure { return t.updates }

func (t *selectionTracker) snapshot() SelectionMeasure {
	t.mu.RLock()
	defer t.mu.RUnlock()
	return t.measure
}
