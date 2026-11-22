package output

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"sync"
	"unicode"
	"unicode/utf8"

	"github.com/windshare/windshare/core/catalog"
	"github.com/windshare/windshare/core/transfer/receivecontract"
)

const (
	MaxPortableNameBytes  = 255
	MaxNameRepairAttempts = 16
	MaxNameMappings       = 65_536
	nameTokenBytes        = 16
	nameSuffix            = ".windshare-"
	defaultNamePrefix     = "entry"
	maxExtensionBytes     = 32
)

var ErrInvalidNameMapping = errors.New("output name mapping is invalid")

// NameMapping persists one logical-to-physical component repair so restarts
// reuse the exact same destination spelling.
type NameMapping struct {
	LogicalPath       string `json:"logical"`
	PhysicalComponent string `json:"physical"`
	Attempt           uint32 `json:"attempt"`
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
	logical  string
	physical string
	attempt  uint32
	reused   bool
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

// bindLeaf resolves the physical leaf name for logicalPath inside parentDir,
// repairing only when the destination rejects the component, it collides with
// .windshare-tmp* at the container root, or it aliases another mapped name.
func (m *nameMapper) bindLeaf(logicalPath string, parentDir *Dir) (string, error) {
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
	return m.repairLeafLocked(logicalPath, parentRel, parentDir)
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

func (m *nameMapper) repairLeafLocked(logicalPath, parentRel string, parentDir *Dir) (string, error) {
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
