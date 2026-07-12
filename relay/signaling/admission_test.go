package signaling_test

import (
	"bytes"
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/windshare/windshare/internal/testnetwork"
	"github.com/windshare/windshare/relay/admission"
	"github.com/windshare/windshare/relay/httpapi"
	"github.com/windshare/windshare/relay/protocol"
	"github.com/windshare/windshare/relay/signaling"
)

const testSourceHeader = "X-WindShare-Test-Source"

func admissionController(t *testing.T, mutate func(*admission.Config)) *admission.Controller {
	t.Helper()
	cfg := admission.DefaultConfig()
	if mutate != nil {
		mutate(&cfg)
	}
	controller, err := admission.NewController(cfg)
	if err != nil {
		t.Fatal(err)
	}
	return controller
}

func sourceAPI() httpapi.Config {
	return httpapi.Config{SourceIdentity: func(r *http.Request) string {
		return r.Header.Get(testSourceHeader)
	}}
}

func dialAs(t *testing.T, tsURL, shareID, source string) *tc {
	t.Helper()
	testnetwork.RequireOSNetwork(t)
	ctx, cancel := context.WithTimeout(context.Background(), ioTimeout)
	defer cancel()
	url := "ws" + strings.TrimPrefix(tsURL, "http") + "/v1/ws/" + shareID
	ws, _, err := websocket.Dial(ctx, url, &websocket.DialOptions{
		HTTPHeader: http.Header{testSourceHeader: []string{source}},
	})
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	ws.SetReadLimit(1 << 26)
	t.Cleanup(func() { _ = ws.CloseNow() })
	return &tc{t: t, ws: ws}
}

func registerAs(t *testing.T, tsURL, shareID, source string, token, sealed []byte) *tc {
	t.Helper()
	c := dialAs(t, tsURL, shareID, source)
	c.send(protocol.NewRegister(shareID, protocol.HashResumeToken(token)))
	c.sendRaw(websocket.MessageBinary, protocol.EncodeManifestFrame(sealed))
	if message, ok := c.readMsg().(*protocol.Registered); !ok || message.ShareID != shareID {
		t.Fatalf("register response = %+v", message)
	}
	return c
}

