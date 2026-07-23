package main

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	pion "github.com/pion/webrtc/v4"
	"github.com/windshare/windshare/core/framechannel"
	windwebrtc "github.com/windshare/windshare/transport/webrtc"
)

// capacityWaitContext turns the adapter's context observation into a test event.
// Send observes Done once while acquiring its serialized send turn; a second
// observation occurs only after the authoritative buffer check enters the
// high-to-low wait. That boundary lets the real browser drive cancellation and
// remote closure without treating elapsed time as proof of blocking.
type capacityWaitContext struct {
	context.Context
	doneCalls atomic.Uint32
	onWait    func()
	waitOnce  sync.Once
}

func (c *capacityWaitContext) Done() <-chan struct{} {
	if c.doneCalls.Add(1) == 2 {
		c.waitOnce.Do(c.onWait)
	}
	return c.Context.Done()
}

func (s *interopServer) runOutboundScenario(channel *windwebrtc.Channel, raw *pion.DataChannel) {
	s.mu.Lock()
	probeReceived := s.result.ClientProbeReceived
	clientBursts := s.result.ClientBurstMessages
	s.mu.Unlock()
	if !probeReceived || clientBursts == 0 {
		s.fail("browser probe/backpressure evidence was incomplete")
		return
	}
	if err := s.saturateOutbound(channel, raw); err != nil {
		s.fail(err.Error())
		return
	}

	switch s.config.Scenario {
	case scenarioHappy:
		s.runHappyScenario(channel)
	case scenarioCancellation:
		s.runCancellationScenario(channel)
	case scenarioRemoteClose:
		s.runRemoteCloseScenario(channel)
	default:
		s.fail("internal scenario selection is invalid")
	}
}

func (s *interopServer) saturateOutbound(channel *windwebrtc.Channel, raw *pion.DataChannel) error {
	if err := channel.Send(context.Background(), patternedFrame(serverProbeMarker, framechannel.MaxFrameSize)); err != nil {
		return fmt.Errorf("send production 64 KiB probe: %w", err)
	}
	s.mu.Lock()
	s.result.ServerProbeSent = true
	s.mu.Unlock()

	burst := patternedFrame(serverBurstMarker, framechannel.MaxFrameSize)
	peak := raw.BufferedAmount()
	count := 0
	for peak < highWaterBytes && count < maximumBursts {
		if err := channel.Send(context.Background(), burst); err != nil {
			return fmt.Errorf("send production backpressure burst: %w", err)
		}
		count++
		peak = max(peak, raw.BufferedAmount())
	}
	if peak < highWaterBytes {
		return fmt.Errorf("Pion buffered amount peaked at %d before the safety bound", peak)
	}
	s.mu.Lock()
	s.result.ServerBurstMessages = count
	s.result.ServerBufferPeak = peak
	s.result.Events = append(s.result.Events, "server-buffer-high")
	s.mu.Unlock()
	return nil
}

func (s *interopServer) runHappyScenario(channel *windwebrtc.Channel) {
	// This marker must pass through Channel's high-to-low wait before terminal
	// admission, proving recovery uses Pion's callback rather than a timer.
	if err := channel.Send(context.Background(), framechannel.Frame{serverFinishedMarker}); err != nil {
		s.fail("send production burst completion marker: " + err.Error())
		return
	}
	s.event("server-burst-recovered")
	s.sendTerminalAndSettle(channel)
}

func (s *interopServer) runCancellationScenario(channel *windwebrtc.Channel) {
	baseContext, cancel := context.WithCancel(context.Background())
	defer cancel()
	result, waitObserved := s.startObservedSend(baseContext, channel, framechannel.Frame{canceledSendMarker})
	if err := waitForCapacityObservation(waitObserved, result); err != nil {
		s.fail("observe canceled production send: " + err.Error())
		return
	}

	s.event("send-cancel-requested")
	cancel()
	err := waitForSendResult(result)
	if !errors.Is(err, context.Canceled) {
		s.fail("canceled production send returned " + errorText(err))
		return
	}
	s.mu.Lock()
	s.result.SendCanceled = true
	s.result.SendError = err.Error()
	s.result.SendErrorCanceled = true
	s.result.Events = append(s.result.Events, "send-returned-context-canceled")
	s.mu.Unlock()

	// Ordered delivery of this barrier before the terminal lets the browser prove
	// the canceled marker was never enqueued, without an absence timeout.
	if err := channel.Send(context.Background(), framechannel.Frame{cancellationBarrier}); err != nil {
		s.fail("send post-cancellation barrier: " + err.Error())
		return
	}
	s.event("cancellation-barrier-sent")
	s.sendTerminalAndSettle(channel)
}

