// Package desktop owns application-lifetime sharing and its authenticated local control endpoint.
package desktop

import (
	"encoding/json"

	"github.com/windshare/windshare/engine/observation/wire"
)

const ProtocolVersion = 1

type Request struct {
	ID          string      `json:"id"`
	Command     string      `json:"command"`
	OperationID string      `json:"operation_id,omitempty"`
	TaskID      string      `json:"task_id,omitempty"`
	Paths       []string    `json:"paths,omitempty"`
	Setup       *SetupInput `json:"setup,omitempty"`
}

type Failure struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

type ShareState string

const (
	AwaitingConfiguration ShareState = "awaiting_configuration"
	Admitting             ShareState = "admitting"
	Preparing             ShareState = "preparing"
	Sharing               ShareState = "sharing"
	Stopping              ShareState = "stopping"
	Finished              ShareState = "finished"
)

type Share struct {
	OperationID string        `json:"operation_id"`
	TaskID      string        `json:"task_id,omitempty"`
	CreatedAt   int64         `json:"created_at_ms"`
	State       ShareState    `json:"state"`
	Startup     string        `json:"startup"`
	URL         string        `json:"url,omitempty"`
	Selection   *Selection    `json:"selection,omitempty"`
	Relay       *wire.Encoded `json:"relay,omitempty"`
	Result      *Completion   `json:"result,omitempty"`
	Error       *Failure      `json:"error,omitempty"`
}

type Selection struct {
	Count uint64        `json:"count"`
	Root  *SelectedRoot `json:"root,omitempty"`
}

type SelectedRoot struct {
	Name     string  `json:"name"`
	Kind     string  `json:"kind"`
	FileSize *uint64 `json:"file_size,omitempty"`
}

type Outcome string

const (
	OutcomeStopped   Outcome = "stopped"
	OutcomeCancelled Outcome = "cancelled"
	OutcomeFailed    Outcome = "failed"
)

type StopReason string

const (
	NoStopReason      StopReason = "none"
	Cancelled         StopReason = "cancelled"
	ShareStopped      StopReason = "share_stopped"
	ApplicationClosed StopReason = "application_closed"
)

type Completion struct {
	Outcome      Outcome    `json:"outcome"`
	StopReason   StopReason `json:"stop_reason"`
	Error        string     `json:"error,omitempty"`
	CleanupError string     `json:"cleanup_error,omitempty"`
}

type Snapshot struct {
	SessionID string    `json:"session_id"`
	Revision  uint64    `json:"revision"`
	Shares    []Share   `json:"shares"`
	Setup     SetupView `json:"setup"`
}

func (s Snapshot) active() bool {
	if s.Setup.State == "running" {
		return true
	}
	for _, share := range s.Shares {
		if share.State != Finished && share.State != AwaitingConfiguration {
			return true
		}
	}
	return false
}

type Message struct {
	Version     int          `json:"version"`
	ID          string       `json:"id,omitempty"`
	Snapshot    *Snapshot    `json:"snapshot,omitempty"`
	Error       *Failure     `json:"error,omitempty"`
	Observation *Observation `json:"observation,omitempty"`
}

type Observation struct {
	OperationID string       `json:"operation_id"`
	TaskID      string       `json:"task_id"`
	Event       wire.Encoded `json:"event"`
}

func failure(code string, err error) *Failure { return &Failure{Code: code, Message: err.Error()} }
func (m Message) bytes() ([]byte, error)      { return json.Marshal(m) }
