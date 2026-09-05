package networkstate

import (
	"context"
	"errors"
	"net/netip"
	"slices"
	"testing"
	"time"
)

func address(ip string, index int) Address {
	return Address{IP: netip.MustParseAddr(ip), InterfaceIndex: index, Class: "ethernet"}
}
func TestNormalizedImmutableGenerations(t *testing.T) {
	now := time.Unix(1000, 0)
	state := State{Addresses: []Address{address("192.168.1.2", 2), address("10.0.0.2", 1), address("192.168.1.2", 2)}, Routes: []Route{{InterfaceIndex: 2, Family: 4, Gateway: netip.MustParseAddr("192.168.1.1"), Metric: 10}, {InterfaceIndex: 1, Family: 4, Gateway: netip.MustParseAddr("10.0.0.1"), Metric: 5}}}
	observer := Observer{Debounce: time.Second}
	first, changed := observer.Observe(state, now)
	if !changed || first.GenerationID() != 1 || len(first.LocalAddresses()) != 2 {
		t.Fatal(first)
	}
	state.Addresses[0].Class = "vpn"
	if first.Addresses()[1].Class != "ethernet" {
		t.Fatal("input mutation")
	}
	output := first.Addresses()
	output[0].Class = "vpn"
	if first.Addresses()[0].Class != "ethernet" {
		t.Fatal("output mutation")
	}
	state.Addresses[0].Class = "ethernet"
	slices.Reverse(state.Addresses)
	slices.Reverse(state.Routes)
	if _, changed = observer.Observe(state, now); changed {
		t.Fatal("ordering created generation")
	}
	state.Routes[0].Metric++
	if _, changed = observer.Observe(state, now); changed {
		t.Fatal("debounce")
	}
	next, changed := observer.Observe(state, now.Add(time.Second))
	if !changed || next.GenerationID() != 2 {
		t.Fatal("route change not observed")
	}
	state.ResumeSequence++
	_, _ = observer.Observe(state, now.Add(2*time.Second))
	resumed, changed := observer.Observe(state, now.Add(3*time.Second))
	if !changed || resumed.GenerationID() != 3 {
		t.Fatal("resume not observed")
	}
}
func TestIPv6SuitabilityAndLifetimes(t *testing.T) {
	now := time.Unix(1000, 0)
	good := address("2606:4700::1", 2)
	good.PreferredUntil = now.Add(time.Minute)
	good.ValidUntil = now.Add(2 * time.Minute)
	deprecated := address("2606:4700::2", 2)
	deprecated.Deprecated = true
	tentative := address("2606:4700::3", 2)
	tentative.Tentative = true
	expired := address("2606:4700::4", 2)
	expired.ValidUntil = now
	unrouted := address("2606:4700::5", 3)
	state := State{Addresses: []Address{good, deprecated, tentative, expired, unrouted, address("fd00::1", 3), address("fe80::1%2", 2), address("::", 2), address("ff02::1", 2)}, Routes: []Route{{InterfaceIndex: 2, Family: 6}}}
	observer := Observer{}
	first, _ := observer.Observe(state, now)
	if len(first.Addresses()) != 3 {
		t.Fatalf("%+v", first.Addresses())
	}
	state.Addresses[0].PreferredUntil = now.Add(59 * time.Second)
	if _, changed := observer.Observe(state, now.Add(time.Second)); changed {
		t.Fatal("countdown caused generation churn")
	}
	second, changed := observer.Observe(state, now.Add(time.Minute))
	if !changed || len(second.Addresses()) != 2 {
		t.Fatal("expired IPv6 remained")
	}
}

type fakeSource struct {
	state State
	err   error
}

func (s *fakeSource) Snapshot(context.Context) (State, error) { return s.state, s.err }
func TestMonitorResumeAndErrors(t *testing.T) {
	source := &fakeSource{state: State{Addresses: []Address{address("10.0.0.2", 1)}}}
	monitor := NewMonitor(source, time.Nanosecond)
	now := time.Unix(1000, 0)
	if _, changed, err := monitor.Poll(context.Background(), now); err != nil || !changed {
		t.Fatal(err)
	}
	if _, changed, err := monitor.Poll(context.Background(), now.Add(ResumeGap+time.Second)); err != nil || changed {
		t.Fatal("resume debounce")
	}
	next, changed, err := monitor.Poll(context.Background(), now.Add(ResumeGap+2*time.Second))
	if err != nil || !changed || next.GenerationID() != 2 {
		t.Fatal("resume")
	}
	source.err = errors.New("snapshot unavailable")
	if snapshot, changed, err := monitor.Poll(context.Background(), now); err == nil || changed || snapshot.GenerationID() != 2 {
		t.Fatal("failed read changed generation")
	}
	if NewMonitor(nil, 0) == nil {
		t.Fatal("default monitor")
	}
}
func TestSystemSourceReadOnly(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := (SystemSource{}).Snapshot(ctx); err == nil {
		t.Fatal("cancelled snapshot")
	}
	state, err := (SystemSource{}).Snapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	observer := Observer{}
	snapshot, _ := observer.Observe(state, time.Now())
	if snapshot.GenerationID() != 1 {
		t.Fatal("system snapshot")
	}
}
