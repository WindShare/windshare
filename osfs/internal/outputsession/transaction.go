package outputsession

import (
	"context"

	"github.com/windshare/windshare/core/transfer"
	"github.com/windshare/windshare/core/transfer/fault"
)

type transactionAction uint8

const (
	actionBeginSettlement transactionAction = iota + 1
	actionWrite
	actionCheckpoint
	actionCommit
	actionPause
	actionRetire
)

func (action transactionAction) terminal() bool {
	return action == actionCommit || action == actionPause || action == actionRetire
}

type guardedTransaction struct {
	session  *Session
	claimID  ClaimID
	executor FileTransactionExecutor
	binding  transfer.MaterializedFileBinding
}

var _ transfer.FileTransaction = (*guardedTransaction)(nil)

func (transaction *guardedTransaction) Binding() transfer.MaterializedFileBinding {
	if transaction == nil {
		return transfer.MaterializedFileBinding{}
	}
	return transaction.binding
}

func (transaction *guardedTransaction) WriteRange(
	ctx context.Context,
	offset uint64,
	data []byte,
) error {
	lease, operationID, err := transaction.begin(ctx)
	if err != nil {
		return err
	}
	defer lease.release()
	writeArgument := uint8(0)
	if len(data) != 0 {
		writeArgument = 1
	}
	entry, operation, _, event, err := transaction.reserve(operationID, actionWrite, writeArgument)
	if err != nil {
		transaction.session.emit(event)
		return err
	}
	cut, executeErr := transaction.executor.WriteRange(ctx, offset, data)
	return transaction.finishNonterminal(ctx, operationID, entry, operation, cut, executeErr, transfer.VerifiedDurableRanges{})
}

func (transaction *guardedTransaction) Checkpoint(
	ctx context.Context,
) (transfer.VerifiedDurableRanges, error) {
	lease, operationID, err := transaction.begin(ctx)
	if err != nil {
		return transfer.VerifiedDurableRanges{}, err
	}
	defer lease.release()
	entry, operation, _, event, err := transaction.reserve(operationID, actionCheckpoint, 0)
	if err != nil {
		transaction.session.emit(event)
		return transfer.VerifiedDurableRanges{}, err
	}
	checkpoint, cut, executeErr := transaction.executor.Checkpoint(ctx)
	err = transaction.finishNonterminal(ctx, operationID, entry, operation, cut, executeErr, checkpoint)
	if err != nil {
		return transfer.VerifiedDurableRanges{}, err
	}
	return checkpoint, nil
}

func (transaction *guardedTransaction) Commit(
	ctx context.Context,
) (transfer.FileSettlement, error) {
	return transaction.terminalOperation(ctx, actionCommit, 0, func() (transfer.FileSettlement, MutationCut, error) {
		return transaction.executor.Commit(ctx)
	})
}

func (transaction *guardedTransaction) Pause(
	ctx context.Context,
	reason transfer.FilePauseReason,
) (transfer.FileSettlement, error) {
	if reason < transfer.FilePauseInterrupted || reason > transfer.FilePauseDependencyContract {
		return transfer.FileSettlement{}, executorContractError(transfer.ErrInvalidOutputSettlement)
	}
	return transaction.terminalOperation(ctx, actionPause, uint8(reason), func() (transfer.FileSettlement, MutationCut, error) {
		return transaction.executor.Pause(ctx, reason)
	})
}

func (transaction *guardedTransaction) Retire(
	ctx context.Context,
	reason transfer.FileRetireReason,
) (transfer.FileSettlement, error) {
	if reason < transfer.FileRetireIsolatedPermanentSourceFailure || reason > transfer.FileRetireInvalidatedRevision {
		return transfer.FileSettlement{}, executorContractError(transfer.ErrInvalidOutputSettlement)
	}
	return transaction.terminalOperation(ctx, actionRetire, uint8(reason), func() (transfer.FileSettlement, MutationCut, error) {
		return transaction.executor.Retire(ctx, reason)
	})
}

