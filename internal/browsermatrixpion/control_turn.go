package browsermatrixpion

import (
	"bytes"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strconv"
	"time"
	"unicode/utf8"
)

// ControlTURNCredentialLease transfers one provider-owned TURN capability into
// the same request scope as its control lease. Credential remains byte-owned by
// the parent process and is excluded from every representation.
type ControlTURNCredentialLease struct {
	RequestID         string `json:"requestId"`
	RunID             string `json:"runId"`
	ProfileID         string `json:"profileId"`
	ProbeNonce        string `json:"probeNonce"`
	ControlLeaseID    string `json:"controlLeaseId"`
	AttestationSHA256 string `json:"attestationSha256"`
	CredentialID      string `json:"credentialId"`
	Username          string `json:"username"`
	ExpiresAt         string `json:"expiresAt"`
	MaxAttempts       int    `json:"maxAttempts"`
	Credential        []byte `json:"-"`
}

type ControlTURNDeclaration struct {
	CredentialID string `json:"credentialId"`
	Username     string `json:"username"`
	ExpiresAt    string `json:"expiresAt"`
}

func cloneControlTURNDeclaration(value *ControlTURNDeclaration) *ControlTURNDeclaration {
	if value == nil {
		return nil
	}
	cloned := *value
	return &cloned
}

func (lease ControlTURNCredentialLease) String() string {
	return fmt.Sprintf(
		"control-turn-lease(request=%s,control=%s,run=%s,profile=%s,attestation=%s,credential-id=%s,expires=%s)",
		lease.RequestID,
		lease.ControlLeaseID,
		lease.RunID,
		lease.ProfileID,
		lease.AttestationSHA256,
		lease.CredentialID,
		lease.ExpiresAt,
	)
}

func (lease ControlTURNCredentialLease) GoString() string { return lease.String() }

type controlTURNCredentialLease struct {
	metadata   ControlTURNCredentialLease
	credential []byte
	expiresAt  time.Time
	delivery   controlTURNDeliveryState
}

type controlTURNDeliveryState uint8

const (
	controlTURNDeliveryAvailable controlTURNDeliveryState = iota + 1
	controlTURNDeliveryInProgress
	controlTURNDeliveryTerminal
)

func (service *Service) BindControlTURNCredential(lease ControlTURNCredentialLease) error {
	if !validControlTURNCredentialBytes(lease.Credential) {
		return errors.New("control TURN credential lease is invalid")
	}
	expiresAt, err := parseCanonicalTimestamp(lease.ExpiresAt)
	if err != nil {
		return errors.New("control TURN credential lease is invalid")
	}

	service.mu.Lock()
	defer service.mu.Unlock()
	if service.closed || service.profileID != "scheduled-coturn" {
		return errors.New("control TURN credential capability is unavailable")
	}
	controlLease, active := service.controlCredentials.activeLeaseMetadata(lease.ControlLeaseID)
	declaration := controlLease.TURN
	if !active || lease.RequestID != controlLease.RequestID || lease.RunID != controlLease.RunID ||
		lease.ProfileID != controlLease.ProfileID || lease.ProbeNonce != controlLease.ProbeNonce ||
		lease.AttestationSHA256 != controlLease.AttestationSHA256 || lease.MaxAttempts != 1 ||
		declaration == nil || lease.CredentialID != declaration.CredentialID ||
		lease.Username != declaration.Username || lease.ExpiresAt != declaration.ExpiresAt {
		return errors.New("control TURN credential scope is invalid")
	}
	controlExpiresAt, parseErr := parseCanonicalTimestamp(controlLease.ExpiresAt)
	if parseErr != nil || !expiresAt.Equal(controlExpiresAt) || !service.clock.Now().UTC().Before(expiresAt) {
		return errors.New("control TURN credential lifetime is invalid")
	}
	if existing := service.controlTURNLeases[lease.ControlLeaseID]; existing != nil {
		if existing.metadata.RequestID != lease.RequestID ||
			existing.metadata.AttestationSHA256 != lease.AttestationSHA256 ||
			!existing.expiresAt.Equal(expiresAt) ||
			len(existing.credential) != len(lease.Credential) ||
			subtle.ConstantTimeCompare(existing.credential, lease.Credential) != 1 {
			return errors.New("control TURN credential replay changed its authority")
		}
		return nil
	}
	metadata := lease
	metadata.Credential = nil
	service.controlTURNLeases[lease.ControlLeaseID] = &controlTURNCredentialLease{
		metadata: metadata, credential: append([]byte(nil), lease.Credential...), expiresAt: expiresAt,
		delivery: controlTURNDeliveryAvailable,
	}
	return nil
}

