package networkprobe

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
)

const (
	// Cap fault body reads so intermediary HTML error pages cannot bloat memory
	// or logs, while still draining legitimate authority Fault JSON responses.
	maxAuthorityFaultBytes = 4 << 10
	maxFaultTokenBytes     = 64
)

// AuthorityError records a non-200 HTTP response from the network authority,
// including any validated fault kind and reason parsed from its JSON body.
type AuthorityError struct {
	Action     string
	StatusCode int
	Kind       string
	Reason     string
}

func (e *AuthorityError) Error() string {
	switch {
	case e.Kind != "" && e.Reason != "":
		return fmt.Sprintf("authority %s: HTTP %d (%s: %s)", e.Action, e.StatusCode, e.Kind, e.Reason)
	case e.Kind != "":
		return fmt.Sprintf("authority %s: HTTP %d (%s)", e.Action, e.StatusCode, e.Kind)
	default:
		return fmt.Sprintf("authority %s: HTTP %d", e.Action, e.StatusCode)
	}
}

// ReadAuthorityError builds a structured diagnostic error from a non-200
// authority HTTP response. Non-JSON, oversized, or non-token payloads fall
// back to the bare HTTP status so intermediary pages cannot pollute logs.
func ReadAuthorityError(action string, res *http.Response) *AuthorityError {
	out := &AuthorityError{Action: action}
	if res == nil {
		return out
	}
	out.StatusCode = res.StatusCode
	if res.Body == nil {
		return out
	}
	data, err := io.ReadAll(io.LimitReader(res.Body, maxAuthorityFaultBytes+1))
	if err != nil || len(data) == 0 || len(data) > maxAuthorityFaultBytes {
		return out
	}
	out.Kind, out.Reason = parseAuthorityFault(data)
	return out
}

func parseAuthorityFault(data []byte) (string, string) {
	var payload struct {
		Kind   string `json:"kind"`
		Reason string `json:"reason"`
	}
	if err := json.Unmarshal(data, &payload); err != nil {
		return "", ""
	}
	if !validFaultToken(payload.Kind) {
		return "", ""
	}
	if payload.Reason != "" && !validFaultToken(payload.Reason) {
		return "", ""
	}
	return payload.Kind, payload.Reason
}

func validFaultToken(token string) bool {
	if len(token) == 0 || len(token) > maxFaultTokenBytes {
		return false
	}
	for i := range len(token) {
		c := token[i]
		isLower := c >= 'a' && c <= 'z'
		isUpper := c >= 'A' && c <= 'Z'
		isDigit := c >= '0' && c <= '9'
		if !isLower && !isUpper && !isDigit && c != '_' && c != '-' {
			return false
		}
	}
	return true
}
