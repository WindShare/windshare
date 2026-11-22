// Package iceconfig describes immutable ICE discovery inputs for a gathering generation.
package iceconfig

import (
	"errors"
	"slices"

	"github.com/pion/stun/v4"
)

const (
	MaximumURLs     = 8
	MaximumURLBytes = 512
)

var ErrConfiguration = errors.New("invalid ICE connection configuration")

// Connection is a snapshot consumed by one gathering generation.
type Connection struct {
	discovery []string
}

func New(discovery []string) (Connection, error) {
	if len(discovery) > MaximumURLs {
		return Connection{}, ErrConfiguration
	}
	for _, raw := range discovery {
		uri, err := stun.ParseURI(raw)
		if len(raw) > MaximumURLBytes || err != nil || uri.Scheme != stun.SchemeTypeSTUN {
			return Connection{}, ErrConfiguration
		}
	}
	return Connection{discovery: slices.Clone(discovery)}, nil
}

func (c Connection) DiscoveryURLs() []string { return slices.Clone(c.discovery) }
