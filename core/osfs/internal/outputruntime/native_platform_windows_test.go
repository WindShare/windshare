//go:build windows

package outputruntime

import (
	"fmt"
	"path/filepath"
	"strings"
)

func requireWindowsRuntimeTestChild(base, child string) error {
	baseAbsolute, err := filepath.Abs(base)
	if err != nil {
		return fmt.Errorf("resolve base: %w", err)
	}
	childAbsolute, err := filepath.Abs(child)
	if err != nil {
		return fmt.Errorf("resolve child: %w", err)
	}
	relative, err := filepath.Rel(filepath.Clean(baseAbsolute), filepath.Clean(childAbsolute))
	if err != nil {
		return fmt.Errorf("relativize child: %w", err)
	}
	if relative == "." || relative == ".." || filepath.IsAbs(relative) ||
		strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return fmt.Errorf("child %q is not strictly beneath base %q", childAbsolute, baseAbsolute)
	}
	return nil
}
