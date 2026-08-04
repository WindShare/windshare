package testprocess

import (
	"errors"
	"os"
	"sort"
	"strings"

	"github.com/windshare/windshare/internal/testtrace"
)

// InheritEnvironment returns one deterministic entry per variable while
// removing transport handles that belong to a different child process.
func InheritEnvironment(overrides map[string]string) ([]string, error) {
	values := make(map[string]string)
	names := make(map[string]string)
	for _, entry := range os.Environ() {
		name, value, found := strings.Cut(entry, "=")
		if !found || name == "" || reservedTransportName(name) {
			continue
		}
		key := strings.ToUpper(name)
		values[key], names[key] = value, name
	}
	for name, value := range overrides {
		if name == "" || strings.ContainsAny(name, "=\x00") || strings.IndexByte(value, 0) >= 0 {
			return nil, errors.New("test process environment override is invalid")
		}
		key := strings.ToUpper(name)
		values[key], names[key] = value, name
	}
	environment := make([]string, 0, len(values))
	for key, value := range values {
		environment = append(environment, names[key]+"="+value)
	}
	sort.Slice(environment, func(left, right int) bool {
		return strings.ToUpper(environment[left]) < strings.ToUpper(environment[right])
	})
	return environment, nil
}

func reservedTransportName(name string) bool {
	return strings.EqualFold(name, testtrace.EventFDEnvironment) ||
		strings.EqualFold(name, testtrace.EventHandleEnvironment)
}
