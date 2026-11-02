package osfs

import (
	"encoding/binary"
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/windshare/windshare/core/catalog"
)

// RootSlot indexes the retained root authorities owned by one filesystem source.
type RootSlot uint16

const sourceReferenceHeaderBytes = 3
const sourceReferenceVersion = 1

type sourceLocation struct {
	slot RootSlot
	path string
}

func (location sourceLocation) RootSlot() RootSlot   { return location.slot }
func (location sourceLocation) RelativePath() string { return location.path }

// NewSourceReference preserves native spelling; canonical public names must not
// change which inode the private reference opens on byte-sensitive filesystems.
func NewSourceReference(slot RootSlot, relativePath string) (catalog.SourceReference, error) {
	if uint64(slot) >= catalog.MaxSelectedRoots {
		return catalog.SourceReference{}, errors.New("osfs: source root slot exceeds selected-root limit")
	}
	if err := validateSourcePath(relativePath); err != nil {
		return catalog.SourceReference{}, err
	}
	return encodeSourceReference(slot, relativePath)
}

// errUnrepresentableSourceName marks a child whose own name fails
// validateSourceName, as opposed to a joined path that is too long or deep.
var errUnrepresentableSourceName = errors.New("osfs: source name is outside the catalog name grammar")

// newChildSourceReference matches NewSourceReference(parent.slot, parent.path+"/"+name)
// for a parent that came from parseSourceReference. Only the new name is validated,
// so a scan normalizes each native name once instead of once per descendant.
func newChildSourceReference(parent sourceLocation, name string) (catalog.SourceReference, error) {
	if err := validateSourceName(name); err != nil {
		return catalog.SourceReference{}, fmt.Errorf("%w: %w: %w", catalog.ErrInvalidPath, errUnrepresentableSourceName, err)
	}
	relative, depth := name, 1
	if parent.path != "" {
		relative, depth = parent.path+"/"+name, strings.Count(parent.path, "/")+2
	}
	if len(relative) > catalog.MaxPathBytes {
		return catalog.SourceReference{}, fmt.Errorf("%w: source path must be relative, valid UTF-8, and slash-separated", catalog.ErrInvalidPath)
	}
	if depth > catalog.MaxPathDepth {
		return catalog.SourceReference{}, fmt.Errorf("%w: got %d components", catalog.ErrPathTooDeep, depth)
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
	if !utf8.ValidString(path) || len(path) > catalog.MaxPathBytes || strings.ContainsRune(path, '\\') ||
		strings.HasPrefix(path, "/") || strings.HasSuffix(path, "/") {
		return fmt.Errorf("%w: source path must be relative, valid UTF-8, and slash-separated", catalog.ErrInvalidPath)
	}
	components := strings.Split(path, "/")
	if len(components) > catalog.MaxPathDepth {
		return fmt.Errorf("%w: got %d components", catalog.ErrPathTooDeep, len(components))
	}
	for index, component := range components {
		if err := validateSourceName(component); err != nil {
			return fmt.Errorf("%w: component %d: %w", catalog.ErrInvalidPath, index, err)
		}
	}
	return nil
}

// validateSourceName is the one rule for whether a native name can appear in a
// source reference. Both the native and the NFC spelling must fit the byte limit:
// the reference keeps native bytes, while catalog records carry the NFC name.
func validateSourceName(name string) error {
	if len(name) > catalog.MaxNameBytes {
		return fmt.Errorf("%w: native name is %d bytes", catalog.ErrInvalidName, len(name))
	}
	_, err := catalog.CanonicalName(name)
	return err
}
