package transfer

import (
	"strings"

	"github.com/windshare/windshare/core/catalog"
	"github.com/windshare/windshare/core/transfer/fault"
)

// DirectoryAdmission is an immutable receipt identifying one directory admitted
// by a DirectTreeSession for a specific catalog generation and destination path.
type DirectoryAdmission struct {
	directory  catalog.DirectoryID
	generation catalog.DirectoryGeneration
	path       string
	parentPath string
	hasParent  bool
	modified   catalog.ModifiedTime
	valid      bool
}

// NewDirectoryAdmission constructs a validated directory admission from a
// projected MaterializationDirectory.
func NewDirectoryAdmission(directory MaterializationDirectory) (DirectoryAdmission, error) {
	if !directory.Valid() {
		return DirectoryAdmission{}, ErrInvalidDirectoryAdmission
	}
	path := directory.Path().String()
	canonical, err := catalog.CanonicalPath(path)
	if path == "" || err != nil || canonical != path {
		return DirectoryAdmission{}, ErrInvalidDirectoryAdmission
	}
	parent := directory.ParentAdmission()
	hasParent := !parent.IsZero()
	parentPath := ""
	if hasParent {
		if !parent.valid || !immediateDirectoryChild(parent.path, path) {
			return DirectoryAdmission{}, ErrInvalidDirectoryAdmission
		}
		parentPath = parent.path
	}
	return DirectoryAdmission{
		directory:  directory.DirectoryID(),
		generation: directory.Generation(),
		path:       path,
		parentPath: parentPath,
		hasParent:  hasParent,
		modified:   directory.ModifiedTime(),
		valid:      true,
	}, nil
}

func (admission DirectoryAdmission) DirectoryID() catalog.DirectoryID { return admission.directory }
func (admission DirectoryAdmission) Generation() catalog.DirectoryGeneration {
	return admission.generation
}
func (admission DirectoryAdmission) Path() string                       { return admission.path }
func (admission DirectoryAdmission) ParentPath() string                 { return admission.parentPath }
func (admission DirectoryAdmission) HasParent() bool                    { return admission.hasParent }
func (admission DirectoryAdmission) ModifiedTime() catalog.ModifiedTime { return admission.modified }
func (admission DirectoryAdmission) IsZero() bool                       { return !admission.valid }

func (admission DirectoryAdmission) Equal(other DirectoryAdmission) bool {
	return admission.valid && admission == other
}

func immediateDirectoryChild(parent, child string) bool {
	if parent == "" || child == "" {
		return false
	}
	parentPath, _, found := strings.CutLast(child, "/")
	return found && parent == parentPath
}

func validateDirectoryAdmission(
	admission DirectoryAdmission,
	directory MaterializationDirectory,
) error {
	expected, err := NewDirectoryAdmission(directory)
	if err != nil || !admission.Equal(expected) {
		return ErrDirectoryAdmissionMismatch
	}
	return nil
}

// DirectorySettlementKind distinguishes a successful metadata seal from a
// reconciled, directory-local metadata failure. Ambiguous mutation is returned
// as an error because it cannot safely become a cached terminal settlement.
type DirectorySettlementKind uint8

const (
	DirectoryFinalized DirectorySettlementKind = iota + 1
	DirectoryIsolatedFailure
)

// DirectorySettlement is an immutable terminal result for one admitted directory.
type DirectorySettlement struct {
	kind      DirectorySettlementKind
	admission DirectoryAdmission
	failure   fault.Fault
}

func NewFinalizedDirectorySettlement(admission DirectoryAdmission) (DirectorySettlement, error) {
	settlement := DirectorySettlement{kind: DirectoryFinalized, admission: admission}
	if !settlement.valid() {
		return DirectorySettlement{}, ErrInvalidOutputSettlement
	}
	return settlement, nil
}

func NewIsolatedDirectorySettlement(
	admission DirectoryAdmission,
	failure fault.Fault,
) (DirectorySettlement, error) {
	settlement := DirectorySettlement{
		kind: DirectoryIsolatedFailure, admission: admission, failure: failure,
	}
	if !settlement.valid() {
		return DirectorySettlement{}, ErrInvalidOutputSettlement
	}
	return settlement, nil
}

func (settlement DirectorySettlement) Kind() DirectorySettlementKind { return settlement.kind }
func (settlement DirectorySettlement) Admission() DirectoryAdmission { return settlement.admission }

func (settlement DirectorySettlement) IsolatedFault() (fault.Fault, bool) {
	return settlement.failure,
		settlement.kind == DirectoryIsolatedFailure && settlement.failure.Valid()
}

func (settlement DirectorySettlement) valid() bool {
	if settlement.admission.IsZero() {
		return false
	}
	switch settlement.kind {
	case DirectoryFinalized:
		return settlement.failure.IsZero()
	case DirectoryIsolatedFailure:
		code, outputFault := settlement.failure.OutputCode()
		return outputFault && settlement.failure.Scope() == fault.ScopeDirectoryLocal &&
			code == fault.OutputDirectoryMetadata
	default:
		return false
	}
}

func validateDirectorySettlement(
	expected DirectoryAdmission,
	settlement DirectorySettlement,
) error {
	if expected.IsZero() || !settlement.valid() || !settlement.Admission().Equal(expected) {
		return outputContractFault(nil)
	}
	return nil
}
