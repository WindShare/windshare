package main

import (
	"errors"
	"fmt"
	"sync"
	"time"

	pion "github.com/pion/webrtc/v4"
	"github.com/windshare/windshare/core/session"
	windwebrtc "github.com/windshare/windshare/transport/webrtc"
)

func (s *interopServer) recordMalformedRejection(raw *pion.DataChannel, err error) {
	typed := errors.Is(err, windwebrtc.ErrInvalidDataChannel)
	s.mu.Lock()
	s.result.InvalidChannelRejected = true
	s.result.InvalidChannelError = err.Error()
	s.result.InvalidChannelErrorTyped = typed
	s.result.Events = append(s.result.Events, "adapter-invalid-channel-rejected")
	if !typed {
		s.result.Errors = append(s.result.Errors, "malformed DataChannel rejection was not ErrInvalidDataChannel")
	}
	if raw.Protocol() != invalidProtocol {
		s.result.Errors = append(s.result.Errors, fmt.Sprintf("Pion exposed protocol %q, want %q", raw.Protocol(), invalidProtocol))
	}
	s.mu.Unlock()
	go s.settleRejectedRawChannel(raw)
}

func (s *interopServer) settleRejectedRawChannel(raw *pion.DataChannel) {
	rawClosed := make(chan struct{})
	var rawClosedOnce sync.Once
	raw.OnClose(func() {
		s.event("raw-channel-closed")
		rawClosedOnce.Do(func() { close(rawClosed) })
	})

	s.event("raw-close-requested")
	if err := raw.Close(); err != nil {
		s.appendScenarioError("close rejected raw DataChannel: " + err.Error())
	}

	timer := time.NewTimer(operationLimit)
	defer timer.Stop()
	select {
	case <-rawClosed:
		state := raw.ReadyState()
		s.mu.Lock()
		s.result.RawChannelState = state.String()
		s.result.RawChannelStateClosed = state == pion.DataChannelStateClosed
		s.result.PhysicalCloseSettled = state == pion.DataChannelStateClosed
		s.result.Events = append(s.result.Events, "physical-close-settled")
		if state != pion.DataChannelStateClosed {
			s.result.Errors = append(s.result.Errors, "rejected raw DataChannel close callback did not expose Closed state")
		}
		s.mu.Unlock()
	case <-timer.C:
		s.appendScenarioError("rejected raw DataChannel did not publish its close event")
	}

	s.settlePeerConnection()
}

func (s *interopServer) settleUnexpectedMalformedAcceptance(channel *windwebrtc.Channel) {
	s.appendScenarioError("production Channel accepted the malformed browser protocol")
	if err := channel.Close(); err != nil {
		s.appendScenarioError("close unexpectedly accepted production Channel: " + err.Error())
	}
	s.mu.Lock()
	select {
	case <-channel.Opened():
		s.result.ChannelOpened = true
	default:
	}
	select {
	case <-channel.Done():
		s.result.ChannelDone = true
	default:
	}
	s.result.ChannelStateObserved = true
	s.result.ChannelStateClosed = channel.State() == session.Closed
	s.result.PhysicalCloseSettled = true
	s.result.Events = append(s.result.Events, "unexpected-channel-settled")
	s.mu.Unlock()
	s.settlePeerConnection()
}

func (s *interopServer) settlePeerConnection() {
	s.event("peer-close-requested")
	err := s.peer.Close()
	s.mu.Lock()
	if err != nil {
		s.result.Errors = append(s.result.Errors, "close Pion peer connection: "+err.Error())
	} else {
		s.result.PeerCloseSettled = true
		s.result.Events = append(s.result.Events, "peer-close-settled")
	}
	s.mu.Unlock()
	s.complete()
}

func (s *interopServer) appendScenarioError(message string) {
	s.mu.Lock()
	s.result.Errors = append(s.result.Errors, message)
	s.mu.Unlock()
}
