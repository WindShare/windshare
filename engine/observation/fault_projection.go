package observation

import (
	transferfault "github.com/windshare/windshare/core/transfer/fault"
)

func ProjectFault(value transferfault.Fault) (Failure, bool) {
	if !value.Valid() {
		return Failure{}, false
	}
	domain, ok := projectFaultDomain(value.Domain())
	if !ok {
		return Failure{}, false
	}
	scope, ok := projectFaultScope(value.Scope())
	if !ok {
		return Failure{}, false
	}
	code, ok := projectFaultCode(value)
	if !ok {
		return Failure{}, false
	}
	context, err := NewFaultContext(domain, scope, value.Code())
	if err != nil {
		return Failure{}, false
	}
	failure, err := NewFaultFailure(code, context)
	return failure, err == nil
}

func ProjectNormalizedFault(domain uint8, scope uint8, code uint16) (Failure, bool) {
	projectedDomain, ok := projectFaultDomain(transferfault.Domain(domain))
	if !ok {
		return Failure{}, false
	}
	projectedScope, ok := projectFaultScope(transferfault.Scope(scope))
	if !ok {
		return Failure{}, false
	}
	projectedCode, ok := projectNumericFaultCode(transferfault.Domain(domain), code)
	if !ok {
		return Failure{}, false
	}
	context, err := NewFaultContext(projectedDomain, projectedScope, code)
	if err != nil {
		return Failure{}, false
	}
	failure, err := NewFaultFailure(projectedCode, context)
	return failure, err == nil
}

func projectNumericFaultCode(domain transferfault.Domain, code uint16) (FailureCode, bool) {
	switch domain {
	case transferfault.DomainSource:
		switch transferfault.SourceCode(code) {
		case transferfault.SourceUnavailable:
			return FailureSourceUnavailable, true
		case transferfault.SourceRevisionChanged:
			return FailureSourceRevisionChanged, true
		case transferfault.SourceRevisionInvalidated:
			return FailureSourceRevisionInvalidated, true
		case transferfault.SourcePermanent:
			return FailureSourcePermanent, true
		}
	case transferfault.DomainCatalog:
		switch transferfault.CatalogCode(code) {
		case transferfault.CatalogUnavailable:
			return FailureCatalogUnavailable, true
		case transferfault.CatalogDirectoryStale:
			return FailureCatalogDirectoryStale, true
		case transferfault.CatalogInvalidGeneration:
			return FailureCatalogInvalidGeneration, true
		}
	case transferfault.DomainSession:
		switch transferfault.SessionCode(code) {
		case transferfault.SessionTransport:
			return FailureSessionTransport, true
		case transferfault.SessionProtocol:
			return FailureSessionProtocol, true
		case transferfault.SessionResourceBudget:
			return FailureSessionResourceBudget, true
		case transferfault.SessionDependencyContract:
			return FailureSessionDependencyContract, true
		}
	case transferfault.DomainOutput:
		switch transferfault.OutputCode(code) {
		case transferfault.OutputStateIO:
			return FailureOutputStateIO, true
		case transferfault.OutputOwnership:
			return FailureOutputOwnership, true
		case transferfault.OutputNamespaceUnsafe:
			return FailureOutputNamespaceUnsafe, true
		case transferfault.OutputUnsupportedFilesystem:
			return FailureOutputUnsupportedFilesystem, true
		case transferfault.OutputDirectoryBinding:
			return FailureOutputDirectoryBinding, true
		case transferfault.OutputDirectoryMetadata:
			return FailureOutputDirectoryMetadata, true
		case transferfault.OutputFileAlreadyActive:
			return FailureOutputFileAlreadyActive, true
		case transferfault.OutputResourceBudget:
			return FailureOutputResourceBudget, true
		case transferfault.OutputMutationAmbiguous:
			return FailureOutputMutationAmbiguous, true
		case transferfault.OutputContract:
			return FailureOutputContract, true
		}
	case transferfault.DomainCheckpoint:
		switch transferfault.CheckpointCode(code) {
		case transferfault.CheckpointBusy:
			return FailureCheckpointBusy, true
		case transferfault.CheckpointCorruptRecord:
			return FailureCheckpointCorruptRecord, true
		case transferfault.CheckpointUnsafeInstall:
			return FailureCheckpointUnsafeInstall, true
		case transferfault.CheckpointOwnershipMismatch:
			return FailureCheckpointOwnershipMismatch, true
		case transferfault.CheckpointStateIO:
			return FailureCheckpointStateIO, true
		}
	}
	return 0, false
}

func projectFaultDomain(value transferfault.Domain) (FaultDomain, bool) {
	switch value {
	case transferfault.DomainSource:
		return FaultSource, true
	case transferfault.DomainCatalog:
		return FaultCatalog, true
	case transferfault.DomainSession:
		return FaultSession, true
	case transferfault.DomainOutput:
		return FaultOutput, true
	case transferfault.DomainCheckpoint:
		return FaultCheckpoint, true
	default:
		return 0, false
	}
}