func (service *Service) retireControlTURNCredentialLocked(controlLeaseID string) {
	lease := service.controlTURNLeases[controlLeaseID]
	if lease == nil {
		return
	}
	eraseCredentialBytes(lease.credential)
	delete(service.controlTURNLeases, controlLeaseID)
}

func (service *Service) beginControlTURNCredentialDeliveryLocked(
	controlLeaseID string,
	attestationSHA256 string,
	now time.Time,
) (ControlTURNCredentialLease, []byte, bool) {
	lease := service.controlTURNLeases[controlLeaseID]
	if lease == nil || lease.metadata.AttestationSHA256 != attestationSHA256 ||
		!now.Before(lease.expiresAt) || lease.delivery != controlTURNDeliveryAvailable {
		return ControlTURNCredentialLease{}, nil, false
	}
	lease.delivery = controlTURNDeliveryInProgress
	credential := lease.credential
	lease.credential = nil
	return lease.metadata, credential, true
}

func (service *Service) finishControlTURNCredentialDeliveryLocked(controlLeaseID string) {
	lease := service.controlTURNLeases[controlLeaseID]
	if lease == nil || lease.delivery != controlTURNDeliveryInProgress {
		panic("control TURN delivery lost its exact registry owner")
	}
	lease.delivery = controlTURNDeliveryTerminal
}

func validControlTURNCredentialBytes(value []byte) bool {
	return len(value) != 0 && len(value) <= 512 && utf8.Valid(value) &&
		!containsAnyByte(value, 0, '\r', '\n')
}

func containsAnyByte(value []byte, candidates ...byte) bool {
	for _, current := range value {
		if slices.Contains(candidates, current) {
			return true
		}
	}
	return false
}

func writeControlTURNCredentialResponse(
	writer http.ResponseWriter,
	binding AttemptBinding,
	lease ControlTURNCredentialLease,
	credential []byte,
) error {
	prefix, err := canonicalJSONLine(struct {
		ProtocolVersion string `json:"protocolVersion"`
		AttemptBinding
		CredentialID string `json:"credentialId"`
		ExpiresAt    string `json:"expiresAt"`
		Username     string `json:"username"`
	}{
		ProtocolVersion: ProtocolVersion, AttemptBinding: binding,
		CredentialID: lease.CredentialID, ExpiresAt: lease.ExpiresAt, Username: lease.Username,
	})
	if err != nil || len(prefix) < 2 || prefix[len(prefix)-2] != '}' {
		writeProtocolError(writer, http.StatusInternalServerError, "turn-credential-encoding-failed")
		return errors.New("control TURN response metadata encoding failed")
	}
	var encodedCredential bytes.Buffer
	appendJSONStringBytes(&encodedCredential, credential)
	document := make([]byte, 0, len(prefix)+encodedCredential.Len()+32)
	document = append(document, prefix[:len(prefix)-2]...)
	document = append(document, `,"credential":`...)
	document = append(document, encodedCredential.Bytes()...)
	document = append(document, '}', '\n')
	if !json.Valid(document) {
		eraseCredentialBytes(encodedCredential.Bytes())
		eraseCredentialBytes(document)
		writeProtocolError(writer, http.StatusInternalServerError, "turn-credential-encoding-failed")
		return errors.New("control TURN response document encoding failed")
	}
	writer.Header().Set("Content-Type", "application/json")
	writer.Header().Set("Content-Length", strconv.Itoa(len(document)))
	writer.WriteHeader(http.StatusOK)
	written, writeErr := writer.Write(document)
	eraseCredentialBytes(encodedCredential.Bytes())
	eraseCredentialBytes(document)
	if writeErr != nil || written != len(document) {
		return errors.New("control TURN response delivery was ambiguous")
	}
	return nil
}

func appendJSONStringBytes(destination *bytes.Buffer, value []byte) {
	destination.WriteByte('"')
	for _, current := range value {
		switch current {
		case '"', '\\':
			destination.WriteByte('\\')
			destination.WriteByte(current)
		case '\b':
			destination.WriteString(`\b`)
		case '\f':
			destination.WriteString(`\f`)
		case '\t':
			destination.WriteString(`\t`)
		default:
			if current < 0x20 {
				const hexadecimal = "0123456789abcdef"
				destination.WriteString(`\u00`)
				destination.WriteByte(hexadecimal[current>>4])
				destination.WriteByte(hexadecimal[current&0x0f])
			} else {
				destination.WriteByte(current)
			}
		}
	}
	destination.WriteByte('"')
}
