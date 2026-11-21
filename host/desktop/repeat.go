package desktop

import (
	"errors"

	"github.com/windshare/windshare/engine"
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
	op, err := a.registerOperation(request.OperationID, source.paths, source.explicitRelays, source.id)
	if err != nil || op == nil {
		return err
	}
	switch {
	case view.Authorization == nil:
		a.admit(op)
	case view.Authorization.Kind == engine.ShareOfficialAuthorization:
		rejectedGrant := a.officialFailureGrantLocked(source)
		currentGrant := a.currentOfficialGrantLocked()
		if currentGrant.ready && currentGrant != rejectedGrant {
			a.deps.log("host.share_official_authorization_resumed", map[string]string{
				"operation_id":        op.id,
				"source_operation_id": source.id,
				"trigger":             "account_already_updated",
			})
			a.admit(op)
		} else {
			op.phase = awaitingAuthorization{authority: *view.Authorization, rejectedGrant: currentGrant}
		}
	default:
		op.phase = awaitingAuthorization{authority: *view.Authorization}
	}
	a.deps.log("host.share_repeated", map[string]string{"operation_id": op.id, "source_operation_id": source.id})
	return nil
}
