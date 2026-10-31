package outputnames

import (
	"errors"
	"strings"
	"sync"

	"github.com/windshare/windshare/core/transfer/receivecontract"
	"github.com/windshare/windshare/osfs/internal/outputcap"
)

const MaximumMappings = 65_536

type Mapping struct {
	LogicalPath       string `json:"logicalPath"`
	PhysicalComponent string `json:"physicalComponent"`
	Attempt           uint32 `json:"attempt"`
}

type Store interface {
	Load() ([]Mapping, error)
	Put(Mapping) error
}

type Namespace interface {
	InspectName(parentPath, component string) (outputcap.EntryKind, error)
}

type Root struct {
	ReservedName  string
	RequestedName string
	PhysicalName  string
	Kind          receivecontract.ContainerEntryKind
}

type Trace struct {
	LogicalPath  string
	PhysicalPath string
	Attempt      uint32
	Reused       bool
}

type Mapper struct {
	resolver  *Resolver
	mu        sync.Mutex
	store     Store
	namespace Namespace

	claims map[string]string
	trace  func(Trace)
}

func New(operation []byte, root Root, store Store, namespace Namespace, trace func(Trace)) (*Mapper, error) {
	if store == nil || namespace == nil {
		return nil, ErrInvalidMapping
	}
	records, err := store.Load()
	if err != nil {
		return nil, err
	}
	resolver, err := NewResolver(operation, root, records)
	if err != nil {
		return nil, err
	}
	mapper := &Mapper{resolver: resolver, store: store, namespace: namespace, claims: make(map[string]string), trace: trace}
	for logical := range resolver.mappings {
		physical, _ := resolver.resolve(logical)
		mapper.claims[physical] = logical
	}
	return mapper, nil
}

// Resolve is read-only: recovery never reinterprets a frozen destination after
// moving to a filesystem that happens to accept more source spellings.
func (mapper *Mapper) Resolve(logical string) (string, error) {
	mapper.mu.Lock()
	defer mapper.mu.Unlock()
	return mapper.resolver.resolve(logical)
}

func (mapper *Mapper) Bind(logical string) (string, error) {
	mapper.mu.Lock()
	defer mapper.mu.Unlock()
	physical, err := mapper.resolver.resolve(logical)
	if err != nil {
		return "", err
	}
	if mapper.resolver.root.Kind == receivecontract.ContainerEntrySingleFile || physical == "" {
		return physical, nil
	}
	if record, mapped := mapper.resolver.mappings[logical]; mapped {
		mapper.emit(Trace{LogicalPath: logical, PhysicalPath: physical, Attempt: record.Attempt, Reused: true})
		return physical, nil
	}
	parent, leaf := splitParent(physical)
	owner, claimed := mapper.claims[physical]
	if !claimed || owner == logical {
		if PortableName(leaf) {
			mapper.claims[physical] = logical
			return physical, nil
		}
		_, inspectErr := mapper.namespace.InspectName(parent, leaf)
		if inspectErr == nil {
			mapper.claims[physical] = logical
			return physical, nil
		}
		if !errors.Is(inspectErr, outputcap.ErrNameRejected) {
			return "", inspectErr
		}
	}
	if len(mapper.resolver.mappings) >= MaximumMappings {
		return "", ErrInvalidMapping
	}
	for attempt := uint32(0); attempt < MaximumAttempts; attempt++ {
		component, err := Candidate(mapper.resolver.operation, logical, attempt)
		if err != nil {
			return "", err
		}
		candidate := joinParent(parent, component)
		if owner, exists := mapper.claims[candidate]; exists && owner != logical {
			continue
		}
		kind, err := mapper.namespace.InspectName(parent, component)
		if err != nil {
			return "", err
		}
		if kind != outputcap.EntryAbsent {
			continue
		}
		record := Mapping{LogicalPath: logical, PhysicalComponent: component, Attempt: attempt}
		// A durable choice precedes public creation, so no crash can orphan content
		// under an unrecorded alternative name.
		if err := mapper.store.Put(record); err != nil {
			return "", err
		}
		mapper.resolver.mappings[logical] = record
		mapper.claims[candidate] = logical
		mapper.emit(Trace{LogicalPath: logical, PhysicalPath: candidate, Attempt: attempt})
		return candidate, nil
	}
	return "", errors.New("compatible output name attempts are exhausted")
}

func (mapper *Mapper) Mappings() []Mapping {
	mapper.mu.Lock()
	defer mapper.mu.Unlock()
	return mapper.resolver.Mappings()
}

func (mapper *Mapper) emit(event Trace) {
	if mapper.trace != nil {
		mapper.trace(event)
	}
}

func splitParent(path string) (string, string) {
	if slash := strings.LastIndexByte(path, '/'); slash >= 0 {
		return path[:slash], path[slash+1:]
	}
	return "", path
}

func joinParent(parent, leaf string) string {
	if parent == "" {
		return leaf
	}
	return parent + "/" + leaf
}
