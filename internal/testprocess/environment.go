package testprocess

import (
	"errors"
	"fmt"
	"os"
	"sort"
	"strings"

	"github.com/windshare/windshare/internal/processowner/protocol"
	"github.com/windshare/windshare/internal/testrun"
	"github.com/windshare/windshare/internal/testtrace"
)

// InheritEnvironment returns an explicit canonical environment. Overrides are
// case-insensitive because the same Spec must preserve its meaning on Windows.
func InheritEnvironment(overrides map[string]string) ([]protocol.EnvironmentEntry, error) {
	return inheritEnvironment(os.Environ(), overrides)
}

type inheritedEnvironmentValue struct {
	name  string
	value string
}

func inheritEnvironment(inherited []string, overrides map[string]string) ([]protocol.EnvironmentEntry, error) {
	values := make(map[string]inheritedEnvironmentValue)
	for _, entry := range inherited {
		name, value, found := strings.Cut(entry, "=")
		// Windows drive-current-directory pseudo variables are process metadata,
		// not portable child configuration.
		if !found || name == "" {
			continue
		}
		if isOwnedEnvironmentName(name) {
			continue
		}
		key := foldEnvironmentName(name)
		if existing, exists := values[key]; exists {
			if existing.value != value {
				return nil, fmt.Errorf("inherited environment contains conflicting ASCII-case aliases %q and %q", existing.name, name)
			}
			// The spelling is diagnostic only on case-sensitive systems. Choosing it
			// by byte order keeps the canonical request independent of os.Environ order.
			if name < existing.name {
				values[key] = inheritedEnvironmentValue{name: name, value: value}
			}
			continue
		}
		values[key] = inheritedEnvironmentValue{name: name, value: value}
	}
	overrideNames := make(map[string]string, len(overrides))
	for name, value := range overrides {
		if name == "" || strings.ContainsAny(name, "=\x00") || strings.IndexByte(value, 0) >= 0 {
			return nil, errors.New("environment override is invalid")
		}
		if isOwnedEnvironmentName(name) {
			return nil, errors.New("environment override uses a process-owner reserved name")
		}
		key := foldEnvironmentName(name)
		if existing, exists := overrideNames[key]; exists {
			return nil, fmt.Errorf("environment overrides contain conflicting ASCII-case aliases %q and %q", existing, name)
		}
		overrideNames[key] = name
		values[key] = inheritedEnvironmentValue{name: name, value: value}
	}
	result := make([]protocol.EnvironmentEntry, 0, len(values))
	for _, value := range values {
		result = append(result, protocol.EnvironmentEntry{Name: value.name, Value: value.value})
	}
	sort.Slice(result, func(left, right int) bool {
		return compareEnvironmentNames(result[left].Name, result[right].Name) < 0
	})
	return result, nil
}

func isOwnedEnvironmentName(name string) bool {
	for _, reserved := range [...]string{
		testtrace.EventFDEnvironment,
		testtrace.EventHandleEnvironment,
		testrun.RunIDEnvironment,
		testrun.OperationIDEnvironment,
		testrun.ScenarioEnvironment,
	} {
		if strings.EqualFold(name, reserved) {
			return true
		}
	}
	return false
}

func compareEnvironmentNames(left, right string) int {
	leftFolded := foldEnvironmentName(left)
	rightFolded := foldEnvironmentName(right)
	if leftFolded < rightFolded {
		return -1
	}
	if leftFolded > rightFolded {
		return 1
	}
	if left < right {
		return -1
	}
	if left > right {
		return 1
	}
	return 0
}

func foldEnvironmentName(name string) string {
	folded := []byte(name)
	for index, character := range folded {
		if character >= 'A' && character <= 'Z' {
			folded[index] = character + ('a' - 'A')
		}
	}
	return string(folded)
}
