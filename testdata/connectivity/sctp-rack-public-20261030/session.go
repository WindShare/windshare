package main

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"sync/atomic"
	"time"

	"github.com/pion/sctp"
)

func payload(seq, size int) []byte {
	p := make([]byte, size)
	binary.BigEndian.PutUint32(p, uint32(seq))
	for i := 4; i < len(p); i++ {
		p[i] = byte(seq + i)
	}
	return p
}
func waitFor(ctx context.Context, done func() bool) error {
	tick := time.NewTicker(time.Millisecond)
	defer tick.Stop()
	for !done() {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-tick.C:
		}
	}
	return nil
}
func send(ctx context.Context, o options, r *recorder, w *wireConn, f *faults) (map[string]any, func(), error) {
	stop := context.AfterFunc(ctx, func() { _ = w.Close() })
	cleanup := func() { stop(); _ = w.Close() }
	a, err := sctp.ClientContext(ctx, sctp.WithNetConn(w), sctp.WithName("sender"), sctp.WithLoggerFactory(r.factory()), sctp.WithEnableInterleaving(o.interleaving))
	if err != nil {
		return nil, cleanup, err
	}
	cleanup = func() { stop(); _ = a.Close() }
	stream, err := a.OpenStream(0, sctp.PayloadTypeWebRTCBinary)
	if err != nil {
		return nil, cleanup, err
	}
	deadline, _ := ctx.Deadline()
	_ = stream.SetReadDeadline(deadline)
	total := warmupMessages + o.messages
	var received atomic.Int64
	readDone := make(chan error, 1)
	go func() {
		buf := make([]byte, 65536)
		for seq := range total {
			n, _, err := stream.ReadSCTP(buf)
			if err != nil {
				readDone <- err
				return
			}
			if !bytes.Equal(buf[:n], payload(seq, o.size)) {
				readDone <- fmt.Errorf("echo mismatch at message %d", seq)
				return
			}
			received.Add(1)
		}
		readDone <- nil
	}()
	write := func(seq int) error {
		if _, err := stream.WriteSCTP(payload(seq, o.size), sctp.PayloadTypeWebRTCBinary); err != nil {
			return err
		}
		spacing := o.interval
		if o.scenario == "ambiguous" && seq == warmupMessages+1 {
			spacing = time.Millisecond
		}
		select {
		case <-time.After(spacing):
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	for seq := range warmupMessages {
		if err = write(seq); err != nil {
			return nil, cleanup, err
		}
	}
	if err = waitFor(ctx, func() bool { return received.Load() == warmupMessages && a.BufferedAmount() == 0 }); err != nil {
		return nil, cleanup, fmt.Errorf("warmup: %w", err)
	}
	r.event("warmup_complete", map[string]any{"srtt_ms": a.SRTT(), "cwnd": a.CWND()})
	f.arm()
	start := time.Now()
	for seq := warmupMessages; seq < total; seq++ {
		if err = write(seq); err != nil {
			return nil, cleanup, err
		}
	}
	select {
	case err = <-readDone:
	case <-ctx.Done():
		err = ctx.Err()
	}
	if err != nil {
		return nil, cleanup, err
	}
	if err = waitFor(ctx, func() bool { return a.BufferedAmount() == 0 }); err != nil {
		return nil, cleanup, err
	}
	if err = f.validate(); err != nil {
		return nil, cleanup, err
	}
	summary := f.summary()
	summary["verified_messages"], summary["verified_bytes"] = total, total*o.size
	summary["transfer_ms"] = float64(time.Since(start).Microseconds()) / 1000
	summary["srtt_ms"] = a.SRTT()
	if err = a.Shutdown(ctx); err != nil {
		return nil, cleanup, err
	}
	return summary, cleanup, nil
}
func echo(ctx context.Context, o options, r *recorder, w *wireConn) error {
	stop := context.AfterFunc(ctx, func() { _ = w.Close() })
	defer stop()
	a, err := sctp.ServerWithOptions(sctp.WithNetConn(w), sctp.WithName("receiver"), sctp.WithLoggerFactory(r.factory()), sctp.WithEnableInterleaving(o.interleaving))
	if err != nil {
		return err
	}
	defer a.Close()
	stream, err := a.AcceptStream()
	if err != nil {
		return err
	}
	deadline, _ := ctx.Deadline()
	_ = stream.SetReadDeadline(deadline)
	buf := make([]byte, 65536)
	for seq := 0; seq < warmupMessages+o.messages; seq++ {
		n, ppi, err := stream.ReadSCTP(buf)
		if err != nil {
			return err
		}
		if !bytes.Equal(buf[:n], payload(seq, o.size)) {
			return fmt.Errorf("receiver mismatch at message %d", seq)
		}
		if _, err = stream.WriteSCTP(buf[:n], ppi); err != nil {
			return err
		}
	}
	r.event("receive_complete", map[string]any{"messages": warmupMessages + o.messages})
	_, _, err = stream.ReadSCTP(buf)
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if errors.Is(err, io.EOF) || errors.Is(err, net.ErrClosed) {
		r.event("receive_closed", map[string]any{"error": err.Error()})
		return nil
	}
	if err != nil {
		return err
	}
	return fmt.Errorf("receiver got an unexpected extra message")
}
func serve(ctx context.Context, o options, r *recorder) error {
	w, err := newWire(ctx, o.listen, nil, o.delay, r, "receiver")
	if err != nil {
		return err
	}
	defer w.Close()
	if err = json.NewEncoder(os.Stdout).Encode(map[string]any{"ready": w.LocalAddr().String()}); err != nil {
		return err
	}
	return echo(ctx, o, r, w)
}
