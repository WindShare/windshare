// SPDX-FileCopyrightText: 2026 WindShare contributors
// SPDX-License-Identifier: MIT

package client

import (
	"errors"
	"fmt"
	"time"

	"github.com/pion/stun/v4"
)

const (
	channelBindingLifetime = 10 * time.Minute
	bindingDemandLifetime  = time.Minute
	bindingRetryDelay      = time.Second
	maximumBindingFailures = 4
)

// BindingStatus reports protocol evidence. Only the embedding application knows
// whether an allocation carries enough demand to justify replacing its path.
type BindingStatus uint8

const (
	BindingActive BindingStatus = iota + 1
	BindingRetrying
	BindingRejected
	BindingMismatch
	BindingExpired
	BindingExhausted
)

func (s BindingStatus) String() string {
	switch s {
	case BindingActive:
		return "active"
	case BindingRetrying:
		return "retrying"
	case BindingRejected:
		return "rejected"
	case BindingMismatch:
		return "mismatch"
	case BindingExpired:
		return "expired"
	case BindingExhausted:
		return "exhausted"
	default:
		return "unknown"
	}
}

type BindingEvent struct {
	Status         BindingStatus
	PeerAddress    string
	RelayedAddress string
	Channel        uint16
	Code           int
}

type channelBindResponseError struct{ code int }

func (e channelBindResponseError) Error() string {
	return fmt.Sprintf("TURN ChannelBind rejected: %d", e.code)
}

func (c *UDPConn) observeBinding(bound *binding, status BindingStatus, code int) {
	c.log.Debugf("TURN binding transition: peer=%s channel=%d status=%d code=%d", bound.addr, bound.number, status, code)
	if c.onBindingEvent != nil {
		c.onBindingEvent(BindingEvent{Status: status, PeerAddress: bound.addr.String(),
			RelayedAddress: c.relayedAddr.String(), Channel: bound.number, Code: code})
	}
}

func (c *UDPConn) handleBindChannelError(bound *binding, _ bindingState, err error) {
	now := time.Now()
	status, code := BindingRejected, 0
	var response channelBindResponseError
	if errors.As(err, &response) {
		code = response.code
	}
	bound.muBind.Lock()
	switch {
	case code == int(stun.CodeBadRequest) || code == int(stun.CodeAllocMismatch):
		// Keep the number for inbound ChannelData, but stop trusting it for
		// outgoing traffic. A new number could compound a lost response.
		bound.setState(bindingStateMismatch)
		status = BindingMismatch
	case errors.Is(err, errChannelBindTransactionFailed) || errors.Is(err, errTryAgain) ||
		code == int(stun.CodeServerError) || code == int(stun.CodeInsufficientCapacity):
		bound.failures++
		if bound.failures >= maximumBindingFailures {
			bound.setState(bindingStateFailed)
			status = BindingExhausted
		} else {
			bound.retryAt = now.Add(bindingRetryDelay << (bound.failures - 1))
			if !bound.refreshedAt().IsZero() && now.Sub(bound.refreshedAt()) < channelBindingLifetime {
				bound.setState(bindingStateReadyUnknown)
			} else {
				bound.setState(bindingStateUnknown)
			}
			status = BindingRetrying
		}
	default:
		bound.setState(bindingStateFailed)
	}
	bound.muBind.Unlock()
	c.observeBinding(bound, status, code)
}
