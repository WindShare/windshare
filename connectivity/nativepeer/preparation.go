package nativepeer

import (
	"context"
	"errors"
	"sync"

	"github.com/windshare/windshare/connectivity/icepolicy"
	"github.com/windshare/windshare/connectivity/networkstate"
	"github.com/windshare/windshare/connectivity/v2signal"
	"github.com/windshare/windshare/transport/webrtc/provider"
)

// PreparedAttempt owns an admitted process opportunity. Separation from Start
// lets receivers queue before signaling; an incoming sender offer instead queues
// within its already-running preparation deadline. Neither spends ICE time.
type PreparedAttempt struct {
	native                *NativePeerConnectivity
	request               AttemptRequest
	snapshot              networkstate.Snapshot
	profile               icepolicy.AttemptICEProfile
	key                   pathKey
	path                  *pathResources
	ctx                   context.Context
	cancel                context.CancelFunc
	mu                    sync.Mutex
	permit                *attemptPermit
	transferred, released bool
	stop                  func()
	done                  chan struct{}
	finishOnce            sync.Once
}

func (p *PreparedAttempt) Matches(session [16]byte, binding v2signal.Binding) bool {
	return p != nil && p.request.ProtocolSessionID == session && p.request.Binding == binding
}

func (n *NativePeerConnectivity) PrepareAttempt(ctx context.Context, request AttemptRequest) (*PreparedAttempt, error) {
	if ctx == nil || request.ProtocolSessionID == ([16]byte{}) || request.Binding.Validate() != nil {
		return nil, errors.New("invalid native peer attempt")
	}
	n.mu.Lock()
	if n.closed {
		n.mu.Unlock()
		return nil, ErrProcessAdmission
	}
	n.operations.Add(1)
	n.mu.Unlock()
	defer n.operations.Done()
	snapshot, err := n.snapshot(ctx)
	if err != nil {
		return nil, err
	}
	n.mu.Lock()
	key := pathKey{request.ProtocolSessionID, request.Binding.PeerPathID}
	path, err := n.pathLocked(key)
	if err != nil {
		n.mu.Unlock()
		return nil, err
	}
	// Selection needs only local facts. No sockets, mappings, DNS or STUN traffic
	// are created until the exact immutable endpoint cost has been admitted.
	profile := n.selectProfileLocked(snapshot, path, request.Binding.AttemptSequence, n.config.Now())
	lifetime, cancel := context.WithCancel(context.Background())
	prepared := &PreparedAttempt{native: n, request: request, snapshot: snapshot, profile: profile, key: key, path: path, ctx: lifetime, cancel: cancel, done: make(chan struct{})}
	if path.attempts == nil {
		path.attempts = make(map[*PreparedAttempt]struct{})
	}
	path.attempts[prepared] = struct{}{}
	context.AfterFunc(lifetime, prepared.Close)
	n.mu.Unlock()
	wait, cancelWait := context.WithTimeout(ctx, ProcessQueueBudget)
	stop := context.AfterFunc(lifetime, cancelWait)
	defer func() { stop(); cancelWait() }()
	subject := Subject{ProtocolSessionID: key.session, PeerPathID: [16]byte(key.path), AttemptID: [16]byte(request.Binding.AttemptID), AttemptSequence: request.Binding.AttemptSequence, NetworkGenerationID: snapshot.GenerationID(), ICEProfileID: profile.ID(), Side: n.config.Side}
	permit, err := n.config.Admission.acquire(wait, n, len(profile.URLs()), func(facts AdmissionFacts) { n.producer.TryPublish(Observation{Subject: subject, Admission: &facts}) })
	if err != nil {
		prepared.Close()
		return nil, err
	}
	prepared.mu.Lock()
	prepared.permit = permit
	if !prepared.released {
		prepared.stop = n.config.Admission.clock.AfterFunc(ProcessAttemptBudget, func() { prepared.cancel(); prepared.Close() })
	}
	if prepared.released {
		permit.release()
	}
	prepared.mu.Unlock()
	if err := wait.Err(); err != nil {
		prepared.Close()
		return nil, err
	}
	return prepared, nil
}
func (p *PreparedAttempt) release() {
	p.mu.Lock()
	p.released = true
	permit, stop := p.permit, p.stop
	p.stop = nil
	p.mu.Unlock()
	if stop != nil {
		stop()
	}
	permit.release()
}

