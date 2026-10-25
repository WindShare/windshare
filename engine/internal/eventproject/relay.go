package eventproject

import (
	url "net/url"
	strconv "strconv"
	strings "strings"

	relayset "github.com/windshare/windshare/connectivity/relayset"
	senderrelay "github.com/windshare/windshare/connectivity/senderrelay"
	share "github.com/windshare/windshare/engine/internal/share"
	appobs "github.com/windshare/windshare/engine/observation"
	v2 "github.com/windshare/windshare/relay/protocol/v2"
)

func NormalizeRelayAuthority(raw string) (appobs.RelayAuthority, error) {
	endpoint, err := v2.NormalizeRelayEndpoint(raw)
	if err != nil {
		return appobs.RelayAuthority{}, ErrInvalidProjection
	}
	return RelayAuthority(endpoint)
}

func RelayAuthority(endpoint v2.RelayEndpoint) (appobs.RelayAuthority, error) {
	// Re-normalization proves that the exported endpoint struct was not assembled
	// or mutated by a caller. Only authority components survive this boundary.
	dial, err := url.Parse(endpoint.DialURL)
	if err != nil || !strings.HasSuffix(dial.Path, v2.WebSocketPath) {
		return appobs.RelayAuthority{}, ErrInvalidProjection
	}
	base := *dial
	base.Path = strings.TrimSuffix(base.Path, v2.WebSocketPath)
	if base.RawPath != "" {
		if !strings.HasSuffix(base.RawPath, v2.WebSocketPath) {
			return appobs.RelayAuthority{}, ErrInvalidProjection
		}
		base.RawPath = strings.TrimSuffix(base.RawPath, v2.WebSocketPath)
	}
	normalized, err := v2.NormalizeRelayEndpoint(base.String())
	if err != nil || normalized.DialURL != endpoint.DialURL ||
		normalized.IdentityURL != endpoint.IdentityURL || normalized.Identity != endpoint.Identity {
		return appobs.RelayAuthority{}, ErrInvalidProjection
	}
	parsed := dial
	var scheme appobs.RelayScheme
	switch parsed.Scheme {
	case "ws":
		scheme = appobs.RelayWS
	case "wss":
		scheme = appobs.RelayWSS
	default:
		return appobs.RelayAuthority{}, ErrInvalidProjection
	}
	port := parsed.Port()
	if port == "" {
		if scheme == appobs.RelayWS {
			port = "80"
		} else {
			port = "443"
		}
	}
	value, err := strconv.ParseUint(port, 10, 16)
	if err != nil || value == 0 {
		return appobs.RelayAuthority{}, ErrInvalidProjection
	}
	authority, err := appobs.NewRelayAuthority(scheme, parsed.Hostname(), uint16(value))
	if err != nil {
		return appobs.RelayAuthority{}, ErrInvalidProjection
	}
	return authority, nil
}

func projectSenderRecovery(value share.RelayRecovery) (appobs.RelayRecovering, error) {
	authority, err := RelayAuthority(value.Endpoint)
	if err != nil {
		return appobs.RelayRecovering{}, err
	}
	attempt := value.Attempt
	var state appobs.RelayRecoveryState
	switch attempt.State {
	case senderrelay.AttemptStarted:
		state = appobs.RelayRecoveryStarted
	case senderrelay.AttemptSucceeded:
		state = appobs.RelayRecoverySucceeded
	case senderrelay.AttemptFailed:
		state = appobs.RelayRecoveryFailed
	case senderrelay.AttemptWaiting:
		state = appobs.RelayRecoveryWaiting
	default:
		return appobs.RelayRecovering{}, ErrInvalidProjection
	}
	var failure appobs.Failure
	if state == appobs.RelayRecoveryFailed {
		failure, _ = ClassifyError(attempt.Failure)
		if !failure.Valid() {
			failure = mustFailure(appobs.FailureUnexpected)
		}
	}
	instance, _ := appobs.NewSharingInstanceID(attempt.ShareInstance[:])
	return appobs.NewRelayRecoveryObservation(appobs.WorkflowShare, authority, attempt.Number, state, failure,
		appobs.RelayRecoveryDetails{ShareInstance: instance, Generation: attempt.Generation, Slow: attempt.Slow, Resume: attempt.Resume, Terminal: attempt.Terminal, NextDelay: attempt.NextDelay})
}

func projectReceiverRecovery(value relayset.ReceiverRecoveryObservation) (appobs.RelayRecovering, bool, error) {
	authority, err := NormalizeRelayAuthority(value.Endpoint)
	if err != nil {
		return appobs.RelayRecovering{}, true, err
	}
	var state appobs.RelayRecoveryState
	var failure appobs.Failure
	switch value.Phase {
	case relayset.ReceiverRecoveryConnecting:
		state = appobs.RelayRecoveryStarted
	case relayset.ReceiverRecoveryConnected:
		if value.Attempt == 1 {
			return appobs.RelayRecovering{}, false, nil
		}
		state = appobs.RelayRecoverySucceeded
	case relayset.ReceiverRecoveryWaiting:
		state = appobs.RelayRecoveryWaiting
	case relayset.ReceiverRecoveryRetrying, relayset.ReceiverRecoveryTerminal:
		state = appobs.RelayRecoveryFailed
		failure, _ = ClassifyError(value.Err)
		if !failure.Valid() {
			failure = mustFailure(appobs.FailureRelayTransport)
		}
	default:
		return appobs.RelayRecovering{}, false, nil
	}
	details := appobs.RelayRecoveryDetails{Generation: value.ConnectionGeneration, Slow: value.Phase == relayset.ReceiverRecoveryWaiting, Terminal: value.Phase == relayset.ReceiverRecoveryTerminal, NextDelay: value.Delay}
	if value.ProtocolSessionID != ([16]byte{}) {
		details.ProtocolSessionID, _ = appobs.NewProtocolSessionID(value.ProtocolSessionID[:])
	}
	event, err := appobs.NewRelayRecoveryObservation(appobs.WorkflowReceive, authority, value.Attempt, state, failure, details)
	return event, true, err
}
