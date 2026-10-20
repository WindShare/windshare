package relayconfig

import (
	"encoding/base64"
	"net"
	"net/url"
	"strings"
	"unicode"

	"github.com/windshare/windshare/connectivity/senderrelay"
)

const (
	claimSecretBytes       = 32
	maximumDeviceNameBytes = 128
	minimumPollIntervalMS  = 1000
	maximumPollIntervalMS  = 30_000
)

type SetupState string

const (
	SetupEnrolling  SetupState = "enrolling"
	SetupInstalling SetupState = "installing"
	SetupCancelling SetupState = "cancelling"
)

func (pending *PendingSetup) valid() bool {
	switch pending.State {
	case SetupEnrolling:
		if pending.CredentialID != "" {
			return false
		}
	case SetupInstalling:
		if pending.CredentialID == "" {
			return false
		}
	case SetupCancelling:
	default:
		return false
	}
	if pending.SettingsRevision == "" {
		return false
	}
	secret, err := base64.RawURLEncoding.DecodeString(pending.ClaimSecret)
	if err != nil || len(secret) != claimSecretBytes || pending.OperationID == "" ||
		(pending.Operation != "add" && pending.Operation != "private-onboarding") ||
		strings.TrimSpace(pending.DeviceName) == "" || len(pending.DeviceName) > maximumDeviceNameBytes ||
		strings.ContainsFunc(pending.DeviceName, unicode.IsControl) {
		return false
	}
	if ValidateFrontURL(pending.ConsoleURL) != nil ||
		(pending.FrontURL != "" && ValidateFrontURL(pending.FrontURL) != nil) {
		return false
	}
	console, _ := url.Parse(pending.ConsoleURL)
	if console.Scheme != "https" && console.Hostname() != "localhost" && !net.ParseIP(console.Hostname()).IsLoopback() {
		return false
	}
	candidate := senderrelay.Candidate{PublicURL: pending.RelayURL, AccessKey: pending.AccessKey}
	if candidate.Validate() != nil || (pending.CredentialID == "") != (pending.AccessKey == "") {
		return false
	}
	if pending.EnrollmentID == "" {
		return pending.VerificationURL == "" && pending.UserCode == "" && pending.ExpiresAt == 0 &&
			pending.IntervalMS == 0 && pending.CredentialID == ""
	}
	verification, err := url.Parse(pending.VerificationURL)
	return err == nil && verification.User == nil && verification.Scheme == console.Scheme &&
		verification.Host == console.Host && pending.UserCode != "" && pending.ExpiresAt > 0 &&
		pending.IntervalMS >= minimumPollIntervalMS && pending.IntervalMS <= maximumPollIntervalMS
}
