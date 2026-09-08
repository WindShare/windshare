// Package socketauthority owns bounded, independent per-path UDP endpoints.
// A lease survives a serialized fresh ICE attempt, while no endpoint is shared
// between peer paths whose remote tuples can otherwise collide in Pion's mux.
package socketauthority

import (
	"errors"
	"fmt"
	"net"
	"net/netip"
	"slices"
	"sync"
	"time"

	"github.com/pion/ice/v4"
)

const (
	DefaultCapacity       = 64
	MaxAddressesPerPath   = 16
	DefaultIdleInterval   = 15 * time.Second
	DefaultRefreshTimeout = 500 * time.Millisecond
)

var (
	ErrClosed   = errors.New("socket authority closed")
	ErrCapacity = errors.New("socket capacity exhausted")
	ErrRetired  = errors.New("network generation retired")
	ErrActive   = errors.New("peer path already has an ICE owner")
	ErrInvalid  = errors.New("invalid socket lease request")
)

// ListenPacket is injected so allocation and failure cleanup are testable.
type ListenPacket func(network, address string) (net.PacketConn, error)

type Config struct {
	Capacity       int
	ListenPacket   ListenPacket
	ListenTCP      func(network, address string) (net.Listener, error)
	IdleInterval   time.Duration
	RefreshTimeout time.Duration
	// Observe must publish promptly; it is never called while holding the authority lock.
	Observe func(Event)
}

type pathKey struct {
	session    [16]byte
	generation uint64
	path       [16]byte
}

type pathSockets struct {
	key       pathKey
	addresses []netip.Addr
	mux       *Mux
	tcp       *TCPMux
	refs      int
	owner     iceOwnership
	idle      *idleTask
	closing   *socketClosure
}

// Authority owns physical sockets; leases own only demand references.
type Authority struct {
	mu             sync.Mutex
	config         Config
	paths          map[pathKey]*pathSockets
	retiredThrough uint64
	socketCount    int
	closing        *socketClosure
}

func New(config Config) *Authority {
	if config.Capacity <= 0 {
		config.Capacity = DefaultCapacity
	}
	if config.IdleInterval <= 0 {
		config.IdleInterval = DefaultIdleInterval
	}
	if config.RefreshTimeout <= 0 {
		config.RefreshTimeout = DefaultRefreshTimeout
	}
	if config.ListenTCP == nil {
		config.ListenTCP = net.Listen
	}
	if config.ListenPacket == nil {
		config.ListenPacket = net.ListenPacket
	}
	return &Authority{config: config, paths: make(map[pathKey]*pathSockets)}
}

// Acquire freezes addresses for this generation/path. Callers must allocate a
// newer generation for address/route changes; an existing snapshot is never edited.
func (a *Authority) Acquire(session [16]byte, generation uint64, path [16]byte, addresses []netip.Addr) (*Lease, error) {
	addresses = slices.Clone(addresses)
	// Preserve the caller's interface/family opportunity order through socket
	// allocation and provider priority. Equal IPs need only one physical socket.
	seen := make(map[netip.Addr]bool)
	addresses = slices.DeleteFunc(addresses, func(address netip.Addr) bool {
		duplicate := seen[address]
		seen[address] = true
		return duplicate
	})
	if session == ([16]byte{}) || generation == 0 || path == ([16]byte{}) || len(addresses) == 0 || len(addresses) > MaxAddressesPerPath {
		return nil, ErrInvalid
	}
	for _, address := range addresses {
		if !address.IsValid() || address.IsUnspecified() || address.IsMulticast() {
			return nil, ErrInvalid
		}
	}
	// PeerPathID is session-scoped; equal values in independent authenticated
	// sessions must never share socket or exclusive ICE ownership.
	key := pathKey{session, generation, path}
	a.mu.Lock()
	defer a.mu.Unlock()
	for {
		if a.closing != nil {
			return nil, ErrClosed
		}
		if generation <= a.retiredThrough {
			return nil, ErrRetired
		}
		entry := a.paths[key]
		if entry == nil {
			break
		}
		if entry.closing != nil {
			closing := entry.closing
			a.mu.Unlock()
			_ = closing.wait()
			a.mu.Lock()
			continue
		}
		if !slices.Equal(addresses, entry.addresses) {
			return nil, ErrInvalid
		}
		entry.refs++
		return &Lease{authority: a, entry: entry}, nil
	}
	if a.socketCount+len(addresses) > a.config.Capacity {
		return nil, ErrCapacity
	}
	mux := &Mux{}
	for _, address := range addresses {
		network := "udp6"
		if address.Is4() {
			network = "udp4"
		}
		conn, err := a.config.ListenPacket(network, net.JoinHostPort(address.String(), "0"))
		if err != nil {
			_ = mux.Close()
			return nil, fmt.Errorf("allocate %s endpoint: %w", network, err)
		}
		mux.endpoints = append(mux.endpoints, ice.NewUniversalUDPMuxDefault(ice.UniversalUDPMuxParams{UDPConn: conn}))
	}
	entry := &pathSockets{key: key, addresses: addresses, mux: mux, refs: 1}
	a.paths[key] = entry
	a.socketCount += len(addresses)
	return &Lease{authority: a, entry: entry}, nil
}

// Retire prevents new demand but lets existing admitted lanes release normally.
func (a *Authority) Retire(generation uint64) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if generation > a.retiredThrough {
		a.retiredThrough = generation
		for _, entry := range a.paths {
			if entry.key.generation <= generation && entry.idle != nil {
				entry.idle.cancel()
			}
		}
	}
}

// Lease is a separately releasable demand reference. Retain never revives a
// released lease or a retired network generation.
type Lease struct {
	authority *Authority
	entry     *pathSockets
	released  bool
}

func (l *Lease) Retain() (*Lease, error) {
	if l == nil {
		return nil, ErrInvalid
	}
	a := l.authority
	a.mu.Lock()
	defer a.mu.Unlock()
	if err := l.unavailableLocked(); err != nil {
		return nil, err
	}
	l.entry.refs++
	return &Lease{authority: a, entry: l.entry}, nil
}

func (l *Lease) SessionID() [16]byte  { return l.entry.key.session }
func (l *Lease) GenerationID() uint64 { return l.entry.key.generation }
func (l *Lease) PathID() [16]byte     { return l.entry.key.path }
func (l *Lease) Endpoints() []netip.AddrPort {
	addresses := l.entry.mux.GetListenAddresses()
	result := make([]netip.AddrPort, 0, len(addresses))
	for _, addr := range addresses {
		result = append(result, addr.(*net.UDPAddr).AddrPort())
	}
	return result
}