func waitForAdmission(t *testing.T, timeout time.Duration, controller *admission.Controller, predicate func(admission.Snapshot) bool) admission.Snapshot {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		snapshot := controller.Snapshot()
		if predicate(snapshot) {
			return snapshot
		}
		if time.Now().After(deadline) {
			t.Fatalf("admission state did not converge: %+v", snapshot)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestConnectionAdmissionCapReleasesOnActualClose(t *testing.T) {
	testnetwork.RequireOSNetwork(t)
	controller := admissionController(t, func(cfg *admission.Config) {
		cfg.MaxConnections = 1
	})
	ts, _ := startRelay(t, signaling.Config{
		Admission:   controller,
		RoleTimeout: time.Second,
	}, sourceAPI())

	first := dialAs(t, ts.URL, shareA, "source-a")
	waitForAdmission(t, time.Second, controller, func(s admission.Snapshot) bool { return s.Connections == 1 })

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	url := "ws" + strings.TrimPrefix(ts.URL, "http") + "/v1/ws/" + shareB
	second, resp, err := websocket.Dial(ctx, url, &websocket.DialOptions{
		HTTPHeader: http.Header{testSourceHeader: []string{"source-b"}},
	})
	cancel()
	if second != nil {
		_ = second.CloseNow()
	}
	if err == nil || resp == nil || resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("second connection was upgraded: err=%v response=%v", err, resp)
	}
	if got := controller.Snapshot().Connections; got != 1 {
		t.Fatalf("rejected connection leaked capacity: %d", got)
	}

	_ = first.ws.CloseNow()
	waitForAdmission(t, time.Second, controller, func(s admission.Snapshot) bool { return s.Connections == 0 })
	third := dialAs(t, ts.URL, shareB, "source-b")
	waitForAdmission(t, time.Second, controller, func(s admission.Snapshot) bool { return s.Connections == 1 })
	_ = third.ws.CloseNow()
}

func TestHubCloseImmediatelyReleasesSilentConnection(t *testing.T) {
	controller := admissionController(t, nil)
	ts, hub := startRelay(t, signaling.Config{
		Admission:   controller,
		RoleTimeout: time.Minute,
	}, sourceAPI())
	_ = dialAs(t, ts.URL, shareA, "silent-source")
	waitForAdmission(t, time.Second, controller, func(s admission.Snapshot) bool {
		return s.Connections == 1
	})

	hub.Close()
	waitForAdmission(t, time.Second, controller, func(s admission.Snapshot) bool {
		return s.Connections == 0
	})
}

func TestFailedUpgradeReleasesConnectionAdmission(t *testing.T) {
	testnetwork.RequireOSNetwork(t)
	controller := admissionController(t, func(cfg *admission.Config) {
		cfg.MaxConnections = 1
	})
	ts, _ := startRelay(t, signaling.Config{Admission: controller}, sourceAPI())
	resp, err := http.Get(ts.URL + "/v1/ws/" + shareA)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode == http.StatusSwitchingProtocols {
		t.Fatal("ordinary HTTP request unexpectedly upgraded")
	}
	if snapshot := controller.Snapshot(); snapshot.Connections != 0 {
		t.Fatalf("failed upgrade leaked connection admission: %+v", snapshot)
	}
	client := dialAs(t, ts.URL, shareA, "source-a")
	waitForAdmission(t, time.Second, controller, func(s admission.Snapshot) bool { return s.Connections == 1 })
	_ = client.ws.CloseNow()
}

func TestShareLeaseSurvivesGraceAndResumeWithoutDoubleCharge(t *testing.T) {
	controller := admissionController(t, nil)
	const grace = 400 * time.Millisecond
	ts, _ := startRelay(t, signaling.Config{
		Admission:            controller,
		SenderReconnectGrace: grace,
	}, sourceAPI())
	sealed := randomManifest(t, 128)
	sender := registerAs(t, ts.URL, shareA, "source-a", testToken, sealed)
	if snapshot := controller.Snapshot(); snapshot.Shares != 1 || snapshot.ManifestBytes != int64(len(sealed)) {
		t.Fatalf("registered snapshot = %+v", snapshot)
	}

	_ = sender.ws.CloseNow()
	waitForAdmission(t, time.Second, controller, func(s admission.Snapshot) bool { return s.Connections == 0 })
	if snapshot := controller.Snapshot(); snapshot.Shares != 1 {
		t.Fatalf("grace released share lease early: %+v", snapshot)
	}

	resumed := dialAs(t, ts.URL, shareA, "source-b")
	resumed.send(protocol.NewResumeRegister(shareA, protocol.HashResumeToken(testToken), protocol.EncodeResumeToken(testToken)))
	resumed.sendRaw(websocket.MessageBinary, protocol.EncodeManifestFrame(sealed))
	if _, ok := resumed.readMsg().(*protocol.Registered); !ok {
		t.Fatal("resume was rejected")
	}
	if snapshot := controller.Snapshot(); snapshot.Shares != 1 || snapshot.Sources != 1 {
		t.Fatalf("resume double-charged or transferred lease: %+v", snapshot)
	}

	_ = resumed.ws.CloseNow()
	waitForAdmission(t, 3*grace, controller, func(s admission.Snapshot) bool {
		return s.Connections == 0 && s.Shares == 0 && s.ManifestBytes == 0
	})
}

func TestResumeDoesNotNeedDuplicateManifestCapacity(t *testing.T) {
	const manifestSize = 128
	controller := admissionController(t, func(cfg *admission.Config) {
		cfg.MaxConnections = 2
		cfg.MaxConcurrentShares = 1
		cfg.MaxSharesPerSource = 1
		cfg.MaxManifestBytesPerSource = manifestSize
		cfg.MaxTotalManifestBytes = manifestSize
	})
	const grace = 400 * time.Millisecond
	ts, _ := startRelay(t, signaling.Config{
		Admission:            controller,
		SenderReconnectGrace: grace,
	}, sourceAPI())
	sealed := randomManifest(t, manifestSize)
	sender := registerAs(t, ts.URL, shareA, "source-a", testToken, sealed)
	_ = sender.ws.CloseNow()
	waitForAdmission(t, time.Second, controller, func(s admission.Snapshot) bool { return s.Connections == 0 })

	resumed := dialAs(t, ts.URL, shareA, "source-b")
	resumed.send(protocol.NewResumeRegister(shareA, protocol.HashResumeToken(testToken), protocol.EncodeResumeToken(testToken)))
	resumed.sendRaw(websocket.MessageBinary, protocol.EncodeManifestFrame(sealed))
	if _, ok := resumed.readMsg().(*protocol.Registered); !ok {
		t.Fatal("resume was rejected while the retained manifest filled the global budget")
	}
	if snapshot := controller.Snapshot(); snapshot.Shares != 1 || snapshot.ManifestBytes != manifestSize ||
		snapshot.PendingShares != 0 || snapshot.PendingManifestBytes != 0 {
		t.Fatalf("resume duplicated capacity: %+v", snapshot)
	}
	_ = resumed.ws.CloseNow()
}

func TestResumeUploadDoesNotExtendGraceBeforeCommit(t *testing.T) {
	controller := admissionController(t, nil)
	const grace = 60 * time.Millisecond
	ts, _ := startRelay(t, signaling.Config{
		Admission:            controller,
		SenderReconnectGrace: grace,
		RoleTimeout:          time.Second,
	}, sourceAPI())
	sealed := randomManifest(t, 64)
	sender := registerAs(t, ts.URL, shareA, "source-a", testToken, sealed)
	_ = sender.ws.CloseNow()
	waitForAdmission(t, time.Second, controller, func(s admission.Snapshot) bool { return s.Connections == 0 })

	resumed := dialAs(t, ts.URL, shareA, "source-a")
	resumed.send(protocol.NewResumeRegister(shareA, protocol.HashResumeToken(testToken), protocol.EncodeResumeToken(testToken)))
	time.Sleep(3 * grace)
	if snapshot := controller.Snapshot(); snapshot.Shares != 1 || snapshot.ManifestBytes != int64(len(sealed)) {
		t.Fatalf("in-flight resume bytes were not pinned after grace expiry: %+v", snapshot)
	}
	resumed.sendRaw(websocket.MessageBinary, protocol.EncodeManifestFrame(sealed))
	resumed.expectError(protocol.ErrCodeResumeRejected)
	waitForAdmission(t, time.Second, controller, func(s admission.Snapshot) bool {
		return s.Shares == 0 && s.ManifestBytes == 0 && s.PendingShares == 0
	})
}

func TestSourceQuotaExhaustionDoesNotDenyAnotherSource(t *testing.T) {
	controller := admissionController(t, func(cfg *admission.Config) {
		cfg.MaxConnections = 8
		cfg.MaxConcurrentShares = 4
		cfg.MaxSharesPerSource = 1
		cfg.MaxManifestBytesPerSource = 256
		cfg.MaxTotalManifestBytes = 1024
	})
	ts, _ := startRelay(t, signaling.Config{Admission: controller}, sourceAPI())
	registerAs(t, ts.URL, shareA, "source-a", testToken, randomManifest(t, 128))

	rejected := dialAs(t, ts.URL, shareB, "source-a")
	rejected.send(protocol.NewRegister(shareB, protocol.HashResumeToken(testToken)))
	rejected.expectError(protocol.ErrCodeManifestBudget)
	rejected.expectClosed()
	if snapshot := controller.Snapshot(); snapshot.Shares != 1 || snapshot.Sources != 1 {
		t.Fatalf("rejection leaked capacity: %+v", snapshot)
	}

	other := registerAs(t, ts.URL, shareB, "source-b", testToken, randomManifest(t, 64))
	if snapshot := controller.Snapshot(); snapshot.Shares != 2 || snapshot.Sources != 2 {
		t.Fatalf("unrelated source was not admitted: %+v", snapshot)
	}
	_ = other.ws.CloseNow()
}

func TestManifestBudgetIsChargedToInjectedSource(t *testing.T) {
	controller := admissionController(t, func(cfg *admission.Config) {
		cfg.MaxConnections = 8
		cfg.MaxConcurrentShares = 5
		cfg.MaxSharesPerSource = 5
		cfg.MaxManifestBytesPerSource = 150
		cfg.MaxTotalManifestBytes = 500
	})
	ts, _ := startRelay(t, signaling.Config{Admission: controller}, sourceAPI())
	registerAs(t, ts.URL, shareA, "source-a", testToken, randomManifest(t, 100))

	rejected := dialAs(t, ts.URL, shareB, "source-a")
	rejected.send(protocol.NewRegister(shareB, protocol.HashResumeToken(testToken)))
	rejected.sendRaw(websocket.MessageBinary, protocol.EncodeManifestFrame(randomManifest(t, 60)))
	rejected.expectError(protocol.ErrCodeManifestBudget)
	if snapshot := controller.Snapshot(); snapshot.Shares != 1 || snapshot.ManifestBytes != 100 {
		t.Fatalf("source budget rejection leaked capacity: %+v", snapshot)
	}

	registerAs(t, ts.URL, shareB, "source-b", testToken, randomManifest(t, 60))
	if snapshot := controller.Snapshot(); snapshot.Shares != 2 || snapshot.ManifestBytes != 160 {
		t.Fatalf("other source did not retain independent budget: %+v", snapshot)
	}
}

func TestRegisterRateIsPerSourceAndDoesNotLeakLease(t *testing.T) {
	controller := admissionController(t, func(cfg *admission.Config) {
		cfg.RegisterPerSource = admission.Rate{PerSecond: 0.0001, Burst: 1}
	})
	ts, _ := startRelay(t, signaling.Config{Admission: controller}, sourceAPI())
	registerAs(t, ts.URL, shareA, "source-a", testToken, randomManifest(t, 64))

	rejected := dialAs(t, ts.URL, shareB, "source-a")
	rejected.send(protocol.NewRegister(shareB, protocol.HashResumeToken(testToken)))
	rejected.expectError(protocol.ErrCodeRateLimited)
	if snapshot := controller.Snapshot(); snapshot.Shares != 1 {
		t.Fatalf("rate rejection leaked share: %+v", snapshot)
	}
	registerAs(t, ts.URL, shareB, "source-b", testToken, randomManifest(t, 64))
}

func TestRegisterFailurePathsPreserveLeaseAccounting(t *testing.T) {
	t.Run("oversize before reservation", func(t *testing.T) {
		controller := admissionController(t, nil)
		ts, _ := startRelay(t, signaling.Config{
			Admission:       controller,
			MaxManifestSize: 32,
		}, sourceAPI())
		client := dialAs(t, ts.URL, shareA, "source-a")
		client.send(protocol.NewRegister(shareA, protocol.HashResumeToken(testToken)))
		client.sendRaw(websocket.MessageBinary, protocol.EncodeManifestFrame(randomManifest(t, 64)))
		client.expectError(protocol.ErrCodeManifestTooLarge)
		client.expectClosed()
		waitForAdmission(t, time.Second, controller, func(s admission.Snapshot) bool { return s.Connections == 0 })
		if snapshot := controller.Snapshot(); snapshot.Shares != 0 || snapshot.ManifestBytes != 0 {
			t.Fatalf("oversize register leaked reservation: %+v", snapshot)
		}
	})

	t.Run("collision retains original reservation", func(t *testing.T) {
		controller := admissionController(t, nil)
		ts, _ := startRelay(t, signaling.Config{Admission: controller}, sourceAPI())
		sealed := randomManifest(t, 64)
		registerAs(t, ts.URL, shareA, "source-a", testToken, sealed)
		collision := dialAs(t, ts.URL, shareA, "source-b")
		collision.send(protocol.NewRegister(shareA, protocol.HashResumeToken(testToken)))
		collision.expectError(protocol.ErrCodeShareIDCollision)
		collision.expectClosed()
		if snapshot := controller.Snapshot(); snapshot.Shares != 1 || snapshot.ManifestBytes != int64(len(sealed)) {
			t.Fatalf("collision changed reservation: %+v", snapshot)
		}
	})

	t.Run("rejected resume and shutdown preserve accounting", func(t *testing.T) {
		controller := admissionController(t, nil)
		const grace = time.Second
		ts, hub := startRelay(t, signaling.Config{
			Admission:            controller,
			SenderReconnectGrace: grace,
		}, sourceAPI())
		sealed := randomManifest(t, 64)
		sender := registerAs(t, ts.URL, shareA, "source-a", testToken, sealed)
		_ = sender.ws.CloseNow()
		waitForAdmission(t, time.Second, controller, func(s admission.Snapshot) bool { return s.Connections == 0 })

		wrongToken := bytes.Repeat([]byte{0x33}, protocol.ResumeTokenBytes)
		resume := dialAs(t, ts.URL, shareA, "source-b")
		resume.send(protocol.NewResumeRegister(shareA, protocol.HashResumeToken(testToken), protocol.EncodeResumeToken(wrongToken)))
		resume.expectError(protocol.ErrCodeResumeRejected)
		resume.expectClosed()
		if snapshot := controller.Snapshot(); snapshot.Shares != 1 || snapshot.ManifestBytes != int64(len(sealed)) {
			t.Fatalf("rejected resume changed reservation: %+v", snapshot)
		}

		hub.Close()
		waitForAdmission(t, time.Second, controller, func(s admission.Snapshot) bool {
			return s.Connections == 0 && s.Shares == 0 && s.ManifestBytes == 0
		})
		if snapshot := controller.Snapshot(); snapshot.Shares != 0 || snapshot.ManifestBytes != 0 || snapshot.Sources != 0 {
			t.Fatalf("shutdown did not release accounting: %+v", snapshot)
		}
	})
}

func TestManifestStepTimeoutRollsBackPendingAdmission(t *testing.T) {
	controller := admissionController(t, nil)
	ts, _ := startRelay(t, signaling.Config{
		Admission:   controller,
		RoleTimeout: 80 * time.Millisecond,
	}, sourceAPI())
	client := dialAs(t, ts.URL, shareA, "source-a")
	client.send(protocol.NewRegister(shareA, protocol.HashResumeToken(testToken)))
	waitForAdmission(t, time.Second, controller, func(s admission.Snapshot) bool {
		return s.PendingShares == 1
	})
	client.expectClosed()
	waitForAdmission(t, time.Second, controller, func(s admission.Snapshot) bool {
		return s.Connections == 0 && s.PendingShares == 0 && s.PendingManifestBytes == 0 && s.Sources == 0
	})
}

func TestStreamingManifestBudgetRollsBackPartialReservation(t *testing.T) {
	const budget = 70 * 1024
	controller := admissionController(t, func(cfg *admission.Config) {
		cfg.MaxConcurrentShares = 2
		cfg.MaxSharesPerSource = 2
		cfg.MaxManifestBytesPerSource = budget
		cfg.MaxTotalManifestBytes = budget
	})
	ts, _ := startRelay(t, signaling.Config{
		Admission:       controller,
		MaxManifestSize: 128 * 1024,
	}, sourceAPI())
	client := dialAs(t, ts.URL, shareA, "source-a")
	client.send(protocol.NewRegister(shareA, protocol.HashResumeToken(testToken)))
	client.sendRaw(websocket.MessageBinary, protocol.EncodeManifestFrame(randomManifest(t, 96*1024)))
	client.expectError(protocol.ErrCodeManifestBudget)
	client.expectClosed()
	waitForAdmission(t, time.Second, controller, func(s admission.Snapshot) bool {
		return s.Connections == 0 && s.PendingShares == 0 && s.PendingManifestBytes == 0 && s.Sources == 0
	})
}

func TestRoleAndKeepaliveTimeoutsAreIndependent(t *testing.T) {
	t.Run("role", func(t *testing.T) {
		ts, _ := startRelay(t, signaling.Config{
			RoleTimeout:      40 * time.Millisecond,
			KeepaliveTimeout: time.Second,
		}, httpapi.Config{})
		idle := dial(t, ts, shareA)
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		if _, _, err := idle.ws.Read(ctx); err == nil {
			t.Fatal("connection without a role stayed open")
		}
	})

	t.Run("keepalive", func(t *testing.T) {
		controller := admissionController(t, nil)
		ts, _ := startRelay(t, signaling.Config{
			Admission:            controller,
			RoleTimeout:          time.Second,
			KeepaliveTimeout:     60 * time.Millisecond,
			SenderReconnectGrace: time.Second,
		}, httpapi.Config{})
		sender := register(t, ts, shareA, testToken, randomManifest(t, 64))
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		if _, _, err := sender.ws.Read(ctx); err == nil {
			t.Fatal("registered connection without keepalive stayed open")
		}
		waitForAdmission(t, time.Second, controller, func(s admission.Snapshot) bool { return s.Connections == 0 })
		if snapshot := controller.Snapshot(); snapshot.Shares != 1 {
			t.Fatalf("inactivity bypassed reconnect grace: %+v", snapshot)
		}
	})
}
