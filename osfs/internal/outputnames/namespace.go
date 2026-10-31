package outputnames

import (
	"errors"
	"slices"
	"strings"

	"github.com/windshare/windshare/osfs/internal/outputcap"
)

// GuardedNamespace probes names through retained directory capabilities. The
// executor still performs its own identity check when it creates the final.
type GuardedNamespace struct {
	Acquire func() (outputcap.PublicOperationGuard, error)
}

func (namespace GuardedNamespace) InspectName(parentPath, component string) (kind outputcap.EntryKind, resultErr error) {
	if namespace.Acquire == nil {
		return 0, ErrInvalidMapping
	}
	guard, err := namespace.Acquire()
	if err != nil {
		return 0, err
	}
	if guard == nil {
		return 0, outputcap.ErrUnsafeNamespace
	}
	defer func() { resultErr = errors.Join(resultErr, guard.Close()) }()
	current := guard.Root()
	if current == nil {
		return 0, outputcap.ErrUnsafeNamespace
	}
	var opened []outputcap.Directory
	defer func() {
		for _, directory := range slices.Backward(opened) {
			resultErr = errors.Join(resultErr, directory.Close())
		}
	}()
	var parents []string
	if parentPath != "" {
		parents = strings.Split(parentPath, "/")
	}
	for _, parent := range parents {
		child, err := openExactParent(current, parent)
		if err != nil {
			return 0, err
		}
		opened = append(opened, child)
		current = child
	}
	kind, _, err = current.ClassifyExactEntry(component)
	return kind, err
}

func openExactParent(current outputcap.Directory, name string) (outputcap.Directory, error) {
	kind, exact, err := current.ClassifyExactEntry(name)
	if err != nil {
		return nil, err
	}
	if kind != outputcap.EntryDirectory || !exact {
		return nil, outputcap.ErrUnsafeNamespace
	}
	reference, err := current.OpenEntry(name)
	if err != nil || reference == nil {
		return nil, errors.Join(outputcap.ErrUnsafeNamespace, err)
	}
	child, childErr := current.OpenPinnedDirectory(reference, false)
	unchanged, matchErr := current.EntryMatches(name, reference)
	closeErr := reference.Close()
	if childErr == nil && matchErr == nil && closeErr == nil && unchanged && child != nil {
		return child, nil
	}
	if child != nil {
		closeErr = errors.Join(closeErr, child.Close())
	}
	return nil, errors.Join(outputcap.ErrUnsafeNamespace, childErr, matchErr, closeErr)
}
