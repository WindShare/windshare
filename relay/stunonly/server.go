// Package stunonly implements independent, bounded UDP STUN binding listeners.
// It deliberately has no TURN allocation or application-relay dependencies.
package stunonly

import (
	"context"
	"errors"
	"net"
	"sync/atomic"
	"time"

	"github.com/pion/stun/v3"
)

const (
	MaximumDatagramBytes           = 1200
	DefaultRequestsPerSecond       = 1000
	DefaultSourceRequestsPerSecond = 20
	DefaultMaximumSources          = 4096
	sourceWindow                   = time.Second
)

type Config struct {
	RequestsPerSecond       int
	SourceRequestsPerSecond int
	MaximumSources          int
	Now                     func() time.Time
}
type Metrics struct {
	Received, Responded, Invalid, Limited, WriteErrors uint64
	Healthy                                            bool
}
type Server struct {
	conn                                               net.PacketConn
	config                                             Config
	limiter                                            *rateLimiter
	healthy                                            atomic.Bool
	received, responded, invalid, limited, writeErrors atomic.Uint64
}

func New(conn net.PacketConn, config Config) (*Server, error) {
	if conn == nil || config.RequestsPerSecond < 0 || config.SourceRequestsPerSecond < 0 || config.MaximumSources < 0 {
		return nil, errors.New("invalid STUN listener configuration")
	}
	if config.RequestsPerSecond == 0 {
		config.RequestsPerSecond = DefaultRequestsPerSecond
	}
	if config.SourceRequestsPerSecond == 0 {
		config.SourceRequestsPerSecond = DefaultSourceRequestsPerSecond
	}
	if config.MaximumSources == 0 {
		config.MaximumSources = DefaultMaximumSources
	}
	if config.Now == nil {
		config.Now = time.Now
	}
	return &Server{conn: conn, config: config, limiter: newRateLimiter(config)}, nil
}

func (s *Server) Metrics() Metrics {
	return Metrics{Received: s.received.Load(), Responded: s.responded.Load(), Invalid: s.invalid.Load(), Limited: s.limited.Load(), WriteErrors: s.writeErrors.Load(), Healthy: s.healthy.Load()}
}

// Serve owns this listener only; failure cannot close a sibling or relay socket.
func (s *Server) Serve(ctx context.Context) error {
	defer s.conn.Close()
	stop := context.AfterFunc(ctx, func() { _ = s.conn.Close() })
	defer stop()
	s.healthy.Store(true)
	defer s.healthy.Store(false)
	buffer := make([]byte, MaximumDatagramBytes+1)
	for {
		n, remote, err := s.conn.ReadFrom(buffer)
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			return err
		}
		s.received.Add(1)
		udp, ok := remote.(*net.UDPAddr)
		if !ok || udp.IP == nil || n > MaximumDatagramBytes {
			s.invalid.Add(1)
			continue
		}
		if !s.limiter.allow(udp.IP.String(), s.config.Now()) {
			s.limited.Add(1)
			continue
		}
		response, err := bindingResponse(buffer[:n], udp)
		if err != nil {
			s.invalid.Add(1)
			continue
		}
		if _, err = s.conn.WriteTo(response, remote); err != nil {
			s.writeErrors.Add(1)
			continue
		}
		s.responded.Add(1)
	}
}

func bindingResponse(data []byte, remote *net.UDPAddr) ([]byte, error) {
	request := &stun.Message{Raw: data}
	if err := request.Decode(); err != nil {
		return nil, err
	}
	if request.Type != stun.BindingRequest || len(data) != int(request.Length)+20 {
		return nil, errors.New("not a STUN binding request")
	}
	response, err := stun.Build(stun.NewTransactionIDSetter(request.TransactionID), stun.BindingSuccess,
		&stun.XORMappedAddress{IP: remote.IP, Port: remote.Port}, stun.Fingerprint)
	if err != nil {
		return nil, err
	}
	return response.Raw, nil
}
