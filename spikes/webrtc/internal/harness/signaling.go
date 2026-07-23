package harness

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
)

const (
	maximumHarnessSignalBytes = 64 * 1024
	signalType                = "signal"
	signalKindOffer           = "offer"
	signalKindAnswer          = "answer"
	signalKindCandidate       = "candidate"
	harnessSessionIDBytes     = 8
)

type signalMessage struct {
	Type      string          `json:"type"`
	SessionID string          `json:"sessionId"`
	Kind      string          `json:"kind"`
	Payload   json.RawMessage `json:"payload"`
}

func encodeSignal(sessionID, kind string, payload json.RawMessage) ([]byte, error) {
	message := signalMessage{Type: signalType, SessionID: sessionID, Kind: kind, Payload: payload}
	if err := validateSignal(message); err != nil {
		return nil, err
	}
	encoded, err := json.Marshal(message)
	if err != nil {
		return nil, fmt.Errorf("encode harness signal: %w", err)
	}
	if len(encoded) > maximumHarnessSignalBytes {
		return nil, errors.New("harness signal exceeds its byte limit")
	}
	return encoded, nil
}

func decodeSignal(encoded []byte) (signalMessage, error) {
	if len(encoded) == 0 || len(encoded) > maximumHarnessSignalBytes {
		return signalMessage{}, errors.New("harness signal has an invalid byte length")
	}
	decoder := json.NewDecoder(bytes.NewReader(encoded))
	decoder.DisallowUnknownFields()
	var message signalMessage
	if err := decoder.Decode(&message); err != nil {
		return signalMessage{}, fmt.Errorf("decode harness signal: %w", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return signalMessage{}, errors.New("harness signal contains trailing JSON")
	}
	if err := validateSignal(message); err != nil {
		return signalMessage{}, err
	}
	return message, nil
}

func validateSignal(message signalMessage) error {
	if message.Type != signalType {
		return errors.New("harness signal has an invalid type")
	}
	if _, err := parseHarnessSessionID(message.SessionID); err != nil {
		return err
	}
	switch message.Kind {
	case signalKindOffer, signalKindAnswer, signalKindCandidate:
	default:
		return errors.New("harness signal has an invalid kind")
	}
	if len(message.Payload) == 0 || !json.Valid(message.Payload) {
		return errors.New("harness signal payload is not valid JSON")
	}
	return nil
}

func parseHarnessSessionID(encoded string) ([harnessSessionIDBytes]byte, error) {
	var identity [harnessSessionIDBytes]byte
	decoded, err := base64.RawURLEncoding.DecodeString(encoded)
	if err != nil || len(decoded) != len(identity) {
		return identity, errors.New("harness session ID is invalid")
	}
	copy(identity[:], decoded)
	return identity, nil
}
