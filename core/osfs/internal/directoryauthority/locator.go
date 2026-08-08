package directoryauthority

import (
	"errors"
	"strings"

	"github.com/windshare/windshare/core/catalog"
	"github.com/windshare/windshare/core/osfs/internal/outputfault"
)

// canonicalLocator performs the platform and reserved-namespace work needed
// before the output-session ledger atomically reserves its locator claim.
func (authority *Authority) canonicalLocator(path string) (locatorKey, error) {
	if authority == nil {
		return locatorKey{}, ErrInvalidLocator
	}
	authority.gate.RLock()
	defer authority.gate.RUnlock()
	authority.mu.Lock()
	closed := authority.closed
	authority.mu.Unlock()
	if closed {
		return locatorKey{}, ErrAuthorityClosed
	}
	if path == "" {
		return locatorKey{authority: authority, canonicalKey: rootLocatorKey}, nil
	}
	canonical, err := catalog.CanonicalPath(path)
	if err != nil || canonical != path {
		return locatorKey{}, errors.Join(ErrInvalidLocator, outputfault.ErrPathEscape, err)
	}
	platformKey, err := authority.platform.CanonicalLocatorKey(path)
	if err != nil || platformKey == "" {
		return locatorKey{}, errors.Join(ErrInvalidLocator, err)
	}
	first, _, _ := strings.Cut(path, "/")
	firstKey, err := authority.platform.CanonicalComponentKey(first)
	if err != nil || firstKey == "" {
		return locatorKey{}, errors.Join(ErrInvalidLocator, err)
	}
	// Prefix reservation also protects probe/bootstrap names below the control
	// namespace and compares both operands through the platform's exact key rule.
	if strings.HasPrefix(firstKey, authority.reservedKey) {
		return locatorKey{}, errors.Join(ErrInvalidLocator, outputfault.ErrReservedPath)
	}
	separator := strings.LastIndexByte(path, '/')
	leaf := path[separator+1:]
	leafKey, err := authority.platform.CanonicalComponentKey(leaf)
	if err != nil || leafKey == "" {
		return locatorKey{}, errors.Join(ErrInvalidLocator, err)
	}
	return locatorKey{
		authority: authority, canonicalPath: path, canonicalKey: platformKey, leaf: leaf, leafKey: leafKey,
	}, nil
}

// CanonicalLocatorKey exposes only the comparable key reserved by outputsession;
// richer path-local details never leave this native-authority module.
func (authority *Authority) CanonicalLocatorKey(path string) (string, error) {
	locator, err := authority.canonicalLocator(path)
	if err != nil {
		return "", err
	}
	return locator.canonicalKey, nil
}

func validateImmediateChild(parentPath, childPath string) bool {
	separator := strings.LastIndexByte(childPath, '/')
	if separator < 0 {
		return parentPath == "" && childPath != ""
	}
	return childPath[:separator] == parentPath && separator+1 < len(childPath)
}
