package output

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"slices"
	"strings"
	"sync"
	"unicode"
	"unicode/utf8"

	"github.com/windshare/windshare/core/catalog"
	"github.com/windshare/windshare/core/transfer"
	"github.com/windshare/windshare/core/transfer/receivecontract"
)

const (
	MaxPortableNameBytes     = 255
	MaxNameRepairAttempts    = 16
	MaxNameMappings          = 65_536
	MaxNameAdjustmentSamples = 20
	nameTokenBytes           = 16
	nameSuffix               = ".windshare-"
	defaultNamePrefix        = "entry"
	maxExtensionBytes        = 32
)

var ErrInvalidNameMapping = errors.New("output name mapping is invalid")

// NameAdjustment records one logical name or path saved under a compatible
// physical spelling: relative to the destination directory for nested items, or
// the destination entry name itself when the top-level root or single file was
// repaired.
type NameAdjustment struct {
	LogicalPath  string
	PhysicalPath string
}

// NameAdjustmentSummary is a bounded value snapshot of compatible-name repairs
// materialized by an operation. Count is exact; Samples is capped.
type NameAdjustmentSummary struct {
	Count   uint64
	Samples []NameAdjustment
}

func (s NameAdjustmentSummary) Clone() NameAdjustmentSummary {
	return NameAdjustmentSummary{
		Count:   s.Count,
		Samples: slices.Clone(s.Samples),
	}
}

// NameMapping persists one logical-to-physical component repair so restarts
// reuse the exact same destination spelling.
type NameMapping struct {
	LogicalPath       string `json:"logical"`
	PhysicalComponent string `json:"physical"`
	Attempt           uint32 `json:"attempt"`
	Directory         bool   `json:"dir,omitempty"`
}

// isReservedContainerRootName reports whether name collides with WindShare's
// private control directory namespace at the destination container root.
func isReservedContainerRootName(name string) bool {
	lower := strings.ToLower(name)
	return lower == ControlDirName || strings.HasPrefix(lower, ControlDirName+".")
}

// PortableName reports whether name is safe across Windows, macOS, and Linux
// filesystems. Original source names are attempted natively first; PortableName
// governs generated repair names and fast-path checks.
func PortableName(name string) bool {
	canonical, err := catalog.CanonicalName(name)
	if err != nil || canonical != name || len(name) > MaxPortableNameBytes ||
		strings.HasSuffix(name, ".") || strings.HasSuffix(name, " ") ||
		strings.ContainsAny(name, `<>:"|?*`) {
		return false
	}
	stem, _, _ := strings.Cut(name, ".")
	stem = strings.ToUpper(strings.TrimRight(stem, " "))
	switch stem {
	case "CON", "PRN", "AUX", "NUL", "CONIN$", "CONOUT$":
		return false
	}
	for _, prefix := range []string{"COM", "LPT"} {
		if suffix, ok := strings.CutPrefix(stem, prefix); ok &&
			(len(suffix) == 1 && suffix[0] >= '1' && suffix[0] <= '9' ||
				suffix == "¹" || suffix == "²" || suffix == "³") {
			return false
		}
	}
	return true
}

// SanitizeCandidate builds a deterministic cross-platform filename while
// keeping a readable stem and safe extension.
func SanitizeCandidate(operationID []byte, logicalPath string, attempt uint32) (string, error) {
	canonical, err := catalog.CanonicalPath(logicalPath)
	if len(operationID) != catalog.IdentityBytes || err != nil || canonical != logicalPath || attempt >= MaxNameRepairAttempts {
		return "", ErrInvalidNameMapping
	}
	_, name := splitLogicalPath(logicalPath)
	stem, extension := name, ""
	if dot := strings.LastIndexByte(name, '.'); dot > 0 {
		candidate := name[dot:]
		if len(candidate) <= maxExtensionBytes && PortableName("file"+candidate) {
			stem, extension = name[:dot], candidate
		}
	}
	var readable strings.Builder
	separator := false
	for _, scalar := range stem {
		if unicode.IsLetter(scalar) || unicode.IsNumber(scalar) || unicode.IsMark(scalar) {
			if separator && readable.Len() > 0 {
				readable.WriteByte('-')
			}
			readable.WriteRune(scalar)
			separator = false
		} else {
			separator = true
		}
	}
	prefix := readable.String()
	if prefix == "" {
		prefix = defaultNamePrefix
	}
	sum := sha256.New()
	_, _ = sum.Write([]byte("windshare/native-compatible-name/v1\x00"))
	_, _ = sum.Write(operationID)
	_, _ = fmt.Fprintf(sum, "\x00%s\x00%d", logicalPath, attempt)
	token := hex.EncodeToString(sum.Sum(nil)[:nameTokenBytes])
	budget := MaxPortableNameBytes - len(nameSuffix) - len(token) - len(extension)
	prefix = truncateUTF8(prefix, budget)
	name = prefix + nameSuffix + token + extension
	if !PortableName(name) {
		prefix = truncateUTF8(defaultNamePrefix+"-"+prefix, budget)
		name = prefix + nameSuffix + token + extension
	}
	if !PortableName(name) {
		return "", ErrInvalidNameMapping
	}
	return name, nil
}

