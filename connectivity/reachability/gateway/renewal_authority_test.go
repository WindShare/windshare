package gateway

import (
	"context"
	"encoding/binary"
	"errors"
	"net/http"
	"net/netip"
	"strings"
	"testing"
	"time"

	r "github.com/windshare/windshare/connectivity/reachability"
)

func installedMapping(seconds string) *http.Response {
	return soapResponse("<NewLeaseDuration>" + seconds + "</NewLeaseDuration><NewInternalClient>192.168.1.2</NewInternalClient><NewInternalPort>4000</NewInternalPort><NewEnabled>1</NewEnabled>")
}

func TestUPnPRenewalPublicationAuthority(t *testing.T) {
	for _, mode := range []string{"permanent", "missing", "verification-timeout", "renewal-timeout"} {
		t.Run(mode, func(t *testing.T) {
			now := time.Unix(100, 0)
			renewing, deleted := false, 0
			client := &UPnP{Service: service(WANIPv1), HTTP: func(req *http.Request) (*http.Response, error) {
				action := req.Header.Get("SOAPAction")
				switch {
				case strings.Contains(action, "GetExternalIPAddress"):
					return soapResponse("<NewExternalIPAddress>8.8.8.8</NewExternalIPAddress>"), nil
				case strings.Contains(action, "AddPortMapping"):
					if renewing && mode == "renewal-timeout" {
						return nil, context.DeadlineExceeded
					}
				case strings.Contains(action, "GetSpecificPortMappingEntry"):
					if renewing {
						switch mode {
						case "permanent":
							return installedMapping("0"), nil
						case "missing":
							return soapResponse("<errorCode>714</errorCode>"), nil
						case "verification-timeout":
							return nil, context.DeadlineExceeded
						}
					}
					return installedMapping("120"), nil
				case strings.Contains(action, "DeletePortMapping"):
					deleted++
				}
				return soapResponse(""), nil
			}}
			a := r.New(r.Config{Now: func() time.Time { return now }, Gateways: []r.Gateway{client}})
			defer a.Close(context.Background())
			d := r.Demand{ID: "path", Endpoint: request().Endpoint, Until: now.Add(time.Minute), Content: true}
			if err := a.SetDemand(d); err != nil {
				t.Fatal(err)
			}
			a.Reconcile(context.Background())
			now = now.Add(r.DefaultHeadStart)
			a.Reconcile(context.Background())
			if len(a.Facts()) != 1 {
				t.Fatal("initial mapping not published")
			}
			original := a.Facts()[0]
			now = now.Add(time.Minute)
			d.Until = now.Add(time.Minute)
			if err := a.SetDemand(d); err != nil {
				t.Fatal(err)
			}
			<-a.Changes()
			renewing = true
			a.Reconcile(context.Background())
			lost := mode == "permanent" || mode == "missing"
			if lost {
				if len(a.Facts()) != 0 {
					t.Fatal("revoked/missing mapping remains published")
				}
				select {
				case <-a.Changes():
				default:
					t.Fatal("candidate consumers were not notified")
				}
			} else if facts := a.Facts(); len(facts) != 1 || facts[0] != original {
				t.Fatal("transient error changed unexpired authority", facts)
			}
			wantDeletes := 0
			if mode == "permanent" {
				wantDeletes = 1
			}
			if deleted != wantDeletes {
				t.Fatalf("deletions %d, want %d", deleted, wantDeletes)
			}
		})
	}
}

func TestDatagramRenewalExplicitLossAndTransientRetention(t *testing.T) {
	for _, protocol := range []string{"PCP", "NAT-PMP"} {
		for _, mode := range []string{"zero-lifetime", "invalid-lifetime", "timeout"} {
			t.Run(protocol+"/"+mode, func(t *testing.T) {
				renewing, deletes := false, 0
				exchange := func(ctx context.Context, _ netip.Addr, _ netip.AddrPort, body []byte) ([]byte, error) {
					if len(body) == 2 {
						return pmpResponse(body), nil
					}
					response := pcpResponse
					requestOffset, responseOffset := 4, 4
					if protocol == "NAT-PMP" {
						response = pmpResponse
						requestOffset, responseOffset = 8, 12
					}
					reply := response(body)
					if binary.BigEndian.Uint32(body[requestOffset:requestOffset+4]) == 0 {
						deletes++
						if ctx.Err() != nil {
							t.Error("cleanup inherited canceled context")
						}
						return reply, nil
					}
					if renewing {
						switch mode {
						case "timeout":
							return nil, context.DeadlineExceeded
						case "zero-lifetime":
							binary.BigEndian.PutUint32(reply[responseOffset:responseOffset+4], 0)
						case "invalid-lifetime":
							binary.BigEndian.PutUint32(reply[responseOffset:responseOffset+4], 121)
						}
					}
					return reply, nil
				}
				var client r.Gateway = &PCP{Egress: "7", Server: netip.MustParseAddrPort("192.168.1.1:5351"), Exchange: exchange}
				if protocol == "NAT-PMP" {
					client = &NATPMP{Egress: "7", Server: netip.MustParseAddrPort("192.168.1.1:5351"), Exchange: exchange}
				}
				now := time.Unix(100, 0)
				a := r.New(r.Config{Now: func() time.Time { return now }, Gateways: []r.Gateway{client}})
				defer a.Close(context.Background())
				d := r.Demand{ID: "path", Endpoint: request().Endpoint, Until: now.Add(time.Minute), Content: true}
				if err := a.SetDemand(d); err != nil {
					t.Fatal(err)
				}
				a.Reconcile(context.Background())
				now = now.Add(r.DefaultHeadStart)
				a.Reconcile(context.Background())
				if len(a.Facts()) != 1 {
					t.Fatal("initial mapping not published")
				}
				now = now.Add(time.Minute)
				d.Until = now.Add(time.Minute)
				if err := a.SetDemand(d); err != nil {
					t.Fatal(err)
				}
				renewing = true
				a.Reconcile(context.Background())
				wantFacts, wantDeletes := 0, 0
				if mode == "timeout" {
					wantFacts = 1
				}
				if mode == "invalid-lifetime" {
					wantDeletes = 1
				}
				if len(a.Facts()) != wantFacts || deletes != wantDeletes {
					t.Fatalf("facts=%d deletes=%d", len(a.Facts()), deletes)
				}
			})
		}
	}
}

func TestInvalidUPnPCreateCleansUpAfterCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	deleted := false
	client := &UPnP{Service: service(WANIPv1), HTTP: func(req *http.Request) (*http.Response, error) {
		switch action := req.Header.Get("SOAPAction"); {
		case strings.Contains(action, "GetExternalIPAddress"):
			return soapResponse("<NewExternalIPAddress>8.8.8.8</NewExternalIPAddress>"), nil
		case strings.Contains(action, "GetSpecificPortMappingEntry"):
			cancel()
			return nil, ctx.Err()
		case strings.Contains(action, "DeletePortMapping"):
			deleted = true
			deadline, ok := req.Context().Deadline()
			if req.Context().Err() != nil || !ok || time.Until(deadline) > r.DefaultOperationTimeout {
				t.Error("cleanup not independently bounded")
			}
		}
		return soapResponse(""), nil
	}}
	_, err := client.Create(ctx, request())
	if !errors.Is(err, r.ErrLeaseLost) || !deleted {
		t.Fatal("late invalid installation retained", err)
	}
}
