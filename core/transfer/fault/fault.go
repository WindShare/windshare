package fault

import (
	"errors"
	"fmt"
)

var ErrInvalidFault = errors.New("transfer fault is invalid")

// Domain identifies the collaborator whose contract produced a fault.
type Domain uint8

const (
	DomainSource Domain = iota + 1
	DomainCatalog
	DomainSession
	DomainOutput
	DomainCheckpoint
)

// Scope is also the severity order used by Join. The numeric order is frozen so
// Go and browser runtimes reduce sibling failures identically.
type Scope uint8

const (
	ScopeFileLocal Scope = iota + 1
	ScopeDirectoryLocal
	ScopeOutputPause
	ScopeSessionTerminal
)

type SourceCode uint16

const (
	SourceUnavailable SourceCode = iota + 1
	SourceRevisionChanged
	SourceRevisionInvalidated
	SourcePermanent
)

type CatalogCode uint16

const (
	CatalogUnavailable CatalogCode = iota + 1
	CatalogDirectoryStale
	CatalogInvalidGeneration
)

type SessionCode uint16

const (
	SessionTransport SessionCode = iota + 1
	SessionProtocol
	SessionResourceBudget
	SessionDependencyContract
)

type OutputCode uint16

const (
	OutputStateIO OutputCode = iota + 1
	OutputOwnership
	OutputNamespaceUnsafe
	OutputUnsupportedFilesystem
	OutputDirectoryBinding
	OutputDirectoryMetadata
	OutputFileAlreadyActive
	OutputResourceBudget
	OutputMutationAmbiguous
	OutputContract
)

type CheckpointCode uint16

const (
	CheckpointBusy CheckpointCode = iota + 1
	CheckpointCorruptRecord
	CheckpointUnsafeInstall
	CheckpointOwnershipMismatch
	CheckpointStateIO
)

// Fault is an immutable, comparable policy value. Scope is deliberately not
// inferred from Code: the stable-cut evidence at a boundary decides whether an
// otherwise identical failure is isolated or requires pausing wider authority.
type Fault struct {
	domain Domain
	scope  Scope
	code   uint16
}

func NewSource(scope Scope, code SourceCode) (Fault, error) {
	return newFault(DomainSource, scope, uint16(code), code.valid())
}

func NewCatalog(scope Scope, code CatalogCode) (Fault, error) {
	return newFault(DomainCatalog, scope, uint16(code), code.valid())
}

func NewSession(scope Scope, code SessionCode) (Fault, error) {
	return newFault(DomainSession, scope, uint16(code), code.valid())
}

func NewOutput(scope Scope, code OutputCode) (Fault, error) {
	return newFault(DomainOutput, scope, uint16(code), code.valid())
}

func NewCheckpoint(scope Scope, code CheckpointCode) (Fault, error) {
	return newFault(DomainCheckpoint, scope, uint16(code), code.valid())
}

func newFault(domain Domain, scope Scope, code uint16, codeValid bool) (Fault, error) {
	value := Fault{domain: domain, scope: scope, code: code}
	if !domain.valid() || !scope.valid() || !codeValid || !value.Valid() {
		return Fault{}, ErrInvalidFault
	}
	return value, nil
}

func (fault Fault) Domain() Domain { return fault.domain }
func (fault Fault) Scope() Scope   { return fault.scope }
func (fault Fault) Code() uint16   { return fault.code }
func (fault Fault) IsZero() bool   { return fault == Fault{} }

func (fault Fault) SourceCode() (SourceCode, bool) {
	code := SourceCode(fault.code)
	return code, fault.domain == DomainSource && fault.scope.valid() && code.valid()
}

func (fault Fault) CatalogCode() (CatalogCode, bool) {
	code := CatalogCode(fault.code)
	return code, fault.domain == DomainCatalog && fault.scope.valid() && code.valid()
}

func (fault Fault) SessionCode() (SessionCode, bool) {
	code := SessionCode(fault.code)
	return code, fault.domain == DomainSession && fault.scope.valid() && code.valid()
}

func (fault Fault) OutputCode() (OutputCode, bool) {
	code := OutputCode(fault.code)
	return code, fault.domain == DomainOutput && fault.scope.valid() && code.valid()
}

func (fault Fault) CheckpointCode() (CheckpointCode, bool) {
	code := CheckpointCode(fault.code)
	return code, fault.domain == DomainCheckpoint && fault.scope.valid() && code.valid()
}

func (fault Fault) Valid() bool {
	if !fault.domain.valid() || !fault.scope.valid() {
		return false
	}
	switch fault.domain {
	case DomainSource:
		return SourceCode(fault.code).valid()
	case DomainCatalog:
		return CatalogCode(fault.code).valid()
	case DomainSession:
		return SessionCode(fault.code).valid()
	case DomainOutput:
		return OutputCode(fault.code).valid()
	case DomainCheckpoint:
		return CheckpointCode(fault.code).valid()
	default:
		return false
	}
}

func (fault Fault) String() string {
	if !fault.Valid() {
		return "invalid"
	}
	return fmt.Sprintf("%s/%s/%s", fault.domain, fault.scope, fault.codeString())
}

