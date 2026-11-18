package relaysetup

import (
	"encoding/base64"
	"errors"
	"net/url"
	"strings"
)

const maximumInvitationBytes = 4096

// ServiceLogin is a browser destination derived from discovery. The enrollment
// response supplies only the opaque invitation, never a replacement origin.
type ServiceLogin struct {
	URL  string `json:"url"`
	Host string `json:"host"`
}

func (pending *PendingSetup) serviceLogin(invitation string) (*ServiceLogin, error) {
	if invitation == "" || pending.AccountLoginPage == "" {
		return nil, nil
	}
	if len(invitation) > maximumInvitationBytes || strings.ContainsAny(invitation, "\r\n") || !secureEndpoint(pending.AccountLoginPage) {
		return nil, errors.New("invalid service account invitation")
	}
	if _, err := base64.RawURLEncoding.Strict().DecodeString(invitation); err != nil {
		return nil, errors.New("invalid service account invitation")
	}
	page, err := url.Parse(pending.AccountLoginPage)
	if err != nil {
		return nil, err
	}
	page.Fragment = "invite=" + invitation
	return &ServiceLogin{URL: page.String(), Host: page.Host}, nil
}
