// Package desktop owns application-lifetime sharing and its authenticated local control endpoint.
package desktop

import (
	"encoding/json"

	"github.com/windshare/windshare/engine/observation/wire"
)

const ProtocolVersion = 1

type Request struct {
	ID          string   `json:"id"`
	Command     string   `json:"command"`
	OperationID string   `json:"operation_id,omitempty"`
	TaskID      string   `json:"task_id,omitempty"`
	Paths       []string `json:"paths,omitempty"`
}

type Failure struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

type Share struct {
	OperationID   string        `json:"operation_id"`
	TaskID        string        `json:"task_id,omitempty"`
	State         string        `json:"state"`
	Startup       string        `json:"startup"`
	URL           string        `json:"url,omitempty"`
	SelectedCount uint64        `json:"selected_count,omitempty"`
	Relay         *wire.Encoded `json:"relay,omitempty"`
	Result        *Completion   `json:"result,omitempty"`
	Error         *Failure      `json:"error,omitempty"`
}

type Completion struct {
	Outcome      string `json:"outcome"`
	StopReason   string `json:"stop_reason"`
	Error        string `json:"error,omitempty"`
	CleanupError string `json:"cleanup_error,omitempty"`
}

type Snapshot struct {
	SessionID string  `json:"session_id"`
	Revision  uint64  `json:"revision"`
	Shares    []Share `json:"shares"`
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

func failure(code string, err error) *Failure {
	return &Failure{Code: code, Message: err.Error()}
}

func (m Message) bytes() ([]byte, error) { return json.Marshal(m) }
