//go:build aix || darwin || dragonfly || freebsd || illumos || linux || netbsd || openbsd || solaris

package e2e

import (
	"context"
	"errors"
	"os"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/windshare/windshare/core/link"
	"github.com/windshare/windshare/core/session"
	"github.com/windshare/windshare/relay/protocol"
	"github.com/windshare/windshare/transport/relay"
)

var receiverConnectedRE = regexp.MustCompile(`receiver connected \(session ([^)]+)\)`)

func TestShareSIGINTStopsServingAndReapsProcesses(t *testing.T) {
	t.Parallel()
	relayURL, relayProc := startRelay(t)
	source := writeTree(t, treeSpec{files: map[string][]byte{"payload.txt": []byte("signal lifecycle")}})
	shareProc := startShare(t, relayURL, []string{source})

	shareURL := shareProc.waitLine(t, "Link: ", procIOTimeout)
	shareLink, err := link.Parse(shareURL)
	if err != nil {
		t.Fatalf("parse share link: %v", err)
	}
	joinCtx, cancelJoin := context.WithTimeout(context.Background(), procIOTimeout)
	defer cancelJoin()
	receiver, err := relay.DialReceiver(joinCtx, relay.ReceiverConfig{
		RelayURL: relayURL,
		ShareID:  shareLink.ShareID,
	})
	if err != nil {
		t.Fatalf("join share before interrupt: %v", err)
	}
	t.Cleanup(func() { _ = receiver.Close() })
	request, err := session.EncodeRequest([]uint64{0})
	if err != nil {
		t.Fatalf("encode receiver request: %v", err)
	}
	if err := receiver.Channel().Send(joinCtx, request); err != nil {
		t.Fatalf("start receiver session: %v", err)
	}

	// The sender-side session event is the readiness barrier: SIGINT is delivered
	// only after the real CLI has accepted this receiver and started serving it.
	sessionID := waitForSubmatch(t, shareProc.stderr, receiverConnectedRE, procIOTimeout, "share receiver session")
	if sessionID != receiver.SessionID().String() {
		t.Fatalf("share accepted session %s, want receiver session %s", sessionID, receiver.SessionID())
	}
	if err := shareProc.cmd.Process.Signal(os.Interrupt); err != nil {
		t.Fatalf("send SIGINT to share process: %v", err)
	}
	if code := shareProc.waitExit(t, procIOTimeout); code != 0 {
		t.Fatalf("SIGINT share exit code = %d, want 0; stderr=%s", code, shareProc.stderr.String())
	}
	if !strings.Contains(shareProc.stderr.String(), "interrupt received; stopping share") {
		t.Fatalf("share did not report its SIGINT shutdown; stderr=%s", shareProc.stderr.String())
	}

	select {
	case <-receiver.Done():
		receiverErr, ok := errors.AsType[*relay.ServerError](receiver.Err())
		if !ok || receiverErr.Code != protocol.ErrCodeSenderGone {
			t.Fatalf("receiver terminal error = %v, want %s", receiver.Err(), protocol.ErrCodeSenderGone)
		}
	case <-time.After(procIOTimeout):
		t.Fatalf("receiver remained connected after share SIGINT; share stderr=%s", shareProc.stderr.String())
	}
	if ended := "session " + sessionID + " ended"; !strings.Contains(shareProc.stderr.String(), ended) {
		t.Fatalf("share did not reap its receiver session %s; stderr=%s", sessionID, shareProc.stderr.String())
	}

	// The relay has no graceful test-control endpoint. Kill still waits for Cmd.Wait,
	// so both process handles below prove that every child has been reaped.
	relayProc.kill()
	if !shareProc.exited() || !relayProc.exited() {
		t.Fatalf("child process was not reaped: share=%t relay=%t", shareProc.exited(), relayProc.exited())
	}
}