func (transaction *guardedTransaction) begin(
	ctx context.Context,
) (*operationLease, uint64, error) {
	if transaction == nil || transaction.session == nil || transaction.executor == nil || ctx == nil {
		return nil, 0, executorContractError(ErrInvalidConfiguration)
	}
	if err := ctx.Err(); err != nil {
		return nil, 0, err
	}
	return transaction.session.beginOperation()
}

func (transaction *guardedTransaction) reserve(
	operationID uint64,
	action transactionAction,
	argument uint8,
) (*fileEntry, *fileTransactionOperation, bool, TraceEvent, error) {
	session := transaction.session
	session.mu.Lock()
	defer session.mu.Unlock()
	entry := session.fileClaims[transaction.claimID]
	if entry == nil || entry.transaction != transaction {
		err := session.markInvariantFailureLocked()
		event := session.traceLocked(operationID, operationKindForAction(action), TraceRejected,
			transaction.claimID, ClaimFile, ClaimActive, ClaimActive, session.requiredFault)
		return nil, nil, false, event, err
	}
	state := fileClaimState(entry.state)
	if entry.state == fileSettled {
		if action.terminal() && entry.terminalAction == action && entry.terminalArgument == argument {
			event := session.traceLocked(operationID, operationKindForAction(action), TraceSettled,
				entry.claim.id, ClaimFile, state, state, fault.Fault{})
			return entry, nil, false, event, nil
		}
		value, _ := fault.NewOutput(fault.ScopeFileLocal, fault.OutputContract)
		err := outputFault(fault.ScopeFileLocal, fault.OutputContract, ErrTransactionOperationConflict)
		event := session.traceLocked(operationID, operationKindForAction(action), TraceRejected,
			entry.claim.id, ClaimFile, state, state, value)
		return nil, nil, false, event, err
	}
	if entry.state != fileActive {
		err := session.markInvariantFailureLocked()
		event := session.traceLocked(operationID, operationKindForAction(action), TraceRejected,
			entry.claim.id, ClaimFile, state, state, session.requiredFault)
		return nil, nil, false, event, err
	}
	if entry.uncertain {
		err := session.operationRejectionLocked()
		if err == nil {
			err = session.markInvariantFailureLocked()
		}
		event := session.traceLocked(operationID, operationKindForAction(action), TraceRejected,
			entry.claim.id, ClaimFile, state, state, session.requiredFault)
		return nil, nil, false, event, err
	}
	if err := session.operationRejectionLocked(); err != nil {
		event := session.traceLocked(operationID, operationKindForAction(action), TraceRejected,
			entry.claim.id, ClaimFile, state, state, session.requiredFault)
		return nil, nil, false, event, err
	}
	if entry.operation != nil {
		if action.terminal() && entry.operation.action == action && entry.operation.argument == argument {
			event := session.traceLocked(operationID, operationKindForAction(action), TraceCoalesced,
				entry.claim.id, ClaimFile, state, state, fault.Fault{})
			return entry, entry.operation, false, event, nil
		}
		value, _ := fault.NewOutput(fault.ScopeFileLocal, fault.OutputFileAlreadyActive)
		err := alreadyActiveError()
		event := session.traceLocked(operationID, operationKindForAction(action), TraceRejected,
			entry.claim.id, ClaimFile, state, state, value)
		return nil, nil, false, event, err
	}
	operation := &fileTransactionOperation{done: make(chan struct{}), action: action, argument: argument}
	entry.operation = operation
	return entry, operation, true, TraceEvent{}, nil
}