func truncateUTF8(value string, maximum int) string {
	if len(value) <= maximum {
		return value
	}
	value = value[:maximum]
	for !utf8.ValidString(value) {
		value = value[:len(value)-1]
	}
	return value
}

func splitLogicalPath(path string) (string, string) {
	if parent, leaf, ok := strings.CutLast(path, "/"); ok {
		return parent, leaf
	}
	return "", path
}

func joinLogicalPath(parent, leaf string) string {
	if parent == "" {
		return leaf
	}
	return parent + "/" + leaf
}

type nameTraceEvent struct {
	logical   string
	physical  string
	attempt   uint32
	reused    bool
	directory bool
}

// nameMapper maps logical materialization paths to physical paths relative to
// the operation's result root, persisting any repaired component in the journal.
type nameMapper struct {
	mu            sync.Mutex
	opID          []byte
	entryKind     receivecontract.ContainerEntryKind // 0 for ReservationContainerRoot
	requestedRoot string
	physicalRoot  string
	mappings      map[string]NameMapping
	claims        map[string]string
	persist       func(NameMapping) error
	trace         func(nameTraceEvent)
}

func newNameMapper(
	opID receivecontract.OperationID,
	reservation receivecontract.DestinationReservation,
	existing []NameMapping,
	persist func(NameMapping) error,
	trace func(nameTraceEvent),
) (*nameMapper, error) {
	mapper := &nameMapper{
		opID:          opID.Bytes(),
		entryKind:     reservation.EntryKind(),
		requestedRoot: reservation.RequestedName(),
		physicalRoot:  reservation.PhysicalName(),
		mappings:      make(map[string]NameMapping, len(existing)),
		claims:        make(map[string]string, len(existing)),
		persist:       persist,
		trace:         trace,
	}
	for _, m := range existing {
		mapper.mappings[m.LogicalPath] = m
	}
	for logical := range mapper.mappings {
		phys, err := mapper.resolveLocked(logical)
		if err != nil {
			return nil, err
		}
		mapper.claims[phys] = logical
	}
	return mapper, nil
}

// resolve returns the physical path relative to the operation's root directory.
func (m *nameMapper) resolve(logical string) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.resolveLocked(logical)
}

// resolveLocked turns a canonical logical materialization path into a physical
// path relative to the operation's root directory (resultDir for ResultRoot,
// container root for CatalogRoot and SingleFile).
func (m *nameMapper) resolveLocked(logical string) (string, error) {
	if logical == "" {
		if m.entryKind == 0 {
			return "", nil
		}
		return "", ErrInvalidNameMapping
	}
	canonical, err := catalog.CanonicalPath(logical)
	if err != nil || canonical != logical {
		return "", ErrInvalidNameMapping
	}
	parts := strings.Split(logical, "/")
	switch m.entryKind {
	case receivecontract.ContainerEntrySingleFile:
		if len(parts) != 1 || parts[0] != m.requestedRoot {
			return "", ErrInvalidNameMapping
		}
		return m.physicalRoot, nil
	case receivecontract.ContainerEntryResultRoot:
		if parts[0] != m.requestedRoot {
			return "", ErrInvalidNameMapping
		}
		if len(parts) == 1 {
			return "", nil
		}
		phys := make([]string, 0, len(parts)-1)
		for i := 1; i < len(parts); i++ {
			comp := parts[i]
			if rec, ok := m.mappings[strings.Join(parts[:i+1], "/")]; ok {
				comp = rec.PhysicalComponent
			}
			phys = append(phys, comp)
		}
		return strings.Join(phys, "/"), nil
	default:
		// Container root (CatalogRoot): all segments are directly under dest root.
		phys := make([]string, 0, len(parts))
		for i := range parts {
			comp := parts[i]
			if rec, ok := m.mappings[strings.Join(parts[:i+1], "/")]; ok {
				comp = rec.PhysicalComponent
			}
			phys = append(phys, comp)
		}
		return strings.Join(phys, "/"), nil
	}
}

