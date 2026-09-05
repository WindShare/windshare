// Package networkstate freezes address and egress facts for a connectivity generation.
package networkstate

import (
	"cmp"
	"net/netip"
	"slices"
	"time"
)

type Address struct {
	IP             netip.Addr
	InterfaceIndex int
	InterfaceName  string
	AdapterID      string
	Class          string
	Tentative      bool
	Deprecated     bool
	PreferredUntil time.Time
	ValidUntil     time.Time
}
type Route struct {
	InterfaceIndex int
	Gateway        netip.Addr
	Family         int
	Metric         uint32
}
type State struct {
	Addresses      []Address
	Routes         []Route
	ResumeSequence uint64
}
type Snapshot struct {
	generation uint64
	state      State
}

func (s Snapshot) GenerationID() uint64 { return s.generation }
func (s Snapshot) Addresses() []Address { return slices.Clone(s.state.Addresses) }
func (s Snapshot) Routes() []Route      { return slices.Clone(s.state.Routes) }
func (s Snapshot) LocalAddresses() []netip.Addr {
	out := make([]netip.Addr, 0, len(s.state.Addresses))
	for _, a := range s.state.Addresses {
		out = append(out, a.IP)
	}
	return out
}
func normalize(state State, now time.Time) State {
	result := State{ResumeSequence: state.ResumeSequence}
	for _, a := range state.Addresses {
		a.IP = a.IP.Unmap()
		if !a.IP.IsValid() || a.IP.IsUnspecified() || a.IP.IsMulticast() || a.Tentative || a.Deprecated || (!a.ValidUntil.IsZero() && !now.Before(a.ValidUntil)) || (!a.PreferredUntil.IsZero() && !now.Before(a.PreferredUntil)) {
			continue
		}
		if a.IP.Is6() && a.IP.IsGlobalUnicast() && !a.IP.IsPrivate() {
			routed := false
			for _, route := range state.Routes {
				if route.Family == 6 && route.InterfaceIndex == a.InterfaceIndex {
					routed = true
					break
				}
			}
			if !routed {
				continue
			}
		}
		result.Addresses = append(result.Addresses, a)
	}
	slices.SortFunc(result.Addresses, func(a, b Address) int {
		if n := cmp.Compare(a.InterfaceIndex, b.InterfaceIndex); n != 0 {
			return n
		}
		return a.IP.Compare(b.IP)
	})
	result.Addresses = slices.Compact(result.Addresses)
	result.Routes = slices.Clone(state.Routes)
	slices.SortFunc(result.Routes, func(a, b Route) int {
		if n := cmp.Compare(a.Family, b.Family); n != 0 {
			return n
		}
		if n := cmp.Compare(a.Metric, b.Metric); n != 0 {
			return n
		}
		if n := cmp.Compare(a.InterfaceIndex, b.InterfaceIndex); n != 0 {
			return n
		}
		return a.Gateway.Compare(b.Gateway)
	})
	result.Routes = slices.Compact(result.Routes)
	return result
}

// Lifetimes count down in OS snapshots; only suitability transitions invalidate
// a generation, otherwise every poll would interrupt an unchanged connection.
func equivalent(a, b State) bool {
	if a.ResumeSequence != b.ResumeSequence || !slices.Equal(a.Routes, b.Routes) || len(a.Addresses) != len(b.Addresses) {
		return false
	}
	for i, x := range a.Addresses {
		y := b.Addresses[i]
		x.PreferredUntil = time.Time{}
		x.ValidUntil = time.Time{}
		y.PreferredUntil = time.Time{}
		y.ValidUntil = time.Time{}
		if x != y {
			return false
		}
	}
	return true
}

type Observer struct {
	Debounce     time.Duration
	current      Snapshot
	pending      State
	pendingSince time.Time
	pendingSet   bool
}

func (o *Observer) Observe(state State, now time.Time) (Snapshot, bool) {
	state = normalize(state, now)
	if o.current.generation == 0 {
		o.current = Snapshot{generation: 1, state: state}
		return o.current, true
	}
	if equivalent(o.current.state, state) {
		o.pendingSet = false
		return o.current, false
	}
	if !o.pendingSet || !equivalent(o.pending, state) {
		o.pending = state
		o.pendingSince = now
		o.pendingSet = true
	}
	if now.Sub(o.pendingSince) < o.Debounce {
		return o.current, false
	}
	o.current = Snapshot{generation: o.current.generation + 1, state: state}
	o.pendingSet = false
	return o.current, true
}
