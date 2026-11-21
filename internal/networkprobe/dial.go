package networkprobe

import (
	"context"
	"errors"
	"net"
	"time"

	"github.com/windshare/windshare/connectivity/reachability"
)

const familyFallbackDelay = 250 * time.Millisecond

type dialFunc func(context.Context, string, string) (net.Conn, error)

// Resolve once and dial only checked addresses. TLS still authenticates the original hostname.
func dialPublic(ctx context.Context, network, address string, attempt dialFunc) (net.Conn, error) {
	ctx, cancel := context.WithTimeout(ctx, requestTimeout)
	defer cancel()
	host, port, err := net.SplitHostPort(address)
	if err != nil {
		return nil, err
	}
	ips, err := net.DefaultResolver.LookupNetIP(ctx, "ip", host)
	if err != nil {
		return nil, err
	}
	if len(ips) == 0 {
		return nil, errors.New("no node addresses")
	}
	var primary, fallback []string
	for _, ip := range ips {
		if !reachability.PublicAddress(ip) {
			return nil, errors.New("node address is not public")
		}
		group := &primary
		if ip.Unmap().Is4() != ips[0].Unmap().Is4() {
			group = &fallback
		}
		*group = append(*group, net.JoinHostPort(ip.Unmap().String(), port))
	}
	return raceFamilies(ctx, network, primary, fallback, attempt)
}

func raceFamilies(ctx context.Context, network string, primary, fallback []string, attempt dialFunc) (net.Conn, error) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	type outcome struct {
		conn net.Conn
		err  error
	}
	results := make(chan outcome)
	run := func(addrs []string) {
		var failures []error
		end, _ := ctx.Deadline()
		for i, address := range addrs {
			if ctx.Err() != nil {
				return
			}
			// One silent address must leave time for its siblings in the same family.
			trial, stop := context.WithTimeout(ctx, time.Until(end)/time.Duration(len(addrs)-i))
			conn, err := attempt(trial, network, address)
			stop()
			if err == nil {
				select {
				case results <- outcome{conn: conn}:
				case <-ctx.Done():
					_ = conn.Close()
				}
				return
			}
			failures = append(failures, err)
		}
		select {
		case results <- outcome{err: errors.Join(failures...)}:
		case <-ctx.Done():
		}
	}
	go run(primary)
	timer := time.NewTimer(familyFallbackDelay)
	defer timer.Stop()
	var delay <-chan time.Time
	if len(fallback) > 0 {
		delay = timer.C
	}
	active := 1
	var failures []error
	for active > 0 {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case res := <-results:
			if res.conn != nil {
				return res.conn, nil
			}
			failures = append(failures, res.err)
			active--
		case <-delay:
		}
		if delay != nil {
			delay = nil
			go run(fallback)
			active++
		}
	}
	return nil, errors.Join(failures...)
}
