package outputnames

import (
	"sort"
	"strings"

	"github.com/windshare/windshare/core/catalog"
	"github.com/windshare/windshare/core/transfer/receivecontract"
)

// Resolver reconstructs frozen names without owning mutation capabilities.
type Resolver struct {
	operation []byte
	root      Root
	mappings  map[string]Mapping
}

func NewResolver(operation []byte, root Root, records []Mapping) (*Resolver, error) {
	if len(operation) != catalog.IdentityBytes ||
		root.Kind != receivecontract.ContainerEntrySingleFile && root.Kind != receivecontract.ContainerEntryResultRoot {
		return nil, ErrInvalidMapping
	}
	for _, name := range []string{root.RequestedName, root.ReservedName, root.PhysicalName} {
		canonical, err := catalog.CanonicalName(name)
		if err != nil || canonical != name {
			return nil, ErrInvalidMapping
		}
	}
	if len(records) > MaximumMappings {
		return nil, ErrInvalidMapping
	}
	resolver := &Resolver{operation: append([]byte(nil), operation...), root: root, mappings: make(map[string]Mapping)}
	for _, record := range records {
		if err := resolver.validate(record); err != nil {
			return nil, err
		}
		if _, exists := resolver.mappings[record.LogicalPath]; exists {
			return nil, ErrInvalidMapping
		}
		resolver.mappings[record.LogicalPath] = record
	}
	claims := make(map[string]string)
	for logical := range resolver.mappings {
		physical, err := resolver.resolve(logical)
		if err != nil {
			return nil, err
		}
		if owner, exists := claims[physical]; exists && owner != logical {
			return nil, ErrInvalidMapping
		}
		claims[physical] = logical
	}
	return resolver, nil
}

func (resolver *Resolver) Resolve(logical string) (string, error) { return resolver.resolve(logical) }

func (resolver *Resolver) Mappings() []Mapping {
	records := make([]Mapping, 0, len(resolver.mappings))
	for _, record := range resolver.mappings {
		records = append(records, record)
	}
	sort.Slice(records, func(i, j int) bool { return records[i].LogicalPath < records[j].LogicalPath })
	return records
}

func (resolver *Resolver) validate(record Mapping) error {
	parts, err := resolver.parts(record.LogicalPath)
	if err != nil || len(parts) < 2 || resolver.root.Kind != receivecontract.ContainerEntryResultRoot {
		return ErrInvalidMapping
	}
	expected, err := Candidate(resolver.operation, record.LogicalPath, record.Attempt)
	if err != nil || record.PhysicalComponent != expected {
		return ErrInvalidMapping
	}
	return nil
}

func (resolver *Resolver) parts(logical string) ([]string, error) {
	canonical, err := catalog.CanonicalPath(logical)
	if err != nil || canonical != logical {
		return nil, ErrInvalidMapping
	}
	parts := strings.Split(logical, "/")
	if parts[0] != resolver.root.RequestedName {
		return nil, ErrInvalidMapping
	}
	return parts, nil
}

func (resolver *Resolver) resolve(logical string) (string, error) {
	parts, err := resolver.parts(logical)
	if err != nil {
		return "", err
	}
	if resolver.root.Kind == receivecontract.ContainerEntrySingleFile {
		if len(parts) != 1 {
			return "", ErrInvalidMapping
		}
		return resolver.root.PhysicalName, nil
	}
	physical := make([]string, 0, len(parts)-1)
	for index := 1; index < len(parts); index++ {
		component := parts[index]
		if record, exists := resolver.mappings[strings.Join(parts[:index+1], "/")]; exists {
			component = record.PhysicalComponent
		}
		physical = append(physical, component)
	}
	return strings.Join(physical, "/"), nil
}
