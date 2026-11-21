package outputcap

import (
	"strings"

	"github.com/windshare/windshare/core/catalog"
	"github.com/windshare/windshare/core/transfer/receivecontract"
)

const (
	// ControlDirectoryName is the private recovery and metadata directory created
	// directly inside a bound destination container.
	ControlDirectoryName = ".windshare-output"

	// RecoveryIdentityPrefix is the prefix of directory recovery witness files
	// created directly inside an authority-created result root.
	RecoveryIdentityPrefix = ".wsresume"
)

// ReservedNamespaceScope identifies which private WindShare namespace shares
// direct children with a native execution root. It is orthogonal to
// RootOpenDisposition because a newly created destination container has
// AuthorityCreatedRoot ownership yet still guards the container control
// namespace rather than a result-root recovery witness prefix.
type ReservedNamespaceScope uint8

const (
	// ReservedNamespaceDestinationContainer guards WindShare's private control
	// directory directly inside a bound destination container.
	ReservedNamespaceDestinationContainer ReservedNamespaceScope = iota + 1

	// ReservedNamespaceResultRoot guards WindShare's private recovery witness
	// files directly inside an authority-created result root.
	ReservedNamespaceResultRoot
)

// Valid reports whether scope names a defined reserved root namespace.
func (scope ReservedNamespaceScope) Valid() bool {
	return scope == ReservedNamespaceDestinationContainer || scope == ReservedNamespaceResultRoot
}

// ReservedNamespacePrefix reports the private WindShare namespace prefix
// reserved for direct children of a root with scope.
func ReservedNamespacePrefix(scope ReservedNamespaceScope) string {
	switch scope {
	case ReservedNamespaceDestinationContainer:
		return ControlDirectoryName
	case ReservedNamespaceResultRoot:
		return RecoveryIdentityPrefix
	default:
		return ""
	}
}

// ReservedRootName reports whether name, as a direct child of a root with
// scope, falls in WindShare's private namespace for that root.
//
// Matching uses the catalog's Unicode 15 full-fold key so case-insensitive
// filesystems cannot alias a user entry onto a private marker or control
// directory. Both reserved prefixes begin with ASCII '.', and no other Unicode
// 15 scalar folds or canonically composes to '.', so ordinary non-dot names
// return without allocation.
func ReservedRootName(scope ReservedNamespaceScope, name string) bool {
	prefix := ReservedNamespacePrefix(scope)
	if prefix == "" || !strings.HasPrefix(name, ".") {
		return false
	}
	return strings.HasPrefix(catalog.SiblingNameKey(name), prefix)
}

// ReservedNamespaceScopeForEntry maps a top-level reservation entry kind to
// the private WindShare namespace that shares direct children with its
// execution root.
func ReservedNamespaceScopeForEntry(kind receivecontract.ContainerEntryKind) ReservedNamespaceScope {
	switch kind {
	case receivecontract.ContainerEntryResultRoot:
		return ReservedNamespaceResultRoot
	case receivecontract.ContainerEntrySingleFile:
		return ReservedNamespaceDestinationContainer
	default:
		return 0
	}
}

// CompatibleNameReport summarizes compatible-name repairs exported beside a
// destination when an operation settles or is discarded. Either all fields are
// zero or all fields describe an exported report.
type CompatibleNameReport struct {
	RepairedCount uint64
	ReportName    string
	PageCount     uint64
}

func (report CompatibleNameReport) Empty() bool {
	return report.RepairedCount == 0 && report.ReportName == "" && report.PageCount == 0
}

func (report CompatibleNameReport) Valid() bool {
	return report.Empty() || (report.RepairedCount > 0 && report.ReportName != "" && report.PageCount > 0)
}
