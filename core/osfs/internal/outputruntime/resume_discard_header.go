package outputruntime

import (
	"errors"

	"github.com/windshare/windshare/core/osfs/internal/outputcap"
	"github.com/windshare/windshare/core/osfs/internal/outputfault"
	"github.com/windshare/windshare/core/osfs/internal/outputnamespace"
	"github.com/windshare/windshare/core/osfs/internal/resumestate"
	"github.com/windshare/windshare/core/transfer"
)

func installDiscardingHeader(
	store outputnamespace.Store,
	control resumestate.Control,
	sessionDir outputcap.Directory,
	reference ResumeStateRef,
	lockPresent bool,
	verifyAuthority func() error,
) (resumestate.SessionNamespaceAuthority, bool, bool, error) {
	if reference.kind == ResumeStateOpaqueUnsafe {
		// The fixed directory identity authorizes explicit cleanup, but an opaque
		// namespace cannot authorize interpreting or replacing any contained header.
		return resumestate.SessionNamespaceAuthority{}, false, false, nil
	}
	namespace, valid, corrupt, err := readDiscardHeader(control, sessionDir, reference)
	if err != nil {
		return resumestate.SessionNamespaceAuthority{}, false, false, err
	}
	if !valid {
		return resumestate.SessionNamespaceAuthority{}, false, corrupt, nil
	}
	if err := outputnamespace.ReconcileHeaderRecordTemporaries(
		sessionDir, namespace, verifyAuthority,
	); err != nil {
		return resumestate.SessionNamespaceAuthority{}, false, false,
			intentOutputFault("reconcile discard-header update", err)
	}
	if !lockPresent {
		return authorizeLocklessDiscardHeader(namespace)
	}
	namespace, err = transitionDiscardHeader(store, sessionDir, namespace, verifyAuthority)
	if err != nil {
		return resumestate.SessionNamespaceAuthority{}, false, false, err
	}
	return namespace, true, false, nil
}

func readDiscardHeader(
	control resumestate.Control,
	sessionDir outputcap.Directory,
	reference ResumeStateRef,
) (resumestate.SessionNamespaceAuthority, bool, bool, error) {
	encoded, err := outputnamespace.ReadRecord(
		sessionDir, resumestate.HeaderRecordName, resumestate.MaxSessionHeaderBytes,
	)
	if err != nil {
		// A corrupt/missing header is itself the listed attention state. Explicit
		// discard authorizes the fixed session directory without inventing claims.
		if isMissing(err) {
			return resumestate.SessionNamespaceAuthority{}, false, false, nil
		}
		if errors.Is(err, outputcap.ErrUnsafeNamespace) {
			return resumestate.SessionNamespaceAuthority{}, false, true, nil
		}
		return resumestate.SessionNamespaceAuthority{}, false, false,
			intentOutputFault("read discard header", err)
	}
	header, err := resumestate.DecodeHeader(encoded)
	if err != nil {
		return resumestate.SessionNamespaceAuthority{}, false, true, nil
	}
	namespace, err := resumestate.BindSessionNamespaceAuthority(
		control, header, reference.namespaceName, reference.sessionName,
	)
	if err != nil || header.ResumeIntent() != reference.intent || header.SessionID() != reference.session {
		// A decodable envelope that does not bind this fixed namespace is still
		// session-local corruption. It grants no transition authority, but the
		// explicit live-pin capability may remove it after every child and lock.
		return resumestate.SessionNamespaceAuthority{}, false, true, nil
	}
	return namespace, true, false, nil
}

func authorizeLocklessDiscardHeader(
	namespace resumestate.SessionNamespaceAuthority,
) (resumestate.SessionNamespaceAuthority, bool, bool, error) {
	// Once a live process has removed the lock, only a bound terminal suffix may
	// authorize cleanup. Never synthesize a new transition in that suffix.
	lifecycle := namespace.Header().Lifecycle()
	if lifecycle != resumestate.SessionCompleting && lifecycle != resumestate.SessionDiscarding {
		return resumestate.SessionNamespaceAuthority{}, false, false, nil
	}
	return namespace, true, false, nil
}