// bindFileLeaf resolves the physical leaf name for a file inside parentDir,
// repairing only when the destination rejects the component, it collides with
// .windshare-tmp* at the container root, or it aliases another mapped name.
// Repaired file names are persisted ahead of staging/publication so crash
// recovery can reconcile interrupted publishes against the same physical leaf;
// NameAdjustments gates file reporting on durable delivery receipts.
func (m *nameMapper) bindFileLeaf(logicalPath string, parentDir *Dir) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	phys, err := m.resolveLocked(logicalPath)
	if err != nil {
		return "", err
	}
	if m.entryKind == receivecontract.ContainerEntrySingleFile || phys == "" {
		return phys, nil
	}
	parentRel, leaf := splitLogicalPath(phys)
	if rec, mapped := m.mappings[logicalPath]; mapped {
		if m.trace != nil {
			m.trace(nameTraceEvent{logical: logicalPath, physical: phys, attempt: rec.Attempt, reused: true})
		}
		return leaf, nil
	}
	owner, claimed := m.claims[phys]
	reserved := m.entryKind == 0 && parentRel == "" && isReservedContainerRootName(leaf)
	if !reserved && (!claimed || owner == logicalPath) {
		usable, checkErr := leafUsableDirectly(parentDir, leaf)
		if checkErr != nil {
			return "", checkErr
		}
		if usable {
			m.claims[phys] = logicalPath
			return leaf, nil
		}
	}
	return m.repairFileLeafLocked(logicalPath, parentRel, parentDir)
}

// bindDirectory resolves and materializes the physical subdirectory for
// logicalPath inside parentDir. Unlike files, directories have no separate
// delivery receipt, so repaired directory names are persisted only after the
// new directory is created on disk (rolling back the empty directory if
// persistence fails).
func (m *nameMapper) bindDirectory(
	logicalPath string,
	parentDir *Dir,
	createFresh func(parent *Dir, name string) (*Dir, error),
	openExisting func(parent *Dir, name string) (*Dir, error),
) (*Dir, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	phys, err := m.resolveLocked(logicalPath)
	if err != nil {
		return nil, err
	}
	if m.entryKind == receivecontract.ContainerEntrySingleFile || phys == "" {
		return nil, ErrInvalidNameMapping
	}
	parentRel, leaf := splitLogicalPath(phys)
	if rec, mapped := m.mappings[logicalPath]; mapped {
		childDir, err := openExisting(parentDir, leaf)
		if err != nil {
			return nil, err
		}
		if m.trace != nil {
			m.trace(nameTraceEvent{logical: logicalPath, physical: phys, attempt: rec.Attempt, reused: true, directory: true})
		}
		return childDir, nil
	}
	owner, claimed := m.claims[phys]
	reserved := m.entryKind == 0 && parentRel == "" && isReservedContainerRootName(leaf)
	if !reserved && (!claimed || owner == logicalPath) {
		usable, checkErr := leafUsableDirectly(parentDir, leaf)
		if checkErr != nil {
			return nil, checkErr
		}
		if usable {
			childDir, err := openExisting(parentDir, leaf)
			if err != nil {
				return nil, err
			}
			m.claims[phys] = logicalPath
			return childDir, nil
		}
	}
	return m.repairDirectoryLocked(logicalPath, parentRel, parentDir, createFresh)
}

func leafUsableDirectly(parentDir *Dir, leaf string) (bool, error) {
	if PortableName(leaf) {
		return true, nil
	}
	if _, _, err := lstatChild(parentDir, leaf); err == nil {
		return true, nil
	} else if !errors.Is(err, ErrNameRejected) {
		return false, err
	}
	return false, nil
}

