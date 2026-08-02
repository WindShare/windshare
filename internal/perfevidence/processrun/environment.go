package processrun

import (
	"bytes"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/windshare/windshare/internal/processowner/protocol"
)

func CanonicalEnvironment(base, overrides []string) ([]protocol.EnvironmentEntry, error) {
	entries := make(map[string]protocol.EnvironmentEntry, len(base)+len(overrides))
	add := func(raw string, inherited bool) error {
		index := strings.IndexByte(raw, '=')
		if index < 1 {
			// Windows drive-current-directory pseudo variables are implementation
			// state, not target configuration, and cannot cross the owner protocol.
			if inherited && strings.HasPrefix(raw, "=") {
				return nil
			}
			return fmt.Errorf("owned command environment entry %q is not NAME=VALUE", raw)
		}
		name := raw[:index]
		if protocol.IsReservedEnvironmentName(name) || strings.EqualFold(name, helperRoleEnvironment) {
			if inherited {
				return nil
			}
			return fmt.Errorf("owned command environment name %q is reserved", name)
		}
		key := string(asciiFold([]byte(name)))
		entries[key] = protocol.EnvironmentEntry{Name: name, Value: raw[index+1:]}
		return nil
	}
	for _, raw := range base {
		if err := add(raw, true); err != nil {
			return nil, err
		}
	}
	for _, raw := range overrides {
		if err := add(raw, false); err != nil {
			return nil, err
		}
	}
	result := make([]protocol.EnvironmentEntry, 0, len(entries))
	for _, entry := range entries {
		result = append(result, entry)
	}
	sort.Slice(result, func(left, right int) bool {
		return bytes.Compare(
			asciiFold([]byte(result[left].Name)),
			asciiFold([]byte(result[right].Name)),
		) < 0
	})
	if result == nil {
		result = make([]protocol.EnvironmentEntry, 0)
	}
	for index := 1; index < len(result); index++ {
		if bytes.Equal(
			asciiFold([]byte(result[index-1].Name)),
			asciiFold([]byte(result[index].Name)),
		) {
			return nil, errors.New("owned command environment aliases remain after canonicalization")
		}
	}
	return result, nil
}

func asciiFold(value []byte) []byte {
	result := bytes.Clone(value)
	for index, character := range result {
		if character >= 'A' && character <= 'Z' {
			result[index] = character + ('a' - 'A')
		}
	}
	return result
}