// Compare establishes a total order for deterministic reduction. Domain and
// code break equal-severity ties only; their order does not grant extra policy
// authority to one collaborator.
func Compare(left, right Fault) int {
	leftValid, rightValid := left.Valid(), right.Valid()
	if !leftValid || !rightValid {
		switch {
		case leftValid:
			return 1
		case rightValid:
			return -1
		default:
			return 0
		}
	}
	if compared := compareUint8(uint8(left.scope), uint8(right.scope)); compared != 0 {
		return compared
	}
	if compared := compareUint8(uint8(left.domain), uint8(right.domain)); compared != 0 {
		return compared
	}
	return compareUint16(left.code, right.code)
}

// Join returns the maximum normalized fault and treats the zero value as the
// identity. Constructors are the only way external packages can create a
// non-zero Fault, so invalid values cannot silently enter policy reduction.
func Join(faults ...Fault) Fault {
	var joined Fault
	for _, candidate := range faults {
		if Compare(candidate, joined) > 0 {
			joined = candidate
		}
	}
	return joined
}

func compareUint8(left, right uint8) int {
	switch {
	case left < right:
		return -1
	case left > right:
		return 1
	default:
		return 0
	}
}

func compareUint16(left, right uint16) int {
	switch {
	case left < right:
		return -1
	case left > right:
		return 1
	default:
		return 0
	}
}

func (domain Domain) valid() bool {
	return domain >= DomainSource && domain <= DomainCheckpoint
}

func (scope Scope) valid() bool {
	return scope >= ScopeFileLocal && scope <= ScopeSessionTerminal
}

func (code SourceCode) valid() bool {
	return code >= SourceUnavailable && code <= SourcePermanent
}

func (code CatalogCode) valid() bool {
	return code >= CatalogUnavailable && code <= CatalogInvalidGeneration
}

func (code SessionCode) valid() bool {
	return code >= SessionTransport && code <= SessionDependencyContract
}

func (code OutputCode) valid() bool {
	return code >= OutputStateIO && code <= OutputContract
}

func (code CheckpointCode) valid() bool {
	return code >= CheckpointBusy && code <= CheckpointStateIO
}

func (domain Domain) String() string {
	switch domain {
	case DomainSource:
		return "source"
	case DomainCatalog:
		return "catalog"
	case DomainSession:
		return "session"
	case DomainOutput:
		return "output"
	case DomainCheckpoint:
		return "checkpoint"
	default:
		return "invalid"
	}
}

func (scope Scope) String() string {
	switch scope {
	case ScopeFileLocal:
		return "file-local"
	case ScopeDirectoryLocal:
		return "directory-local"
	case ScopeOutputPause:
		return "output-pause"
	case ScopeSessionTerminal:
		return "session-terminal"
	default:
		return "invalid"
	}
}

func (fault Fault) codeString() string {
	switch fault.domain {
	case DomainSource:
		return SourceCode(fault.code).String()
	case DomainCatalog:
		return CatalogCode(fault.code).String()
	case DomainSession:
		return SessionCode(fault.code).String()
	case DomainOutput:
		return OutputCode(fault.code).String()
	case DomainCheckpoint:
		return CheckpointCode(fault.code).String()
	default:
		return "invalid"
	}
}

func (code SourceCode) String() string {
	switch code {
	case SourceUnavailable:
		return "unavailable"
	case SourceRevisionChanged:
		return "revision-changed"
	case SourceRevisionInvalidated:
		return "revision-invalidated"
	case SourcePermanent:
		return "permanent"
	default:
		return "invalid"
	}
}

func (code CatalogCode) String() string {
	switch code {
	case CatalogUnavailable:
		return "unavailable"
	case CatalogDirectoryStale:
		return "directory-stale"
	case CatalogInvalidGeneration:
		return "invalid-generation"
	default:
		return "invalid"
	}
}

func (code SessionCode) String() string {
	switch code {
	case SessionTransport:
		return "transport"
	case SessionProtocol:
		return "protocol"
	case SessionResourceBudget:
		return "resource-budget"
	case SessionDependencyContract:
		return "dependency-contract"
	default:
		return "invalid"
	}
}

func (code OutputCode) String() string {
	switch code {
	case OutputStateIO:
		return "state-io"
	case OutputOwnership:
		return "ownership"
	case OutputNamespaceUnsafe:
		return "namespace-unsafe"
	case OutputUnsupportedFilesystem:
		return "unsupported-filesystem"
	case OutputDirectoryBinding:
		return "directory-binding"
	case OutputDirectoryMetadata:
		return "directory-metadata"
	case OutputFileAlreadyActive:
		return "file-already-active"
	case OutputResourceBudget:
		return "resource-budget"
	case OutputMutationAmbiguous:
		return "mutation-ambiguous"
	case OutputContract:
		return "contract"
	default:
		return "invalid"
	}
}

func (code CheckpointCode) String() string {
	switch code {
	case CheckpointBusy:
		return "busy"
	case CheckpointCorruptRecord:
		return "corrupt-record"
	case CheckpointUnsafeInstall:
		return "unsafe-install"
	case CheckpointOwnershipMismatch:
		return "ownership-mismatch"
	case CheckpointStateIO:
		return "state-io"
	default:
		return "invalid"
	}
}
