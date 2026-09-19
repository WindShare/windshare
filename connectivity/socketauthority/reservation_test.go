package socketauthority

import (
	"errors"
	"net"
	"net/netip"
	"testing"
)

func socketRequest(t *testing.T, a *Authority, path byte) *Request {
	t.Helper()
	r, err := a.Request([16]byte{1}, 1, [16]byte{path}, []netip.Addr{netip.MustParseAddr("127.0.0.1")})
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func TestReservationProtectsMandatoryUDPFromOptionalTCP(t *testing.T) {
	allocations := 0
	a := New(Config{Capacity: 2, ListenPacket: func(network, address string) (net.PacketConn, error) {
		allocations++
		return net.ListenPacket(network, address)
	}})
	defer a.Close()
	first, err := socketRequest(t, a, 1).Reserve()
	if err != nil {
		t.Fatal(err)
	}
	second, err := socketRequest(t, a, 2).Reserve()
	if err != nil {
		t.Fatal(err)
	}
	if allocations != 0 {
		t.Fatal("reservation opened sockets")
	}
	waiting := socketRequest(t, a, 3)
	changed := waiting.Changes()
	_, err = waiting.Reserve()
	capacity, ok := errors.AsType[*CapacityError](err)
	if !ok || capacity.Used != 0 || capacity.Reserved != 2 || capacity.Requested != 1 || capacity.Limit != 2 {
		t.Fatal("missing exact capacity cause", err)
	}
	if capacity.Error() == "" || !errors.Is(err, ErrCapacity) {
		t.Fatal(err)
	}
	lease, err := first.Activate()
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Close()
	if err := lease.PrepareTCP(false); !errors.Is(err, ErrCapacity) {
		t.Fatal("TCP stole reserved UDP", err)
	}
	second.Close()
	second.Close()
	select {
	case <-changed:
	default:
		t.Fatal("no release notification")
	}
	third, err := waiting.Reserve()
	if err != nil {
		t.Fatal(err)
	}
	third.Close()
	if err := lease.PrepareTCP(false); err != nil {
		t.Fatal(err)
	}
	if _, err := first.Activate(); !errors.Is(err, ErrInvalid) {
		t.Fatal("reservation activated twice", err)
	}
	first.Close()
}

func TestReservationRetainsExistingPathAndRejectsRetirement(t *testing.T) {
	for _, retirement := range []string{"none", "generation", "authority"} {
		t.Run(retirement, func(t *testing.T) {
			a := New(Config{Capacity: 1})
			defer a.Close()
			r := socketRequest(t, a, 1)
			initial, _ := r.Reserve()
			lease, err := initial.Activate()
			if err != nil {
				t.Fatal(err)
			}
			retained, err := r.Reserve()
			if err != nil {
				t.Fatal("existing path charged twice", err)
			}
			if err := lease.Close(); err != nil {
				t.Fatal(err)
			}
			switch retirement {
			case "generation":
				a.Retire(1)
			case "authority":
				_ = a.Close()
			}
			reused, err := retained.Activate()
			if retirement == "none" {
				if err != nil {
					t.Fatal(err)
				}
				_ = reused.Close()
			} else if err == nil {
				t.Fatal("retired allocation revived")
			}
			retained.Close()
		})
	}
}

func TestReservationAbandonmentAndFailedActivationReturnCapacity(t *testing.T) {
	a := New(Config{Capacity: 1, ListenPacket: func(string, string) (net.PacketConn, error) {
		return nil, errors.New("bind failed")
	}})
	defer a.Close()
	r := socketRequest(t, a, 1)
	first, _ := r.Reserve()
	if _, err := r.Reserve(); !errors.Is(err, ErrCapacity) {
		t.Fatal("same path reserved twice", err)
	}
	first.Close()
	second, err := r.Reserve()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := second.Activate(); err == nil {
		t.Fatal("bind failure lost")
	}
	third, err := r.Reserve()
	if err != nil {
		t.Fatal("failed bind leaked capacity", err)
	}
	a.Retire(1)
	if _, err := third.Activate(); !errors.Is(err, ErrRetired) {
		t.Fatal(err)
	}
	if _, err := r.Reserve(); !errors.Is(err, ErrRetired) {
		t.Fatal(err)
	}
	_ = a.Close()
	if _, err := r.Reserve(); !errors.Is(err, ErrClosed) {
		t.Fatal(err)
	}
	var absent *Reservation
	absent.Close()
	if _, err := a.Request([16]byte{1}, 1, [16]byte{1}, []netip.Addr{netip.MustParseAddr("127.0.0.1"), netip.MustParseAddr("127.0.0.2")}); !errors.Is(err, ErrInvalid) {
		t.Fatal("impossible request should not queue", err)
	}
}
