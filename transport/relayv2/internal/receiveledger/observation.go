package receiveledger

import v2 "github.com/windshare/windshare/relay/protocol/v2"

// Observer belongs to the embedding transport's diagnostic pipeline. Callbacks
// are synchronous and must not reenter the ledger; no payloads are retained.
type Observer interface {
	ObserveReceive(Event)
}

type Action string

const (
	ActionOpen       Action = "open"
	ActionReceive    Action = "receive"
	ActionConsume    Action = "consume"
	ActionDiscard    Action = "discard"
	ActionRetire     Action = "retire"
	ActionNotice     Action = "notice"
	ActionFeedback   Action = "feedback"
	ActionSettlement Action = "settlement"
	ActionForget     Action = "forget"
	ActionClose      Action = "close"
)

type Event struct {
	ConnectionID uint64
	SessionID    v2.RelaySessionID
	Action       Action
	Progress     Progress
	Usage        Usage
	Failure      error
}

func (l *Ledger) trace(action Action, f *flow, failure error) {
	if l.config.Observer == nil {
		return
	}
	event := Event{ConnectionID: l.config.ConnectionID, Action: action, Usage: l.usage, Failure: failure}
	if f != nil {
		event.SessionID = f.id
		event.Progress = f.Progress
	}
	l.config.Observer.ObserveReceive(event)
}

func (l *Ledger) reject(action Action, f *flow, failure error) error {
	l.trace(action, f, failure)
	return failure
}
