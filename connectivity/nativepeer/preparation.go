package nativepeer

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/windshare/windshare/connectivity/iceconfig"
	"github.com/windshare/windshare/connectivity/icepolicy"
	"github.com/windshare/windshare/connectivity/networkstate"
	"github.com/windshare/windshare/connectivity/pionpeer"
	"github.com/windshare/windshare/connectivity/v2signal"
)

// PreparedAttempt owns an admitted application opportunity. Separation from Start
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
	return n.PrepareAttemptWithin(ctx, request, QueueBudget)
}

// PrepareAttemptWithin admits immediately available resources even when no queue time remains.
func (n *NativePeerConnectivity) PrepareAttemptWithin(ctx context.Context, request AttemptRequest, queueWait time.Duration) (*PreparedAttempt, error) {
	if ctx == nil || queueWait < 0 || request.ProtocolSessionID == ([16]byte{}) || request.Binding.Validate() != nil {
		return nil, errors.New("invalid native peer attempt")
	}
	if cause := context.Cause(ctx); cause != nil {
		return nil, cause
	}
	n.mu.Lock()
	if n.closed {
		n.mu.Unlock()
		return nil, ErrAdmission
	}
	n.operations.Add(1)
	n.mu.Unlock()
	defer n.operations.Done()
	wait, cancelWait := context.WithCancelCause(ctx)
	defer cancelWait(context.Canceled)
	if queueWait > 0 {
		stopQueue := n.config.Admission.clock.AfterFunc(min(queueWait, QueueBudget), func() { cancelWait(context.DeadlineExceeded) })
		defer stopQueue()
		_, err := n.snapshot(wait)
		if cause := context.Cause(wait); cause != nil {
			return nil, cause
		}
		if err != nil {
			return nil, err
		}
	}
	n.mu.Lock()
	// A network change can supersede a poll before this lock is acquired.
	// Selecting from the latest generation also lets an immediate retry reuse
	// its sampled network without waiting for a cold OS lookup.
	snapshot := n.snapshotState
	if snapshot.GenerationID() == 0 {
		n.mu.Unlock()
		return nil, ErrAdmission
	}
	key := pathKey{request.ProtocolSessionID, request.Binding.PeerPathID}
	path, err := n.pathLocked(key)
	if err != nil {
		n.mu.Unlock()
		return nil, err
	}
	// Selection needs only local facts. No sockets, mappings, DNS or STUN traffic
	// are created until the exact immutable endpoint cost has been admitted.
	profile := n.selectProfileLocked(snapshot, path, request.Binding.AttemptSequence, n.config.Now())
	sockets, err := n.config.Sockets.Request(key.session, snapshot.GenerationID(), [16]byte(key.path), selectAddresses(snapshot.Addresses()))
	if err != nil {
		n.mu.Unlock()
		return nil, err
	}
	if path.lease != nil && path.lease.GenerationID() != snapshot.GenerationID() {
		// Retired providers retain their own references until teardown. The path
		// must drop its obsolete demand before waiting for replacement capacity.
		_ = path.lease.Close()
		path.lease = nil
	}
	lifetime, cancel := context.WithCancel(context.Background())
	prepared := &PreparedAttempt{native: n, request: request, snapshot: snapshot, profile: profile, key: key, path: path, ctx: lifetime, cancel: cancel, done: make(chan struct{})}
	if path.attempts == nil {
		path.attempts = make(map[*PreparedAttempt]struct{})
	}
	path.attempts[prepared] = struct{}{}
	if request.Binding.Purpose == v2signal.ThroughputExploration {
		n.holdOptionalLocked(key, path, sockets)
	}
	context.AfterFunc(lifetime, prepared.Close)
	n.mu.Unlock()
	stop := context.AfterFunc(lifetime, func() { cancelWait(context.Canceled) })
	defer stop()
	subject := Subject{ProtocolSessionID: key.session, PeerPathID: [16]byte(key.path), AttemptID: [16]byte(request.Binding.AttemptID), AttemptSequence: request.Binding.AttemptSequence, NetworkGenerationID: snapshot.GenerationID(), ICEProfileID: profile.ID(), Side: n.config.Side}
	permit, err := n.config.Admission.acquirePurpose(wait, n, len(profile.URLs()), sockets, func(facts AdmissionFacts) { n.producer.TryPublish(Observation{Subject: subject, Admission: &facts}) }, request.Binding.Purpose, queueWait)
	if err != nil {
		prepared.Close()
		return nil, err
	}
	prepared.mu.Lock()
	prepared.permit = permit
	if !prepared.released {
		prepared.stop = n.config.Admission.clock.AfterFunc(PermitLifetime, func() { prepared.cancel(); prepared.Close() })
	}
	if prepared.released {
		permit.release()
	}
	prepared.mu.Unlock()
	if err := context.Cause(wait); err != nil {
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

// Close abandons a preparation which has not transferred to a pionpeer.
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
func (p *PreparedAttempt) Start(ctx context.Context) (*pionpeer.Connection, error) {
	if p == nil || ctx == nil {
		return nil, ErrAdmission
	}
	p.mu.Lock()
	if p.transferred || p.released {
		p.mu.Unlock()
		return nil, ErrAdmission
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
func (p *PreparedAttempt) connect(ctx context.Context) (*pionpeer.Connection, error) {
	n := p.native
	n.mu.Lock()
	path := p.path
	if ctx.Err() != nil || p.ctx.Err() != nil || n.paths[p.key] != path || path.retired || n.generation != p.snapshot.GenerationID() {
		n.mu.Unlock()
		return nil, errors.Join(ErrAdmission, ctx.Err(), p.ctx.Err())
	}
	lease, err := p.permit.sockets.Activate()
	if err != nil {
		n.mu.Unlock()
		return nil, err
	}
	leaseInstalled := false
	if path.lease == nil || path.lease.GenerationID() != p.snapshot.GenerationID() {
		if path.lease != nil {
			_ = path.lease.Close()
		}
		path.lease = lease
		leaseInstalled = true
		previous := path.generation
		path.generation = p.snapshot.GenerationID()
		n.observeLifecycleLocked(p.key, path, NetworkChanged, previous)
		path.mapped = false
	} else {
		// The path already owns this allocation across serialized ICE attempts.
		_ = lease.Close()
	}
	// Prepare optional passive TCP listeners before refreshing reachability so
	// UPnP/PCP/NAT-PMP can map TCP alongside UDP; capacity/bind failures leave
	// the mandatory UDP path intact.
	_ = path.lease.PrepareTCP()
	iceConfiguration, err := iceconfig.NewDirect(p.profile.URLs())
	if err != nil {
		n.mu.Unlock()
		return nil, err
	}
	config := pionpeer.AttemptConfig{ProtocolSessionID: p.key.session, PeerPathID: [16]byte(p.key.path), AttemptID: [16]byte(p.request.Binding.AttemptID),
		NetworkGenerationID: path.generation, ICEProfileID: p.profile.ID(), ICE: iceConfiguration, SocketLease: path.lease, MappedEndpoints: n.mappedLocked(path)}
	// Release admission capacity synchronously inside Connection.Close so an
	// immediate retry never observes stale concurrency/active-time accounting
	// while a candidate observation or concurrent Close is still in flight.
	// Keeping finish on ProviderClosed preserves ClosePath's wait for terminal
	// observation delivery before CompleteObservations cuts the stream.
	config.OnClosed = func() {
		p.release()
		p.cancel()
	}
	observer := n.attemptObserver(p.key, path, p.profile, p.request.Binding)
	config.Observe = func(event pionpeer.Event) {
		observer(event)
		if event.Milestone == pionpeer.ProviderClosed {
			p.finish()
		}
	}
	n.refreshDemandLocked(p.key, path)
	if leaseInstalled && path.content {
		n.wakeMaintenanceLocked()
	}
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
		return nil, errors.Join(ErrAdmission, ctx.Err(), p.ctx.Err())
	}
	return connection, nil
}