func (transaction *guardedTransaction) finishNonterminal(
	ctx context.Context,
	operationID uint64,
	entry *fileEntry,
	operation *fileTransactionOperation,
	cut MutationCut,
	executeErr error,
	checkpoint transfer.VerifiedDurableRanges,
) error {
	session := transaction.session
	if executeErr == nil && cut != MutationStable {
		cut, executeErr = MutationAmbiguous, ErrExecutorContract
	}
	if executeErr == nil && operation.action == actionCheckpoint && checkpoint.Binding() != transaction.binding {
		cut, executeErr = MutationAmbiguous, ErrExecutorContract
	}
	// A stable cut without its required result cannot authorize replay; treating
	// that contradiction as ambiguous prevents a second executor mutation.
	if executeErr != nil && (!cut.valid() || cut == MutationStable) {
		cut, executeErr = MutationAmbiguous, executorContractError(executeErr)
	}

	session.mu.Lock()
	if session.fileClaims[entry.claim.id] != entry || entry.operation != operation || entry.state != fileActive {
		err := session.markInvariantFailureLocked()
		session.mu.Unlock()
		return err
	}
	entry.operation = nil
	decision := TraceActive
	var value fault.Fault
	if executeErr != nil {
		value, executeErr = session.normalizeFailureLocked(ctx, executeErr, cut)
		decision = TraceRejected
		if cut == MutationAmbiguous {
			entry.uncertain = true
			session.attention = true
			decision = TraceAmbiguous
		}
	} else {
		operation.checkpoint = checkpoint
	}
	operation.err = executeErr
	close(operation.done)
	traceOperation := operationKindForAction(operation.action)
	if executeErr == nil && operation.action == actionWrite && operation.argument != 0 && !entry.firstWrite {
		entry.firstWrite = true
		traceOperation = OperationFirstWrite
	}
	event := session.traceLocked(operationID, traceOperation, decision, entry.claim.id,
		ClaimFile, ClaimActive, ClaimActive, value)
	session.mu.Unlock()
	session.emit(event)
	return executeErr
}

func (transaction *guardedTransaction) terminalOperation(
	ctx context.Context,
	action transactionAction,
	argument uint8,
	execute func() (transfer.FileSettlement, MutationCut, error),
) (transfer.FileSettlement, error) {
	lease, operationID, err := transaction.begin(ctx)
	if err != nil {
		return transfer.FileSettlement{}, err
	}
	defer lease.release()
	entry, operation, owner, event, err := transaction.reserve(operationID, action, argument)
	if err != nil {
		transaction.session.emit(event)
		return transfer.FileSettlement{}, err
	}
	if operation == nil {
		transaction.session.emit(event)
		return entry.settlement, nil
	}
	if !owner {
		transaction.session.emit(event)
		return waitFileTerminal(ctx, lease.closing(), operation)
	}
	settlement, cut, executeErr := execute()
	return transaction.finishTerminal(ctx, operationID, entry, operation, settlement, cut, executeErr)
}

func (transaction *guardedTransaction) finishTerminal(
	ctx context.Context,
	operationID uint64,
	entry *fileEntry,
	operation *fileTransactionOperation,
	settlement transfer.FileSettlement,
	cut MutationCut,
	executeErr error,
) (transfer.FileSettlement, error) {
	if executeErr == nil && (cut != MutationStable || !validTerminalSettlement(operation.action, transaction.binding, settlement)) {
		cut, executeErr = MutationAmbiguous, ErrExecutorContract
	}
	// A stable cut without its required result cannot authorize replay; treating
	// that contradiction as ambiguous prevents a second executor mutation.
	if executeErr != nil && (!cut.valid() || cut == MutationStable) {
		cut, executeErr = MutationAmbiguous, executorContractError(executeErr)
	}
	session := transaction.session
	session.mu.Lock()
	if session.fileClaims[entry.claim.id] != entry || entry.operation != operation || entry.state != fileActive {
		err := session.markInvariantFailureLocked()
		session.mu.Unlock()
		return transfer.FileSettlement{}, err
	}
	entry.operation = nil
	var value fault.Fault
	if executeErr != nil {
		value, executeErr = session.normalizeFailureLocked(ctx, executeErr, cut)
		decision := TraceRejected
		if cut == MutationAmbiguous {
			entry.uncertain = true
			session.attention = true
			decision = TraceAmbiguous
		}
		operation.err = executeErr
		close(operation.done)
		event := session.traceLocked(operationID, operationKindForAction(operation.action), decision,
			entry.claim.id, ClaimFile, ClaimActive, ClaimActive, value)
		session.mu.Unlock()
		session.emit(event)
		return transfer.FileSettlement{}, executeErr
	}

	entry.state = fileSettled
	entry.settlement = settlement
	entry.terminalAction = operation.action
	entry.terminalArgument = operation.argument
	operation.settlement = settlement
	session.activeFiles--
	session.fileSlots--
	if err := session.adjustActiveAncestorsLocked(entry.claim.parent, false); err != nil {
		err = session.markInvariantFailureLocked()
		operation.err = err
		close(operation.done)
		session.mu.Unlock()
		return transfer.FileSettlement{}, err
	}
	close(operation.done)
	decision := TraceSettled
	if operation.action == actionCommit && settlement.Kind() == transfer.FileCollision {
		decision = TraceCollision
	}
	event := session.traceLocked(operationID, operationKindForAction(operation.action), decision,
		entry.claim.id, ClaimFile, ClaimActive, ClaimSettled, fault.Fault{})
	session.mu.Unlock()
	session.emit(event)
	return settlement, nil
}

