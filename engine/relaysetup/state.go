package relaysetup

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/url"
	"strings"
	"unicode"

	"github.com/windshare/windshare/connectivity/senderrelay"
	"github.com/windshare/windshare/engine/relayconfig"
)

const (
	claimSecretBytes        = 32
	operationIDBytes        = 16
	maximumDeviceNameBytes  = 128
	minimumPollIntervalMS   = 1000
	maximumPollIntervalMS   = 30_000
	maximumCheckpointBytes  = 16 * 1024
	maximumOperationIDBytes = 128
)

type setupIntent struct {
	SettingsRevision string    `json:"settings_revision"`
	OperationID      string    `json:"operation_id"`
	RelayURL         string    `json:"relay_url"`
	ConsoleURL       string    `json:"console_url"`
	FrontURL         string    `json:"front_url,omitempty"`
	DeviceName       string    `json:"device_name"`
	ClaimSecret      string    `json:"claim_secret"`
	Operation        Operation `json:"operation"`
}

type enrollment struct {
	ID              string `json:"id"`
	UserCode        string `json:"user_code"`
	VerificationURL string `json:"verification_url"`
	ExpiresAt       int64  `json:"expires_at"`
	IntervalMS      int64  `json:"interval_ms"`
}
type credential struct {
	AccessKey string `json:"access_key"`
	ID        string `json:"id"`
}

// The private sum type prevents a caller from combining a cancellation with an
// installable credential or constructing an installation without its approval.
type setupPhase interface{ stage() string }
type requesting struct{}
type awaiting struct {
	Enrollment enrollment `json:"enrollment"`
}
type installing struct {
	Enrollment enrollment `json:"enrollment"`
	Credential credential `json:"credential"`
}
type cancelling struct {
	EnrollmentID string `json:"enrollment_id,omitempty"`
}

func (requesting) stage() string { return "requesting" }
func (awaiting) stage() string   { return "awaiting" }
func (installing) stage() string { return "installing" }
func (cancelling) stage() string { return "cancelling" }

type PendingSetup struct {
	setupIntent
	phase setupPhase
}

func (pending *PendingSetup) enrollment() enrollment {
	switch phase := pending.phase.(type) {
	case awaiting:
		return phase.Enrollment
	case installing:
		return phase.Enrollment
	case cancelling:
		return enrollment{ID: phase.EnrollmentID}
	default:
		return enrollment{}
	}
}
func (pending *PendingSetup) credential() credential {
	if phase, ok := pending.phase.(installing); ok {
		return phase.Credential
	}
	return credential{}
}
func (pending *PendingSetup) withPhase(phase setupPhase) *PendingSetup {
	next := *pending
	next.phase = phase
	return &next
}

func (pending *PendingSetup) validate() error {
	invalid := errors.New("invalid relay setup checkpoint")
	secret, err := base64.RawURLEncoding.DecodeString(pending.ClaimSecret)
	if err != nil || len(secret) != claimSecretBytes || pending.SettingsRevision == "" || pending.OperationID == "" ||
		len(pending.OperationID) > maximumOperationIDBytes || strings.ContainsFunc(pending.OperationID, func(c rune) bool {
		return (c < 'a' || c > 'z') && (c < 'A' || c > 'Z') && (c < '0' || c > '9') && c != '-' && c != '_'
	}) ||
		(pending.Operation != AddProfile && pending.Operation != PrivateOnboarding) ||
		strings.TrimSpace(pending.DeviceName) == "" || len(pending.DeviceName) > maximumDeviceNameBytes ||
		strings.ContainsFunc(pending.DeviceName, unicode.IsControl) {
		return invalid
	}
	if relayconfig.ValidateFrontURL(pending.ConsoleURL) != nil ||
		pending.FrontURL != "" && relayconfig.ValidateFrontURL(pending.FrontURL) != nil {
		return invalid
	}
	console, _ := url.Parse(pending.ConsoleURL)
	if console.Scheme != "https" && console.Hostname() != "localhost" && !net.ParseIP(console.Hostname()).IsLoopback() {
		return invalid
	}
	candidate := senderrelay.Candidate{PublicURL: pending.RelayURL, AccessKey: pending.credential().AccessKey}
	if candidate.Validate() != nil {
		return invalid
	}
	switch phase := pending.phase.(type) {
	case requesting:
		return nil
	case cancelling:
		return nil
	case installing:
		if phase.Credential.ID == "" || phase.Credential.AccessKey == "" {
			return invalid
		}
	case awaiting:
	default:
		return invalid
	}
	approval := pending.enrollment()
	verification, err := url.Parse(approval.VerificationURL)
	if err != nil || verification.User != nil || verification.Scheme != console.Scheme || verification.Host != console.Host ||
		approval.ID == "" || approval.UserCode == "" || approval.ExpiresAt <= 0 ||
		approval.IntervalMS < minimumPollIntervalMS || approval.IntervalMS > maximumPollIntervalMS {
		return invalid
	}
	return nil
}

type checkpointDocument struct {
	Intent setupIntent     `json:"intent"`
	Stage  string          `json:"stage"`
	Data   json.RawMessage `json:"data"`
}

func (pending *PendingSetup) MarshalJSON() ([]byte, error) {
	if err := pending.validate(); err != nil {
		return nil, err
	}
	data, err := json.Marshal(pending.phase)
	if err != nil {
		return nil, err
	}
	encoded, err := json.Marshal(checkpointDocument{Intent: pending.setupIntent, Stage: pending.phase.stage(), Data: data})
	if err == nil && len(encoded) > maximumCheckpointBytes {
		return nil, errors.New("setup checkpoint too large")
	}
	return encoded, err
}
func strictDecode(data []byte, value any) error {
	if bytes.Equal(bytes.TrimSpace(data), []byte("null")) {
		return errors.New("checkpoint object is null")
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(value); err != nil {
		return err
	}
	var trailing any
	if decoder.Decode(&trailing) != io.EOF {
		return errors.New("trailing checkpoint data")
	}
	return nil
}
func (pending *PendingSetup) UnmarshalJSON(data []byte) error {
	if len(data) > maximumCheckpointBytes {
		return errors.New("setup checkpoint too large")
	}
	var disk checkpointDocument
	if err := strictDecode(data, &disk); err != nil {
		return err
	}
	pending.setupIntent = disk.Intent
	switch disk.Stage {
	case "requesting":
		var value requesting
		if err := strictDecode(disk.Data, &value); err != nil {
			return err
		}
		pending.phase = value
	case "awaiting":
		var value awaiting
		if err := strictDecode(disk.Data, &value); err != nil {
			return err
		}
		pending.phase = value
	case "installing":
		var value installing
		if err := strictDecode(disk.Data, &value); err != nil {
			return err
		}
		pending.phase = value
	case "cancelling":
		var value cancelling
		if err := strictDecode(disk.Data, &value); err != nil {
			return err
		}
		pending.phase = value
	default:
		return errors.New("unknown setup checkpoint stage")
	}
	return pending.validate()
}