func transitionDiscardHeader(
	store outputnamespace.Store,
	sessionDir outputcap.Directory,
	namespace resumestate.SessionNamespaceAuthority,
	verifyAuthority func() error,
) (resumestate.SessionNamespaceAuthority, error) {
	for namespace.Header().Lifecycle() != resumestate.SessionDiscarding {
		updated, err := namespace.WithLifecycle(nextDiscardLifecycle(namespace.Header().Lifecycle()))
		if err != nil {
			return resumestate.SessionNamespaceAuthority{}, intentOutputFault("transition discard header", err)
		}
		namespace, err = replaceDiscardHeader(
			store, sessionDir, namespace, updated, verifyAuthority,
		)
		if err != nil {
			return resumestate.SessionNamespaceAuthority{}, err
		}
	}
	return namespace, nil
}

func nextDiscardLifecycle(current resumestate.SessionLifecycle) resumestate.SessionLifecycle {
	if current == resumestate.SessionPausing {
		return resumestate.SessionPaused
	}
	return resumestate.SessionDiscarding
}

func replaceDiscardHeader(
	store outputnamespace.Store,
	sessionDir outputcap.Directory,
	current resumestate.SessionNamespaceAuthority,
	next resumestate.SessionNamespaceAuthority,
	verifyAuthority func() error,
) (resumestate.SessionNamespaceAuthority, error) {
	currentEncoded, err := resumestate.EncodeHeader(current.Header())
	if err != nil {
		return resumestate.SessionNamespaceAuthority{}, outputfault.New(
			transfer.OutputFaultSession, transfer.OutputFaultContract, err,
		)
	}
	nextEncoded, err := resumestate.EncodeHeader(next.Header())
	if err != nil {
		return resumestate.SessionNamespaceAuthority{}, outputfault.New(
			transfer.OutputFaultSession, transfer.OutputFaultContract, err,
		)
	}
	if err := verifyAuthority(); err != nil {
		return resumestate.SessionNamespaceAuthority{}, err
	}
	outcome, replaceErr := store.ReplaceRecord(
		sessionDir,
		resumestate.HeaderRecordName,
		outputnamespace.NewRecordImage(currentEncoded, current.Header().StateGeneration()),
		outputnamespace.NewRecordImage(nextEncoded, next.Header().StateGeneration()),
		resumestate.MaxSessionHeaderBytes,
	)
	return settleDiscardHeaderReplacement(outcome, replaceErr, next)
}

func settleDiscardHeaderReplacement(
	outcome outputnamespace.ReplaceOutcome,
	replaceErr error,
	next resumestate.SessionNamespaceAuthority,
) (resumestate.SessionNamespaceAuthority, error) {
	switch outcome {
	case outputnamespace.ReplaceAdopted:
		if replaceErr == nil {
			return next, nil
		}
		// Child deletion must not begin while the owner that adopted this header
		// still has unresolved cleanup. A retry continues from the exact generation.
		return resumestate.SessionNamespaceAuthority{}, outputfault.New(
			transfer.OutputFaultSession, transfer.OutputFaultStateIO, replaceErr,
		)
	case outputnamespace.ReplaceUnchanged:
		if replaceErr == nil {
			replaceErr = outputcap.ErrUnsafeNamespace
		}
		return resumestate.SessionNamespaceAuthority{}, outputfault.New(
			transfer.OutputFaultSession, transfer.OutputFaultStateIO, replaceErr,
		)
	case outputnamespace.ReplaceUncertain:
		return resumestate.SessionNamespaceAuthority{}, intentOutputFault(
			"replace discard header with uncertain authority",
			errors.Join(outputcap.ErrUnsafeNamespace, replaceErr),
		)
	default:
		return resumestate.SessionNamespaceAuthority{}, outputfault.New(
			transfer.OutputFaultSession, transfer.OutputFaultContract, resumestate.ErrInvalidState,
		)
	}
}