func validTerminalSettlement(
	action transactionAction,
	binding transfer.MaterializedFileBinding,
	settlement transfer.FileSettlement,
) bool {
	settledBinding, ok := settlement.MaterializedBinding()
	if !ok || settledBinding != binding || settlement.Target() != binding.Target() {
		return false
	}
	switch action {
	case actionCommit:
		return settlement.Kind() == transfer.FilePublished || settlement.Kind() == transfer.FileCollision ||
			settlement.Kind() == transfer.FileItemBlocked
	case actionPause:
		return settlement.Kind() == transfer.FilePaused || settlement.Kind() == transfer.FileItemBlocked ||
			settlement.Kind() == transfer.FileFailed
	case actionRetire:
		return settlement.Kind() == transfer.FileFailed || settlement.Kind() == transfer.FileItemBlocked
	default:
		return false
	}
}

func operationKindForAction(action transactionAction) OperationKind {
	switch action {
	case actionWrite:
		return OperationWriteRange
	case actionCheckpoint:
		return OperationCheckpointFile
	case actionCommit:
		return OperationCommitFile
	case actionPause:
		return OperationPauseFile
	case actionRetire:
		return OperationRetireFile
	default:
		return OperationBeginFile
	}
}

func waitFileTerminal(
	ctx context.Context,
	closing <-chan struct{},
	operation *fileTransactionOperation,
) (transfer.FileSettlement, error) {
	select {
	case <-operation.done:
		return operation.settlement, operation.err
	case <-ctx.Done():
		return transfer.FileSettlement{}, ctx.Err()
	case <-closing:
		return transfer.FileSettlement{}, sessionClosedError()
	}
}

func (transaction *guardedTransaction) pauseForClose(
	ctx context.Context,
	operationID uint64,
	reason transfer.FilePauseReason,
) (transfer.FileSettlement, error) {
	session := transaction.session
	session.mu.Lock()
	entry := session.fileClaims[transaction.claimID]
	if entry == nil || entry.transaction != transaction || entry.state != fileActive || entry.operation != nil {
		err := session.markInvariantFailureLocked()
		session.mu.Unlock()
		return transfer.FileSettlement{}, err
	}
	if entry.uncertain {
		session.attention = true
		session.mu.Unlock()
		return transfer.FileSettlement{}, mutationAmbiguousError(ErrSessionRequiresPause)
	}
	operation := &fileTransactionOperation{
		done: make(chan struct{}), action: actionPause, argument: uint8(reason),
	}
	entry.operation = operation
	session.mu.Unlock()
	settlement, cut, err := transaction.executor.Pause(ctx, reason)
	return transaction.finishTerminal(ctx, operationID, entry, operation, settlement, cut, err)
}
