// Package websocketheartbeat detects failed WebSockets without owning reconnection.
package websocketheartbeat

import (
	"context"
	"errors"
	"time"
)

const (
	DefaultInterval = 15 * time.Second
	// Two bounded relay write/forward waits may precede the Pong. Reserve a
	// further interval for network delay rather than treating queue pressure as loss.
	DefaultTimeout = 45 * time.Second
)

var (
	ErrConfig = errors.New("websocket heartbeat: invalid configuration")
	ErrFailed = errors.New("websocket heartbeat: probe failed")
)

type Config struct {
	Interval time.Duration
	Timeout  time.Duration
}

func (c Config) Normalize() (Config, error) {
	if c.Interval == 0 {
		c.Interval = DefaultInterval
	}
	if c.Timeout == 0 {
		c.Timeout = DefaultTimeout
	}
	if c.Interval < 0 || c.Timeout < 0 {
		return Config{}, ErrConfig
	}
	return c, nil
}

type Pinger interface{ Ping(context.Context) error }

type Stage string

const (
	Probe        Stage = "probe"
	Acknowledged Stage = "acknowledged"
	Failed       Stage = "failed"
)

type Event struct {
	Round   uint64
	Stage   Stage
	Timeout time.Duration
	Elapsed time.Duration
	Cause   error
}

// Run keeps one probe outstanding, including its write and matching response.
// Socket implementations must respect the supplied context, as coder/websocket does.
func Run(ctx context.Context, socket Pinger, config Config, trace func(Event)) error {
	config, err := config.Normalize()
	if err != nil || socket == nil {
		return ErrConfig
	}
	timer := time.NewTimer(config.Interval)
	defer timer.Stop()
	for round := uint64(1); ; round++ {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-timer.C:
		}
		probeContext, cancel := context.WithTimeout(ctx, config.Timeout)
		started := time.Now()
		emit(trace, Event{Round: round, Stage: Probe, Timeout: config.Timeout})
		err := socket.Ping(probeContext)
		cancel()
		if ctx.Err() != nil {
			return ctx.Err()
		}
		event := Event{Round: round, Stage: Acknowledged, Timeout: config.Timeout, Elapsed: time.Since(started)}
		if err != nil {
			event.Stage, event.Cause = Failed, err
			emit(trace, event)
			return errors.Join(ErrFailed, err)
		}
		emit(trace, event)
		timer.Reset(config.Interval)
	}
}

func emit(trace func(Event), event Event) {
	if trace != nil {
		trace(event)
	}
}
