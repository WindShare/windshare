package desktop

import (
	"errors"
	"slices"
)

// A repeat owns a new identity and eventual engine task. The source remains a
// retained terminal result; replaying either command cannot resurrect that task.
func (a *Application) repeat(request Request) error {
	source, ok := a.operations[request.SourceOperationID]
	if !ok {
		return errors.New("source creation operation not found")
	}
	view := projectShare(source)
	if view.State != Finished {
		return errors.New("stop the existing share before sharing again")
	}
	op, err := a.registerOperation(request.OperationID, source.paths, source.id)
	if err != nil || op == nil {
		return err
	}
	op.relays = slices.Clone(source.relays)
	if view.Authorization != nil {
		op.phase = awaitingAuthorization{authority: *view.Authorization}
	} else {
		a.admit(op)
	}
	a.deps.log("host.share_repeated", map[string]string{"operation_id": op.id, "source_operation_id": source.id})
	return nil
}
