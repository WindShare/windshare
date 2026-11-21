package outputcap

import (
	"strings"

	"github.com/windshare/windshare/core/catalog"
)

const (
	// ControlDirectoryName is the private recovery and metadata directory created
	// directly inside a bound destination container.
	ControlDirectoryName = ".windshare-output"

	// RecoveryIdentityPrefix is the prefix of directory recovery witness files
	// created directly inside an authority-created result root.
	RecoveryIdentityPrefix = ".wsresume"
)

// ReservedRootPrefix reports the private WindShare namespace prefix reserved
// for direct children of a root opened with disposition.
func ReservedRootPrefix(disposition RootOpenDisposition) string {
	switch disposition {
	case CallerProvidedContainer:
		return ControlDirectoryName
	case AuthorityCreatedRoot:
		return RecoveryIdentityPrefix
	default:
		return ""
	}
}

// ReservedRootName reports whether name, as a direct child of a root opened
// with disposition, falls in WindShare's private namespace for that root.
//
// Matching uses the catalog's Unicode 15 full-fold key so case-insensitive
// filesystems cannot alias a user entry onto a private marker or control
// directory. Both reserved prefixes begin with ASCII '.', and no other Unicode
// 15 scalar folds or canonically composes to '.', so ordinary non-dot names
// return without allocation.
func ReservedRootName(disposition RootOpenDisposition, name string) bool {
	prefix := ReservedRootPrefix(disposition)
	if prefix == "" || !strings.HasPrefix(name, ".") {
		return false
	}
	return strings.HasPrefix(catalog.SiblingNameKey(name), prefix)
}