func (m *nameMapper) repairFileLeafLocked(logicalPath, parentRel string, parentDir *Dir) (string, error) {
	if len(m.mappings) >= MaxNameMappings {
		return "", ErrInvalidNameMapping
	}
	for attempt := range uint32(MaxNameRepairAttempts) {
		comp, err := SanitizeCandidate(m.opID, logicalPath, attempt)
		if err != nil {
			return "", err
		}
		candRel := joinLogicalPath(parentRel, comp)
		if owner, exists := m.claims[candRel]; exists && owner != logicalPath {
			continue
		}
		kind, _, statErr := lstatChild(parentDir, comp)
		if statErr != nil {
			return "", statErr
		}
		if kind != EntryAbsent {
			continue
		}
		rec := NameMapping{
			LogicalPath:       logicalPath,
			PhysicalComponent: comp,
			Attempt:           attempt,
		}
		if m.persist != nil {
			if err := m.persist(rec); err != nil {
				return "", err
			}
		}
		m.mappings[logicalPath] = rec
		m.claims[candRel] = logicalPath
		if m.trace != nil {
			m.trace(nameTraceEvent{logical: logicalPath, physical: candRel, attempt: attempt})
		}
		return comp, nil
	}
	return "", errors.New("compatible output name attempts are exhausted")
}

func (m *nameMapper) repairDirectoryLocked(
	logicalPath, parentRel string,
	parentDir *Dir,
	createFresh func(parent *Dir, name string) (*Dir, error),
) (*Dir, error) {
	if len(m.mappings) >= MaxNameMappings {
		return nil, ErrInvalidNameMapping
	}
	for attempt := range uint32(MaxNameRepairAttempts) {
		comp, err := SanitizeCandidate(m.opID, logicalPath, attempt)
		if err != nil {
			return nil, err
		}
		candRel := joinLogicalPath(parentRel, comp)
		if owner, exists := m.claims[candRel]; exists && owner != logicalPath {
			continue
		}
		kind, _, statErr := lstatChild(parentDir, comp)
		if statErr != nil {
			return nil, statErr
		}
		if kind != EntryAbsent {
			continue
		}
		childDir, createErr := createFresh(parentDir, comp)
		if errors.Is(createErr, ErrNamespaceCollision) || errors.Is(createErr, fs.ErrExist) {
			continue
		}
		if createErr != nil {
			return nil, createErr
		}
		rec := NameMapping{
			LogicalPath:       logicalPath,
			PhysicalComponent: comp,
			Attempt:           attempt,
			Directory:         true,
		}
		if m.persist != nil {
			if err := m.persist(rec); err != nil {
				if childDir != nil {
					_ = childDir.Close()
				}
				_ = removeChild(parentDir, comp, true)
				return nil, err
			}
		}
		m.mappings[logicalPath] = rec
		m.claims[candRel] = logicalPath
		if m.trace != nil {
			m.trace(nameTraceEvent{logical: logicalPath, physical: candRel, attempt: attempt, directory: true})
		}
		return childDir, nil
	}
	return nil, errors.New("compatible output name attempts are exhausted")
}

// sanitizeTopLevelComponent prepares a physical name for a top-level container
// entry when the requested/collision-suffixed name is rejected or reserved.
func sanitizeTopLevelComponent(opID receivecontract.OperationID, logicalName string, container *Dir) (string, bool, error) {
	if !isReservedContainerRootName(logicalName) {
		if PortableName(logicalName) {
			return logicalName, false, nil
		}
		if _, _, err := lstatChild(container, logicalName); err == nil {
			return logicalName, false, nil
		} else if !errors.Is(err, ErrNameRejected) {
			return "", false, err
		}
	}
	for attempt := range uint32(MaxNameRepairAttempts) {
		comp, err := SanitizeCandidate(opID.Bytes(), logicalName, attempt)
		if err != nil {
			return "", false, err
		}
		kind, _, err := lstatChild(container, comp)
		if err != nil {
			return "", false, err
		}
		if kind == EntryAbsent {
			return comp, true, nil
		}
	}
	return "", false, errors.New("compatible top-level name attempts are exhausted")
}

func (op *Operation) NameAdjustments() NameAdjustmentSummary {
	if op == nil {
		return NameAdjustmentSummary{}
	}
	op.summaryMu.Lock()
	defer op.summaryMu.Unlock()
	return op.nameAdjustments.Clone()
}