func (s *interopServer) runRemoteCloseScenario(channel *windwebrtc.Channel) {
	result, waitObserved := s.startObservedSend(context.Background(), channel, framechannel.Frame{remoteCloseMarker})
	if err := waitForCapacityObservation(waitObserved, result); err != nil {
		s.fail("observe production send before browser close: " + err.Error())
		return
	}

	s.event("browser-close-requested")
	s.actions <- actionCloseChannel
	err := waitForSendResult(result)
	if !errors.Is(err, windwebrtc.ErrRemoteClosed) {
		s.fail("production send interrupted by browser close returned " + errorText(err))
		return
	}
	s.mu.Lock()
	s.result.SendError = err.Error()
	s.result.SendErrorRemoteClosed = true
	s.result.Events = append(s.result.Events, "send-returned-remote-close")
	s.mu.Unlock()

	if !s.waitForDone(channel) {
		return
	}
	channelErr := channel.Err()
	s.mu.Lock()
	s.result.ChannelDone = true
	s.result.ChannelStateObserved = true
	s.result.ChannelStateClosed = channel.State() == framechannel.Closed
	s.result.ChannelError = errorText(channelErr)
	s.result.ChannelErrorRemoteClosed = errors.Is(channelErr, windwebrtc.ErrRemoteClosed)
	s.result.Events = append(s.result.Events, "channel-done")
	s.mu.Unlock()
	if channel.State() != framechannel.Closed || !errors.Is(channelErr, windwebrtc.ErrRemoteClosed) {
		s.fail("browser close did not publish the expected closed state and typed remote-close error")
		return
	}
	if err := channel.Close(); err != nil {
		s.fail("settle physically closed production Channel: " + err.Error())
		return
	}
	s.mu.Lock()
	s.result.PhysicalCloseSettled = true
	s.result.Events = append(s.result.Events, "physical-close-settled")
	s.mu.Unlock()
	s.complete()
}

func (s *interopServer) startObservedSend(
	ctx context.Context,
	channel *windwebrtc.Channel,
	frame framechannel.Frame,
) (<-chan error, <-chan struct{}) {
	waitObserved := make(chan struct{})
	observedContext := &capacityWaitContext{
		Context: ctx,
		onWait: func() {
			s.mu.Lock()
			s.result.SendWaitObserved = true
			s.result.Events = append(s.result.Events, "send-wait-observed")
			s.mu.Unlock()
			close(waitObserved)
		},
	}
	result := make(chan error, 1)
	go func() {
		result <- channel.Send(observedContext, frame)
	}()
	return result, waitObserved
}

func (s *interopServer) sendTerminalAndSettle(channel *windwebrtc.Channel) {
	s.event("terminal-send-started")
	if err := channel.SendTerminal(context.Background(), patternedFrame(serverTerminalMarker, terminalFrameBytes)); err != nil {
		s.fail("send acknowledged production terminal: " + err.Error())
		return
	}
	s.mu.Lock()
	s.result.TerminalAcknowledged = true
	s.mu.Unlock()
	s.event("terminal-acknowledged")
	if !s.waitForDone(channel) {
		return
	}
	channelErr := channel.Err()
	s.mu.Lock()
	s.result.ChannelDone = true
	s.result.ChannelStateObserved = true
	s.result.ChannelStateClosed = channel.State() == framechannel.Closed
	s.result.ChannelError = errorText(channelErr)
	s.result.Events = append(s.result.Events, "channel-done")
	s.mu.Unlock()
	if channel.State() != framechannel.Closed || channelErr != nil {
		s.fail("acknowledged terminal did not close the production Channel cleanly")
		return
	}
	if err := channel.Close(); err != nil {
		s.fail("settle terminal production Channel close: " + err.Error())
		return
	}
	s.mu.Lock()
	s.result.PhysicalCloseSettled = true
	s.result.Events = append(s.result.Events, "physical-close-settled")
	s.mu.Unlock()
	s.complete()
}

func (s *interopServer) waitForDone(channel *windwebrtc.Channel) bool {
	timer := time.NewTimer(operationLimit)
	defer timer.Stop()
	select {
	case <-channel.Done():
		return true
	case <-timer.C:
		s.fail("production Channel Done did not close")
		return false
	}
}

func waitForCapacityObservation(observed <-chan struct{}, result <-chan error) error {
	timer := time.NewTimer(operationLimit)
	defer timer.Stop()
	select {
	case <-observed:
		return nil
	case err := <-result:
		if err == nil {
			return errors.New("send completed before entering capacity wait")
		}
		return fmt.Errorf("send completed before entering capacity wait: %w", err)
	case <-timer.C:
		return errors.New("send did not enter capacity wait")
	}
}

func waitForSendResult(result <-chan error) error {
	timer := time.NewTimer(operationLimit)
	defer timer.Stop()
	select {
	case err := <-result:
		return err
	case <-timer.C:
		return errors.New("send did not settle after its wake event")
	}
}
