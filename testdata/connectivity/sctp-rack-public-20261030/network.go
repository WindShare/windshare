package main

import (
	"container/heap"
	"context"
	"net"
	"sync"
	"time"
)

type datagram struct {
	bytes   []byte
	due     time.Time
	order   uint64
	drained chan struct{}
}
type deliveries []datagram

func (d deliveries) Len() int { return len(d) }
func (d deliveries) Less(i, j int) bool {
	if d[i].due.Equal(d[j].due) {
		return d[i].order < d[j].order
	}
	return d[i].due.Before(d[j].due)
}
func (d deliveries) Swap(i, j int) { d[i], d[j] = d[j], d[i] }
func (d *deliveries) Push(x any)   { *d = append(*d, x.(datagram)) }
func (d *deliveries) Pop() any     { old := *d; n := len(old); x := old[n-1]; *d = old[:n-1]; return x }

type wireConn struct {
	*net.UDPConn
	remoteMu  sync.Mutex
	remote    *net.UDPAddr
	delay     time.Duration
	send      chan datagram
	ctx       context.Context
	cancel    context.CancelFunc
	wg        sync.WaitGroup
	faults    *faults
	record    *recorder
	name      string
	closeOnce sync.Once
	closeErr  error
}

func newWire(ctx context.Context, bind string, remote *net.UDPAddr, delay time.Duration, r *recorder, name string) (*wireConn, error) {
	addr, err := net.ResolveUDPAddr("udp4", bind)
	if err != nil {
		return nil, err
	}
	conn, err := net.ListenUDP("udp4", addr)
	if err != nil {
		return nil, err
	}
	child, cancel := context.WithCancel(ctx)
	w := &wireConn{UDPConn: conn, remote: remote, delay: delay, send: make(chan datagram, 256), ctx: child, cancel: cancel, record: r, name: name}
	w.wg.Add(1)
	go w.pump()
	return w, nil
}
func (w *wireConn) Close() error {
	w.closeOnce.Do(func() {
		// Flush accepted writes so endpoint shutdown does not silently drop its final control packet.
		drained := make(chan struct{})
		select {
		case w.send <- datagram{due: time.Now(), drained: drained}:
		case <-w.ctx.Done():
		}
		select {
		case <-drained:
		case <-w.ctx.Done():
		}
		w.cancel()
		w.closeErr = w.UDPConn.Close()
	})
	w.wg.Wait()
	return w.closeErr
}
func (w *wireConn) RemoteAddr() net.Addr {
	w.remoteMu.Lock()
	defer w.remoteMu.Unlock()
	return w.remote
}
func (w *wireConn) enqueue(p []byte, delay time.Duration) error {
	d := datagram{bytes: append([]byte(nil), p...), due: time.Now().Add(delay)}
	select {
	case w.send <- d:
		return nil
	case <-w.ctx.Done():
		return net.ErrClosed
	}
}
func (w *wireConn) Write(p []byte) (int, error) {
	delay := w.delay
	if w.faults != nil {
		drop, extra, release, err := w.faults.outbound(p)
		if err != nil {
			return 0, err
		}
		if release != nil {
			if err = w.enqueue(release, 0); err != nil {
				return 0, err
			}
		}
		if drop {
			return len(p), nil
		}
		delay += extra
	}
	if err := w.enqueue(p, delay); err != nil {
		return 0, err
	}
	return len(p), nil
}
func (w *wireConn) Read(p []byte) (int, error) {
	n, addr, err := w.UDPConn.ReadFromUDP(p)
	if err != nil {
		return n, err
	}
	w.remoteMu.Lock()
	if w.remote == nil {
		w.remote = addr
	}
	same := addr.String() == w.remote.String()
	w.remoteMu.Unlock()
	if !same {
		return 0, net.ErrClosed
	}
	if w.faults != nil {
		if err = w.faults.inbound(p[:n]); err != nil {
			return 0, err
		}
	}
	return n, nil
}
func (w *wireConn) pump() {
	defer w.wg.Done()
	queue := deliveries{}
	timer := time.NewTimer(time.Hour)
	defer timer.Stop()
	var sequence uint64
	for {
		var tick <-chan time.Time
		if len(queue) > 0 {
			timer.Reset(max(time.Until(queue[0].due), 0))
			tick = timer.C
		}
		select {
		case <-w.ctx.Done():
			return
		case item := <-w.send:
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
			if item.drained != nil {
				for _, pending := range queue {
					if !item.due.After(pending.due) {
						item.due = pending.due.Add(time.Nanosecond)
					}
				}
			}
			sequence++
			item.order = sequence
			heap.Push(&queue, item)
		case <-tick:
			item := heap.Pop(&queue).(datagram)
			if item.drained != nil {
				close(item.drained)
				continue
			}
			w.remoteMu.Lock()
			remote := w.remote
			w.remoteMu.Unlock()
			if _, err := w.UDPConn.WriteToUDP(item.bytes, remote); err != nil && w.ctx.Err() == nil {
				w.record.event("wire_error", map[string]any{"side": w.name, "error": err.Error()})
				w.cancel()
				_ = w.UDPConn.Close()
				return
			}
		}
	}
}
