// Package iceconfig describes immutable ICE discovery inputs for a gathering generation.
package iceconfig

import (
	"errors"
	"math"
	"slices"

	"github.com/pion/stun/v4"
)

const (
	MaximumURLs     = 8
	MaximumURLBytes = 512
)

var ErrConfiguration = errors.New("invalid ICE connection configuration")

// STUNServer is a UDP discovery endpoint with its default port already applied.
type STUNServer struct {
	Host string
	Port uint16
}

// Connection is a snapshot consumed by one gathering generation.
type Connection struct {
	discovery []STUNServer
}

func NewDirect(discovery []string) (Connection, error) {
	if len(discovery) > MaximumURLs {
		return Connection{}, ErrConfiguration
	}
	servers := make([]STUNServer, len(discovery))
	for i, raw := range discovery {
		if len(raw) > MaximumURLBytes {
			return Connection{}, ErrConfiguration
		}
		uri, err := stun.ParseURI(raw)
		if err != nil || uri.Scheme != stun.SchemeTypeSTUN || uri.Port < 1 || uri.Port > math.MaxUint16 {
			return Connection{}, ErrConfiguration
		}
		servers[i] = STUNServer{Host: uri.Host, Port: uint16(uri.Port)}
	}
	return Connection{discovery: servers}, nil
}

func (c Connection) DiscoveryServers() []STUNServer { return slices.Clone(c.discovery) }
