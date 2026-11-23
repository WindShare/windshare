package iceconfig

import (
	"slices"

	p "github.com/windshare/windshare/core/session/protocolsession"
)

// TURNRelay cannot be passed to a direct attempt, which owns leased host sockets.
type TURNRelay struct {
	urls               []string
	username, password string
}

func NewTURNRelay(urls []string, username, password string) (TURNRelay, error) {
	if len(urls) == 0 || len(urls) > MaximumURLs || !p.ValidTURNCredential(username) || !p.ValidTURNCredential(password) {
		return TURNRelay{}, ErrConfiguration
	}
	for _, u := range urls {
		if !p.ValidTURNURL(u) {
			return TURNRelay{}, ErrConfiguration
		}
	}
	return TURNRelay{slices.Clone(urls), username, password}, nil
}
func (c TURNRelay) Credentials() ([]string, string, string) {
	return slices.Clone(c.urls), c.username, c.password
}
