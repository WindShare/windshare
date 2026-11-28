package serviceauthority

import (
	"bytes"
	"encoding/json"
)

// Each value represents a complete signed public-admission decision.
type PublicRelay uint8

const (
	RelayUnknown PublicRelay = iota
	RelayAnonymous
	RelayAuthenticated
	RelayBudgetExhausted
	RelayUnavailable
	RelayDisabled
)

func (state PublicRelay) MarshalJSON() ([]byte, error) {
	switch state {
	case RelayAnonymous:
		return []byte(`{"state":"enabled","anonymous":true}`), nil
	case RelayAuthenticated:
		return []byte(`{"state":"enabled","anonymous":false}`), nil
	case RelayBudgetExhausted:
		return []byte(`{"state":"budget_exhausted"}`), nil
	case RelayUnavailable:
		return []byte(`{"state":"unavailable"}`), nil
	case RelayDisabled:
		return []byte(`{"state":"disabled"}`), nil
	default:
		return nil, ErrFormat
	}
}

func (state *PublicRelay) UnmarshalJSON(data []byte) error {
	var raw struct {
		State     string          `json:"state"`
		Anonymous json.RawMessage `json:"anonymous"`
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if dec.Decode(&raw) != nil {
		return ErrFormat
	}
	if raw.State == "enabled" {
		switch string(raw.Anonymous) {
		case "true":
			*state = RelayAnonymous
		case "false":
			*state = RelayAuthenticated
		default:
			return ErrFormat
		}
		return nil
	}
	if raw.Anonymous != nil {
		return ErrFormat
	}
	switch raw.State {
	case "budget_exhausted":
		*state = RelayBudgetExhausted
	case "unavailable":
		*state = RelayUnavailable
	case "disabled":
		*state = RelayDisabled
	default:
		return ErrFormat
	}
	return nil
}