func projectFaultScope(value transferfault.Scope) (FaultScope, bool) {
	switch value {
	case transferfault.ScopeFileLocal:
		return FaultFileLocal, true
	case transferfault.ScopeDirectoryLocal:
		return FaultDirectoryLocal, true
	case transferfault.ScopeOutputPause:
		return FaultOutputPause, true
	case transferfault.ScopeSessionTerminal:
		return FaultSessionTerminal, true
	default:
		return 0, false
	}
}

func projectFaultCode(value transferfault.Fault) (FailureCode, bool) {
	switch value.Domain() {
	case transferfault.DomainSource:
		code, ok := value.SourceCode()
		if !ok {
			return 0, false
		}
		return projectSourceFaultCode(code)
	case transferfault.DomainCatalog:
		code, ok := value.CatalogCode()
		if !ok {
			return 0, false
		}
		return projectCatalogFaultCode(code)
	case transferfault.DomainSession:
		code, ok := value.SessionCode()
		if !ok {
			return 0, false
		}
		return projectSessionFaultCode(code)
	case transferfault.DomainOutput:
		code, ok := value.OutputCode()
		if !ok {
			return 0, false
		}
		return projectOutputFaultCode(code)
	case transferfault.DomainCheckpoint:
		code, ok := value.CheckpointCode()
		if !ok {
			return 0, false
		}
		return projectCheckpointFaultCode(code)
	}
	return 0, false
}

func projectSourceFaultCode(code transferfault.SourceCode) (FailureCode, bool) {
	switch code {
	case transferfault.SourceUnavailable:
		return FailureSourceUnavailable, true
	case transferfault.SourceRevisionChanged:
		return FailureSourceRevisionChanged, true
	case transferfault.SourceRevisionInvalidated:
		return FailureSourceRevisionInvalidated, true
	case transferfault.SourcePermanent:
		return FailureSourcePermanent, true
	default:
		return 0, false
	}
}

func projectCatalogFaultCode(code transferfault.CatalogCode) (FailureCode, bool) {
	switch code {
	case transferfault.CatalogUnavailable:
		return FailureCatalogUnavailable, true
	case transferfault.CatalogDirectoryStale:
		return FailureCatalogDirectoryStale, true
	case transferfault.CatalogInvalidGeneration:
		return FailureCatalogInvalidGeneration, true
	default:
		return 0, false
	}
}

func projectSessionFaultCode(code transferfault.SessionCode) (FailureCode, bool) {
	switch code {
	case transferfault.SessionTransport:
		return FailureSessionTransport, true
	case transferfault.SessionProtocol:
		return FailureSessionProtocol, true
	case transferfault.SessionResourceBudget:
		return FailureSessionResourceBudget, true
	case transferfault.SessionDependencyContract:
		return FailureSessionDependencyContract, true
	default:
		return 0, false
	}
}

func projectOutputFaultCode(code transferfault.OutputCode) (FailureCode, bool) {
	switch code {
	case transferfault.OutputStateIO:
		return FailureOutputStateIO, true
	case transferfault.OutputOwnership:
		return FailureOutputOwnership, true
	case transferfault.OutputNamespaceUnsafe:
		return FailureOutputNamespaceUnsafe, true
	case transferfault.OutputUnsupportedFilesystem:
		return FailureOutputUnsupportedFilesystem, true
	case transferfault.OutputDirectoryBinding:
		return FailureOutputDirectoryBinding, true
	case transferfault.OutputDirectoryMetadata:
		return FailureOutputDirectoryMetadata, true
	case transferfault.OutputFileAlreadyActive:
		return FailureOutputFileAlreadyActive, true
	case transferfault.OutputResourceBudget:
		return FailureOutputResourceBudget, true
	case transferfault.OutputMutationAmbiguous:
		return FailureOutputMutationAmbiguous, true
	case transferfault.OutputContract:
		return FailureOutputContract, true
	default:
		return 0, false
	}
}

func projectCheckpointFaultCode(code transferfault.CheckpointCode) (FailureCode, bool) {
	switch code {
	case transferfault.CheckpointBusy:
		return FailureCheckpointBusy, true
	case transferfault.CheckpointCorruptRecord:
		return FailureCheckpointCorruptRecord, true
	case transferfault.CheckpointUnsafeInstall:
		return FailureCheckpointUnsafeInstall, true
	case transferfault.CheckpointOwnershipMismatch:
		return FailureCheckpointOwnershipMismatch, true
	case transferfault.CheckpointStateIO:
		return FailureCheckpointStateIO, true
	default:
		return 0, false
	}
}