func (op *Operation) refreshNameAdjustments() {
	if op == nil || op.journal == nil {
		return
	}
	mappings, delivered, hasDelivered := op.journal.nameAdjustmentSnapshot()
	summary := summarizeNameAdjustments(op.intent, mappings, delivered, hasDelivered)
	op.summaryMu.Lock()
	op.nameAdjustments = summary
	op.summaryMu.Unlock()
}

// summarizeNameAdjustments derives a deterministic, bounded value snapshot of
// compatible-name repairs actually materialized on disk so ordinary transfers
// pay zero allocation cost and settled results survive control-dir retirement.
func summarizeNameAdjustments(
	intent transfer.ReceiveIntent,
	mappings []NameMapping,
	delivered map[string]struct{},
	hasDelivered bool,
) NameAdjustmentSummary {
	if intent.IsZero() {
		return NameAdjustmentSummary{}
	}
	reservation, ok := intent.MaterializationPlan().DestinationReservation()
	if !ok || reservation.IsZero() {
		return NameAdjustmentSummary{}
	}
	topAdjustment, hasTop := topLevelNameAdjustment(reservation, hasDelivered)
	if !hasTop && len(mappings) == 0 {
		return NameAdjustmentSummary{}
	}
	nested := collectNestedNameAdjustments(intent.OperationID(), reservation, mappings, delivered)
	total := len(nested)
	if hasTop {
		total++
	}
	if total == 0 {
		return NameAdjustmentSummary{}
	}
	sampleCount := min(total, MaxNameAdjustmentSamples)
	samples := make([]NameAdjustment, 0, sampleCount)
	if hasTop {
		samples = append(samples, topAdjustment)
	}
	if remaining := sampleCount - len(samples); remaining > 0 {
		samples = append(samples, nested[:remaining]...)
	}
	return NameAdjustmentSummary{
		Count:   uint64(total),
		Samples: samples,
	}
}

func topLevelNameAdjustment(
	reservation receivecontract.DestinationReservation,
	hasDelivered bool,
) (NameAdjustment, bool) {
	if reservation.Kind() != receivecontract.ReservationNamedContainerEntry ||
		reservation.PhysicalName() == reservation.LogicalReservedName() {
		return NameAdjustment{}, false
	}
	switch reservation.EntryKind() {
	case receivecontract.ContainerEntryResultRoot:
	case receivecontract.ContainerEntrySingleFile:
		if !hasDelivered {
			return NameAdjustment{}, false
		}
	default:
		return NameAdjustment{}, false
	}
	return NameAdjustment{
		LogicalPath:  reservation.LogicalReservedName(),
		PhysicalPath: reservation.PhysicalName(),
	}, true
}

func collectNestedNameAdjustments(
	opID receivecontract.OperationID,
	reservation receivecontract.DestinationReservation,
	mappings []NameMapping,
	delivered map[string]struct{},
) []NameAdjustment {
	if len(mappings) == 0 {
		return nil
	}
	mapper, err := newNameMapper(opID, reservation, mappings, nil, nil)
	if err != nil {
		return nil
	}
	prefix := ""
	if reservation.EntryKind() == receivecontract.ContainerEntryResultRoot {
		prefix = reservation.RequestedName() + "/"
	}
	nested := make([]NameAdjustment, 0, len(mappings))
	for _, mapping := range mappings {
		// Directory mappings are persisted only after mkdir succeeds in
		// bindDirectory; file mappings are persisted before commit and require
		// a durable delivery receipt before being reported as saved.
		if !mapping.Directory {
			if _, ok := delivered[mapping.LogicalPath]; !ok {
				continue
			}
		}
		phys, resolveErr := mapper.resolveLocked(mapping.LogicalPath)
		if resolveErr != nil || phys == "" {
			continue
		}
		logical := strings.TrimPrefix(mapping.LogicalPath, prefix)
		if logical == "" || logical == phys {
			continue
		}
		nested = append(nested, NameAdjustment{
			LogicalPath:  logical,
			PhysicalPath: phys,
		})
	}
	slices.SortFunc(nested, func(a, b NameAdjustment) int {
		if a.LogicalPath != b.LogicalPath {
			return strings.Compare(a.LogicalPath, b.LogicalPath)
		}
		return strings.Compare(a.PhysicalPath, b.PhysicalPath)
	})
	return nested
}
