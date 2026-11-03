package osfs

import (
	"encoding/binary"
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/windshare/windshare/core/catalog"
)

// RootSlot indexes the retained root authorities owned by one filesystem source.
type RootSlot uint16

const sourceReferenceHeaderBytes = 3
const sourceReferenceVersion = 1
const maxNativeSourcePathBytes = catalog.MaxSourceReferenceBytes - sourceReferenceHeaderBytes

type sourceLocation struct {
	slot RootSlot
	path string
}

func (location sourceLocation) RootSlot() RootSlot   { return location.slot }
func (location sourceLocation) RelativePath() string { return location.path }

// NewSourceReference preserves native spelling and bytes independently of public
// catalog names. Normalizing this locator could open a different source object.
func NewSourceReference(slot RootSlot, relativePath string) (catalog.SourceReference, error) {
	if uint64(slot) >= catalog.MaxSelectedRoots {
		return catalog.SourceReference{}, errors.New("osfs: source root slot exceeds selected-root limit")
	}
	if err := validateSourcePath(relativePath); err != nil {
		return catalog.SourceReference{}, err
	}
	return encodeSourceReference(slot, relativePath)
}

// The parsed parent already owns the native path invariant. Only the new native
// component needs validation; catalog admission happens separately in scanChild.
func newChildSourceReference(parent sourceLocation, name string) (catalog.SourceReference, error) {
	if err := validateNativeSourceName(name); err != nil {
		return catalog.SourceReference{}, err
	}
	relative, depth := name, 1
	if parent.path != "" {
		relative, depth = parent.path+"/"+name, strings.Count(parent.path, "/")+2
	}
	if err := validateSourcePathBounds(relative, depth); err != nil {
		return catalog.SourceReference{}, err
	}
	return encodeSourceReference(parent.slot, relative)
}

func encodeSourceReference(slot RootSlot, relativePath string) (catalog.SourceReference, error) {
	raw := make([]byte, sourceReferenceHeaderBytes+len(relativePath))
	raw[0] = sourceReferenceVersion
	binary.BigEndian.PutUint16(raw[1:sourceReferenceHeaderBytes], uint16(slot))
	copy(raw[sourceReferenceHeaderBytes:], relativePath)
	return catalog.NewSourceReference(raw)
}

func parseSourceReference(reference catalog.SourceReference) (sourceLocation, error) {
	raw := reference.Bytes()
	if len(raw) < sourceReferenceHeaderBytes || raw[0] != sourceReferenceVersion {
		return sourceLocation{}, errors.New("osfs: invalid filesystem source reference")
	}
	slot := RootSlot(binary.BigEndian.Uint16(raw[1:sourceReferenceHeaderBytes]))
	path := string(raw[sourceReferenceHeaderBytes:])
	if uint64(slot) >= catalog.MaxSelectedRoots {
		return sourceLocation{}, errors.New("osfs: source root slot exceeds selected-root limit")
	}
	if err := validateSourcePath(path); err != nil {
		return sourceLocation{}, err
	}
	return sourceLocation{slot: slot, path: path}, nil
}

func validateSourcePath(path string) error {
	if path == "" {
		return nil
	}
	components := strings.Split(path, "/")
	if err := validateSourcePathBounds(path, len(components)); err != nil {
		return err
	}
	for index, component := range components {
		if err := validateNativeSourceName(component); err != nil {
			return fmt.Errorf("component %d: %w", index, err)
		}
	}
	return nil
}

func validateSourcePathBounds(path string, depth int) error {
	if len(path) > maxNativeSourcePathBytes {
		return fmt.Errorf("%w: native source path is %d bytes; limit is %d", catalog.ErrInvalidPath, len(path), maxNativeSourcePathBytes)
	}
	if depth > catalog.MaxPathDepth {
		return fmt.Errorf("%w: got %d components", catalog.ErrPathTooDeep, depth)
	}
	return nil
}

func validateNativeSourceName(name string) error {
	if name == "" || name == "." || name == ".." || strings.ContainsAny(name, "/\x00") {
		return fmt.Errorf("%w: native source name must be a single relative component", catalog.ErrInvalidPath)
	}
	// Backslash and colon are ordinary Unix bytes, but invoke separators or
	// alternate streams on Windows. Private locators follow their owning host.
	if os.PathSeparator == '\\' && strings.ContainsAny(name, "\\:") {
		return fmt.Errorf("%w: native source name contains Windows path syntax", catalog.ErrInvalidPath)
	}
	return nil
}
