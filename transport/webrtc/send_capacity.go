package webrtc

import (
	"context"
	"sync"
	"time"

	"github.com/windshare/windshare/core/framechannel"
)

const (
	aggregateSendBytes      = 4 << 20
	aggregateControlReserve = 4 * framechannel.MaxFrameSize
	aggregateSubmissions    = 4
	aggregateDrainPoll      = 5 * time.Millisecond
)

// SendCapacity is injected by the application. Pion's BufferedAmount retains
// each charge after Send returns; returning from Send is not delivery completion.
// Lane windows and SCTP congestion control remain independent.
type SendCapacity struct {
	mu          sync.Mutex
	channels    map[*Channel]struct{}
	reserved    uint64
	submissions int
	changed     chan struct{}
}

func NewSendCapacity() *SendCapacity {
	return &SendCapacity{channels: make(map[*Channel]struct{}), changed: make(chan struct{})}
}
func (s *SendCapacity) register(c *Channel) {
	if s == nil {
		return
	}
	s.mu.Lock()
	s.channels[c] = struct{}{}
	s.mu.Unlock()
}
func (s *SendCapacity) remove(c *Channel) {
	if s == nil {
		return
	}
	s.mu.Lock()
	delete(s.channels, c)
	s.wakeLocked()
	s.mu.Unlock()
}
func (s *SendCapacity) wake() {
	if s == nil {
		return
	}
	s.mu.Lock()
	s.wakeLocked()
	s.mu.Unlock()
}
func (s *SendCapacity) wakeLocked() { close(s.changed); s.changed = make(chan struct{}) }

func (s *SendCapacity) acquire(c *Channel, admission *sendAdmission, bytes uint64, control bool) (func(), error) {
	return s.reserve(admission.done, func() error { return c.lifecycle.sendAdmissionError(admission) }, bytes, control)
}

func (s *SendCapacity) reserve(stop <-chan struct{}, check func() error, bytes uint64, control bool) (func(), error) {
	if s == nil {
		return func() {}, nil
	}
	limit, bursts := uint64(aggregateSendBytes-aggregateControlReserve), aggregateSubmissions
	if control {
		limit, bursts = aggregateSendBytes, aggregateSubmissions+1
	}
	for {
		if err := check(); err != nil {
			return nil, err
		}
		s.mu.Lock()
		buffered := s.reserved
		for channel := range s.channels {
			buffered += channel.dc.BufferedAmount()
		}
		if buffered+bytes <= limit && s.submissions < bursts {
			s.reserved += bytes
			s.submissions++
			s.mu.Unlock()
			return sync.OnceFunc(func() {
				s.mu.Lock()
				s.reserved -= bytes
				s.submissions--
				s.wakeLocked()
				s.mu.Unlock()
			}), nil
		}
		changed := s.changed
		s.mu.Unlock()
		// Several individually small lane buffers can fill the aggregate window
		// without crossing a lane's low-water threshold. Poll only while blocked.
		timer := time.NewTimer(aggregateDrainPoll)
		select {
		case <-stop:
		case <-changed:
		case <-timer.C:
		}
		timer.Stop()
	}
}

// SendControl borrows the shared control reserve without changing wire ordering.
func (c *Channel) SendControl(ctx context.Context, frame framechannel.Frame) error {
	return c.send(ctx, frame, true)
}
