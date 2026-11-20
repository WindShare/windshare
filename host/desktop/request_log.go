package desktop

import "net/url"

type requestResultLog struct {
	RequestID         string `json:"request_id"`
	Command           string `json:"command"`
	Action            string `json:"action,omitempty"`
	OperationID       string `json:"operation_id,omitempty"`
	SourceOperationID string `json:"source_operation_id,omitempty"`
	TaskID            string `json:"task_id,omitempty"`
	SetupID           string `json:"setup_id,omitempty"`
	PathCount         int    `json:"path_count,omitempty"`
	RelayOrigin       string `json:"relay_origin,omitempty"`
	FrontendOrigin    string `json:"frontend_origin,omitempty"`
	Accepted          bool   `json:"accepted"`
	ErrorCode         string `json:"error_code,omitempty"`
}

// Request acceptance is distinct from the eventual result of an asynchronous operation.
func (a *Application) logRequestResult(request Request, rejection *Failure) {
	fields := requestResultLog{
		RequestID: request.ID, Command: request.Command,
		OperationID: request.OperationID, SourceOperationID: request.SourceOperationID,
		TaskID: request.TaskID, SetupID: request.SetupID, PathCount: len(request.Paths),
		Accepted: rejection == nil,
	}
	if rejection != nil {
		fields.ErrorCode = rejection.Code
	}
	switch request.Command {
	case "account":
		if request.Account != nil {
			fields.Action = request.Account.Action
		}
	case "switch_relay":
		fields.RelayOrigin = endpointOrigin(request.RelayURL)
	case "configure":
		if request.Setup != nil {
			fields.SetupID = request.Setup.ID
			if edit := request.Setup.Preference; edit != nil {
				switch edit.Action {
				case "enable", "disable", "remove":
					fields.Action = edit.Action
					fields.RelayOrigin = endpointOrigin(edit.Value)
				case "frontend":
					fields.Action = edit.Action
					fields.FrontendOrigin = endpointOrigin(edit.Value)
				case "mode":
					fields.Action = edit.Action
				}
			} else {
				fields.RelayOrigin = endpointOrigin(request.Setup.RelayURL)
				fields.FrontendOrigin = endpointOrigin(request.Setup.FrontURL)
			}
		}
	}
	a.deps.log("host.request_result", fields)
}

func (a *Application) logShareState(share Share) {
	// The control snapshot contains the read key and selected files. Support
	// logs only need the lifecycle fields and identities used for correlation.
	fields := map[string]any{
		"operation_id": share.OperationID, "task_id": share.TaskID,
		"state": share.State, "startup": share.Startup, "blocking": share.Blocking,
	}
	if share.Error != nil {
		fields["error_code"] = share.Error.Code
	}
	if share.Result != nil {
		fields["outcome"] = share.Result.Outcome
		fields["stop_reason"] = share.Result.StopReason
	}
	a.deps.log("host.share_state", fields)
}

func (a *Application) logSetupResult(event string) {
	setup := a.setup
	fields := map[string]any{
		"setup_id": setup.ID, "operation_id": setup.OperationID,
		"relay_origin": endpointOrigin(setup.RelayURL), "state": setup.State, "stage": setup.Stage,
		"frontend_saved": setup.FrontendSaved, "relay_saved": setup.RelaySaved, "recovery": setup.Recovery,
		"relay_operation": setup.RelayOperation,
	}
	if setup.Error != nil {
		fields["error_code"] = setup.Error.Code
	}
	a.deps.log(event, fields)
}

func endpointOrigin(raw string) string {
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Hostname() == "" {
		return ""
	}
	switch parsed.Scheme {
	case "http", "https", "ws", "wss":
		// User-supplied URLs can carry credentials outside their origin.
		return parsed.Scheme + "://" + parsed.Host
	default:
		return ""
	}
}
