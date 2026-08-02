package mutationdomain

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"sort"

	"github.com/windshare/windshare/internal/perfevidence"
)

type Factory struct{}

func NewFactory() Factory {
	return Factory{}
}

func (Factory) Open(ctx context.Context, spec perfevidence.MutationDomainSpec) (
	perfevidence.MutationDomain,
	error,
) {
	if len(spec.Roots) == 0 {
		return nil, errors.New("private mutation domain requires leased input roots")
	}
	runtimeRoot, err := filepath.Abs(spec.RuntimeRoot)
	if err != nil {
		return nil, err
	}
	configuration := initialization{RuntimeRoot: runtimeRoot}
	names := make(map[string]struct{}, len(spec.Roots))
	for _, root := range spec.Roots {
		if root.Name == "" || filepath.Base(root.Name) != root.Name {
			return nil, fmt.Errorf("private mutation root name %q is invalid", root.Name)
		}
		if _, duplicate := names[root.Name]; duplicate {
			return nil, fmt.Errorf("private mutation root name %q is duplicated", root.Name)
		}
		names[root.Name] = struct{}{}
		absolute, err := filepath.Abs(root.HostPath)
		if err != nil {
			return nil, err
		}
		configuration.Roots = append(configuration.Roots, rootSpec{
			Name: root.Name, HostPath: absolute,
		})
	}
	sort.Slice(configuration.Roots, func(left, right int) bool {
		return configuration.Roots[left].Name < configuration.Roots[right].Name
	})
	return openPlatformSession(ctx, configuration)
}