// Close abandons a preparation which has not transferred to a provider.
// Once Start succeeds, provider close or authenticated lane admission settles it.
func (p *PreparedAttempt) Close() {
	if p == nil {
		return
	}
	p.mu.Lock()
	transferred := p.transferred
	p.mu.Unlock()
	if transferred {
		return
	}
	p.finish()
}
func (p *PreparedAttempt) finish() {
	p.finishOnce.Do(func() {
		p.release()
		p.cancel()
		p.native.mu.Lock()
		delete(p.path.attempts, p)
		p.native.mu.Unlock()
		close(p.done)
	})
}
func (p *PreparedAttempt) abort() {
	p.cancel()
	p.Close()
	<-p.done
}
func (p *PreparedAttempt) Start(ctx context.Context) (*provider.Connection, error) {
	if p == nil || ctx == nil {
		return nil, ErrProcessAdmission
	}
	p.mu.Lock()
	if p.transferred || p.released {
		p.mu.Unlock()
		return nil, ErrProcessAdmission
	}
	p.transferred = true
	p.mu.Unlock()
	connection, err := p.connect(ctx)
	if err != nil {
		p.finish()
		return nil, err
	}
	go func() { <-p.ctx.Done(); _ = connection.Close() }()
	return connection, nil
}
func (p *PreparedAttempt) connect(ctx context.Context) (*provider.Connection, error) {
	n := p.native
	n.mu.Lock()
	path := p.path
	if ctx.Err() != nil || p.ctx.Err() != nil || n.paths[p.key] != path || path.retired || n.generation != p.snapshot.GenerationID() {
		n.mu.Unlock()
		return nil, errors.Join(ErrProcessAdmission, ctx.Err(), p.ctx.Err())
	}
	var err error
	if path.lease == nil || path.lease.GenerationID() != p.snapshot.GenerationID() {
		if path.lease != nil {
			_ = path.lease.Close()
		}
		path.lease, err = n.config.Sockets.Acquire(p.key.session, p.snapshot.GenerationID(), [16]byte(p.key.path), selectAddresses(p.snapshot.Addresses()))
		if err != nil {
			n.mu.Unlock()
			return nil, err
		}
		previous := path.generation
		path.generation = p.snapshot.GenerationID()
		n.observeLifecycleLocked(p.key, path, NetworkChanged, previous)
		path.mapped = false
	}
	now := n.config.Now()
	remoteProfile := path.remoteProfile
	if !now.Before(path.remoteProfileExpires) {
		remoteProfile = ""
	}
	capability := provider.Capabilities(remoteProfile)
	if capability.IPv4 || capability.IPv6 {
		_ = path.lease.PrepareTCP(capability.IPv6)
	}
	config := provider.AttemptConfig{ProtocolSessionID: p.key.session, PeerPathID: [16]byte(p.key.path), AttemptID: [16]byte(p.request.Binding.AttemptID),
		NetworkGenerationID: path.generation, ICEProfileID: p.profile.ID(), STUNURLs: p.profile.URLs(), SocketLease: path.lease, MappedEndpoints: n.mappedLocked(path),
		InitialCheckingTimeout: InitialCheckingBudget, TCPProfile: remoteProfile}
	observer := n.attemptObserver(p.key, path, p.profile, p.request.Binding, now)
	config.Observe = func(event provider.Event) {
		observer(event)
		if event.Milestone == "provider_closed" {
			p.finish()
		}
	}
	n.refreshDemandLocked(p.key, path)
	n.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	connection, err := n.config.Connect(p.request.Configuration, config)
	if err != nil {
		if connection != nil {
			_ = connection.Close()
		}
		return nil, err
	}
	if connection == nil {
		return nil, errors.New("native provider returned no connection")
	}
	if ctx.Err() != nil || p.ctx.Err() != nil {
		_ = connection.Close()
		return nil, errors.Join(ErrProcessAdmission, ctx.Err(), p.ctx.Err())
	}
	return connection, nil
}
