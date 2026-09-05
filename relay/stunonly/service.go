package stunonly

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"sync"
	"time"
)

const adminReadTimeout = 5 * time.Second

// ServiceConfig describes only STUN resources; relay capacity and health are separate.
type ServiceConfig struct {
	UDPAddresses []string
	AdminAddress string
	Limits       Config
}

// Listeners allows the process owner to supply controlled sockets in lifecycle tests.
type Listeners struct {
	Packet func(string, string) (net.PacketConn, error)
	TCP    func(string, string) (net.Listener, error)
}

type Service struct {
	cancel    context.CancelFunc
	workers   sync.WaitGroup
	closeOnce sync.Once
	admin     *http.Server
}

// StartService records unavailable listeners instead of propagating their failures
// into the application relay. Even a total STUN outage leaves relay traffic usable.
func StartService(ctx context.Context, config ServiceConfig, listeners Listeners, logf func(string, ...any)) *Service {
	ctx, cancel := context.WithCancel(ctx)
	service := &Service{cancel: cancel}
	if listeners.Packet == nil {
		listeners.Packet = net.ListenPacket
	}
	if listeners.TCP == nil {
		listeners.TCP = net.Listen
	}
	if logf == nil {
		logf = func(string, ...any) {}
	}
	statuses := make([]ListenerStatus, 0, len(config.UDPAddresses))
	for index, address := range config.UDPAddresses {
		id := fmt.Sprintf("udp-%d", index)
		status := ListenerStatus{ID: id}
		conn, err := listeners.Packet("udp", address)
		if err == nil {
			status.Server, err = New(conn, config.Limits)
			if err != nil {
				_ = conn.Close()
			}
		}
		statuses = append(statuses, status)
		if err != nil {
			logf("wsrelay: stun_listener_unavailable listener_id=%s address=%q error=%q", id, address, err)
			continue
		}
		logf("wsrelay: stun_listener_started listener_id=%s address=%q", id, conn.LocalAddr())
		service.workers.Go(func() {
			err := status.Server.Serve(ctx)
			logf("wsrelay: stun_listener_stopped listener_id=%s canceled=%t error=%q", id, ctx.Err() != nil, err)
		})
	}
	if config.AdminAddress != "" && len(config.UDPAddresses) > 0 {
		service.startAdmin(config.AdminAddress, listeners.TCP, Handler(statuses), logf)
	}
	return service
}

func (s *Service) startAdmin(address string, listen func(string, string) (net.Listener, error), handler http.Handler, logf func(string, ...any)) {
	listener, err := listen("tcp", address)
	if err != nil {
		logf("wsrelay: stun_admin_unavailable address=%q error=%q", address, err)
		return
	}
	s.admin = &http.Server{Handler: handler, ReadHeaderTimeout: adminReadTimeout, ReadTimeout: adminReadTimeout, WriteTimeout: adminReadTimeout}
	logf("wsrelay: stun_admin_started address=%q", listener.Addr())
	s.workers.Go(func() {
		err := s.admin.Serve(listener)
		logf("wsrelay: stun_admin_stopped address=%q error=%q", address, err)
	})
}

// Close joins every owned worker before returning; no STUN goroutine survives
// relay startup failure, readiness failure, or normal process shutdown.
func (s *Service) Close() {
	s.closeOnce.Do(func() {
		s.cancel()
		if s.admin != nil {
			_ = s.admin.Close()
		}
		s.workers.Wait()
	})
}
