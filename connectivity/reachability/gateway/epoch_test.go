package gateway

import (
	"context"
	"encoding/binary"
	"net/netip"
	"testing"
)

func TestPCPAndNATPMPRecreateAfterEpochReset(t *testing.T) {
	epoch := uint32(100)
	ctx := context.Background()
	req := request()
	pcp := &PCP{Egress: "7", Server: netip.MustParseAddrPort("192.168.1.1:5351"), Exchange: func(_ context.Context, _ netip.Addr, _ netip.AddrPort, body []byte) ([]byte, error) {
		response := pcpResponse(body)
		binary.BigEndian.PutUint32(response[8:12], epoch)
		return response, nil
	}}
	pmp := &NATPMP{Egress: "7", Server: pcp.Server, Exchange: func(_ context.Context, _ netip.Addr, _ netip.AddrPort, body []byte) ([]byte, error) {
		response := pmpResponse(body)
		binary.BigEndian.PutUint32(response[4:8], epoch)
		return response, nil
	}}
	first, err := pcp.Create(ctx, req)
	if err != nil {
		t.Fatal(err)
	}
	second, err := pmp.Create(ctx, req)
	if err != nil {
		t.Fatal(err)
	}
	epoch = 1
	first, err = pcp.Renew(ctx, req, first)
	if err != nil || !first.ServerRestarted || first.ServerEpoch != 1 {
		t.Fatal(first, err)
	}
	second, err = pmp.Renew(ctx, req, second)
	if err != nil || !second.ServerRestarted || second.ServerEpoch != 1 {
		t.Fatal(second, err)
	}
	if err = pcp.Delete(ctx, req, first); err != nil {
		t.Fatal(err)
	}
	if err = pmp.Delete(ctx, req, second); err != nil {
		t.Fatal(err)
	}
}
