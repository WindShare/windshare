// Package outputportable supplies best-effort restart storage using Go files.
// Native adapters are limited to exclusion and no-replace namespace operations.
package outputportable

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"

	"github.com/windshare/windshare/core/catalog"
	"github.com/windshare/windshare/core/transfer"
	"github.com/windshare/windshare/osfs/internal/outputcap"
)

type Platform struct {
	root         *directory
	disposition  outputcap.RootOpenDisposition
	mu           sync.Mutex
	capabilities outputcap.DestinationCapabilities
}

func Open(path string, create bool) (outputcap.Platform, error) {
	absolute, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	disposition := outputcap.CallerProvidedContainer
	if create {
		if _, err := os.Lstat(absolute); errors.Is(err, fs.ErrNotExist) {
			if err := os.MkdirAll(absolute, 0o755); err != nil {
				return nil, err
			}
			disposition = outputcap.AuthorityCreatedRoot
		}
	}
	root, err := openDirectory(absolute)
	if err != nil {
		return nil, err
	}
	return &Platform{root: root, disposition: disposition}, nil
}

func (platform *Platform) Root() outputcap.Directory { return platform.root }
func (platform *Platform) RootOpenDisposition() outputcap.RootOpenDisposition {
	return platform.disposition
}
func (platform *Platform) Close() error {
	platform.mu.Lock()
	defer platform.mu.Unlock()
	return platform.root.Close()
}
func (*Platform) IdentityScheme() outputcap.IdentitySchemeID {
	return outputcap.IdentitySchemePortableMetadataV1
}
func (*Platform) Durability() transfer.DurabilityLevel            { return transfer.DurabilityProcessRestart }
func (*Platform) ValidateModifiedTime(catalog.ModifiedTime) error { return nil }
func (platform *Platform) DestinationCapabilities() (outputcap.DestinationCapabilities, error) {
	return platform.destinationCapabilities(probeDestinationCapabilities)
}
func (platform *Platform) AcquirePublicOperationGuard() (outputcap.PublicOperationGuard, error) {
	root, err := platform.root.Duplicate()
	if err != nil {
		return nil, err
	}
	return &guard{root: root}, nil
}

type guard struct{ root outputcap.Directory }

func (g *guard) Root() outputcap.Directory { return g.root }
func (g *guard) Close() error              { return g.root.Close() }
func (*Platform) CanonicalLocatorKey(path string) (string, error) {
	canonical, err := catalog.CanonicalPath(path)
	if err != nil || canonical != path {
		return "", outputcap.ErrUnsafeNamespace
	}
	if runtime.GOOS == "windows" || runtime.GOOS == "darwin" {
		canonical = strings.ToLower(canonical)
	}
	return canonical, nil
}
func (platform *Platform) CanonicalComponentKey(name string) (string, error) {
	if err := validName(name); err != nil {
		return "", err
	}
	return platform.CanonicalLocatorKey(name)
}
