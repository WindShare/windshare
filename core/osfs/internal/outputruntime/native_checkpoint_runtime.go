package outputruntime

import (
	"context"
	"errors"
	"fmt"

	"github.com/windshare/windshare/core/osfs/internal/checkpointmodel"
	"github.com/windshare/windshare/core/osfs/internal/checkpointstore"
	"github.com/windshare/windshare/core/osfs/internal/outputcap"
	"github.com/windshare/windshare/core/transfer"
	transferfault "github.com/windshare/windshare/core/transfer/fault"
	"github.com/windshare/windshare/core/transfer/receivecontract"
)

func initializeNativeCheckpointNamespace(
	platform outputcap.Platform,
	authorityRef receivecontract.AuthorityRef,
) (checkpointstore.Namespace, checkpointmodel.Ownership, error) {
	return nativeCheckpointNamespace(platform, authorityRef, true)
}

func openNativeCheckpointNamespace(
	platform outputcap.Platform,
	authorityRef receivecontract.AuthorityRef,
) (checkpointstore.Namespace, checkpointmodel.Ownership, error) {
	return nativeCheckpointNamespace(platform, authorityRef, false)
}

func nativeCheckpointNamespace(
	platform outputcap.Platform,
	authorityRef receivecontract.AuthorityRef,
	create bool,
) (checkpointstore.Namespace, checkpointmodel.Ownership, error) {
	if platform == nil || platform.Root() == nil || authorityRef.IsZero() {
		return checkpointstore.Namespace{}, checkpointmodel.Ownership{}, transfer.ErrInvalidOutputBinding
	}
	disposition := platform.RootOpenDisposition()
	namespace, ownership, err := nativeCheckpointNamespaceForDisposition(
		platform, authorityRef, disposition, create,
	)
	if err == nil {
		return namespace, ownership, nil
	}
	var checkpointErr *checkpointstore.Error
	if disposition != outputcap.CallerProvidedContainer || !errors.As(err, &checkpointErr) ||
		checkpointErr.Code() != checkpointstore.ErrorOwnershipMismatch {
		return checkpointstore.Namespace{}, checkpointmodel.Ownership{}, err
	}
	// Path existence after restart cannot recover creation authority. Only the
	// exact durable marker may prove the originally authority-created root.
	return nativeCheckpointNamespaceForDisposition(
		platform, authorityRef, outputcap.AuthorityCreatedRoot, create,
	)
}

func nativeCheckpointNamespaceForDisposition(
	platform outputcap.Platform,
	authorityRef receivecontract.AuthorityRef,
	disposition outputcap.RootOpenDisposition,
	create bool,
) (checkpointstore.Namespace, checkpointmodel.Ownership, error) {
	ownership, err := nativeCheckpointOwnership(platform, authorityRef, disposition)
	if err != nil {
		return checkpointstore.Namespace{}, checkpointmodel.Ownership{}, err
	}
	config := checkpointstore.CertifiedConfig{Root: platform.Root(), Ownership: ownership}
	var namespace checkpointstore.Namespace
	if create {
		namespace, err = checkpointstore.Initialize(config)
	} else {
		namespace, err = checkpointstore.OpenNamespace(config)
	}
	if err != nil {
		return checkpointstore.Namespace{}, checkpointmodel.Ownership{}, err
	}
	return namespace, ownership, nil
}

func nativeCheckpointOwnership(
	platform outputcap.Platform,
	authorityRef receivecontract.AuthorityRef,
	disposition outputcap.RootOpenDisposition,
) (checkpointmodel.Ownership, error) {
	if platform == nil {
		return checkpointmodel.Ownership{}, transfer.ErrInvalidOutputBinding
	}
	return checkpointmodel.NewOwnership(checkpointmodel.OwnershipSpec{
		Materializer: checkpointmodel.MaterializerNativeTree, Certification: platform.Certification(),
		AuthorityRef: authorityRef.Bytes(), RootOpenDisposition: disposition,
	})
}

func checkpointRuntimeError(ctx context.Context, operation string, cause error) error {
	if cause == nil {
		return nil
	}
	if ctx != nil && ctx.Err() != nil {
		return ctx.Err()
	}
	if errors.Is(cause, context.Canceled) || errors.Is(cause, context.DeadlineExceeded) {
		return cause
	}
	return transferfault.Wrap(checkpointRuntimeFault(cause), fmt.Errorf("%s: %w", operation, cause))
}

func checkpointRuntimeFault(cause error) transferfault.Fault {
	var checkpointErr *checkpointstore.Error
	if !errors.As(cause, &checkpointErr) || checkpointErr == nil {
		return transferfault.DependencyContractFault()
	}
	var code transferfault.CheckpointCode
	switch checkpointErr.Code() {
	case checkpointstore.ErrorBusy:
		code = transferfault.CheckpointBusy
	case checkpointstore.ErrorCorruptRecord:
		code = transferfault.CheckpointCorruptRecord
	case checkpointstore.ErrorUnsafeInstall:
		code = transferfault.CheckpointUnsafeInstall
	case checkpointstore.ErrorOwnershipMismatch:
		code = transferfault.CheckpointOwnershipMismatch
	case checkpointstore.ErrorStateIO:
		code = transferfault.CheckpointStateIO
	default:
		return transferfault.DependencyContractFault()
	}
	value, _ := transferfault.NewCheckpoint(transferfault.ScopeOutputPause, code)
	return value
}
