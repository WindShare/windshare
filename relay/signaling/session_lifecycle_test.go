package signaling

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"sync"
	"testing"
	"time"

	"github.com/windshare/windshare/relay/admission"
	"github.com/windshare/windshare/relay/forward"
	"github.com/windshare/windshare/relay/protocol"
)

type lifecycleWrite struct {
	binary bool
	data   []byte
}

type lifecycleGatedWriter struct {
	entered chan lifecycleWrite
	release chan struct{}
	once    sync.Once
}

func newLifecycleGatedWriter() *lifecycleGatedWriter {
	return &lifecycleGatedWriter{
		entered: make(chan lifecycleWrite, 1),
		release: make(chan struct{}),
	}
}

func (w *lifecycleGatedWriter) write(binary bool, data []byte) error {
	w.entered <- lifecycleWrite{binary: binary, data: bytes.Clone(data)}
	<-w.release
	return nil
}

func (w *lifecycleGatedWriter) WriteText(data []byte) error {
	return w.write(false, data)
}

func (w *lifecycleGatedWriter) WriteBinary(data []byte) error {
	return w.write(true, data)
}

func (w *lifecycleGatedWriter) awaitEntered(t *testing.T) lifecycleWrite {
	t.Helper()
	select {
	case write := <-w.entered:
		return write
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for gated pump write")
		return lifecycleWrite{}
	}
}

func (w *lifecycleGatedWriter) finish() {
	w.once.Do(func() { close(w.release) })
}

type lifecycleRecordingWriter struct {
	writes chan lifecycleWrite
}

func newLifecycleRecordingWriter() *lifecycleRecordingWriter {
	return &lifecycleRecordingWriter{writes: make(chan lifecycleWrite, 4)}
}

func (w *lifecycleRecordingWriter) WriteText(data []byte) error {
	w.writes <- lifecycleWrite{data: bytes.Clone(data)}
	return nil
}

func (w *lifecycleRecordingWriter) WriteBinary(data []byte) error {
	w.writes <- lifecycleWrite{binary: true, data: bytes.Clone(data)}
	return nil
}

type lifecycleDiscardWriter struct{}

func (lifecycleDiscardWriter) WriteText([]byte) error   { return nil }
func (lifecycleDiscardWriter) WriteBinary([]byte) error { return nil }

type lifecyclePumpSpy struct {
	*forward.Pump
	openResults     []forward.EnqueueResult
	terminalResults []forward.EnqueueResult
	openCalls       int
	terminalCalls   int
	closeCalls      int
	connectionCalls int
	onOpen          func()
}

type lifecycleOpenGatePump struct {
	connectionPump
	target  protocol.SessionID
	block   bool
	entered chan struct{}
	release chan struct{}
	once    sync.Once
	unblock sync.Once
	mu      sync.Mutex
	calls   int
}

func (p *lifecycleOpenGatePump) releaseOpen() {
	p.unblock.Do(func() { close(p.release) })
}

func newLifecycleOpenGatePump(pump connectionPump, target protocol.SessionID, block bool) *lifecycleOpenGatePump {
	return &lifecycleOpenGatePump{
		connectionPump: pump,
		target:         target,
		block:          block,
		entered:        make(chan struct{}),
		release:        make(chan struct{}),
	}
}

func (p *lifecycleOpenGatePump) OpenSession(id protocol.SessionID) forward.EnqueueResult {
	if id == p.target {
		p.mu.Lock()
		p.calls++
		p.mu.Unlock()
		p.once.Do(func() { close(p.entered) })
		if p.block {
			<-p.release
		}
	}
	return p.connectionPump.OpenSession(id)
}

func (p *lifecycleOpenGatePump) targetCalls() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.calls
}

func (p *lifecyclePumpSpy) OpenSession(protocol.SessionID) forward.EnqueueResult {
	p.openCalls++
	if p.onOpen != nil {
		p.onOpen()
	}
	return p.takeResult(&p.openResults, forward.Enqueued)
}

func (p *lifecyclePumpSpy) EnqueueSessionTerminal(protocol.SessionID, bool, []byte) (forward.EnqueueResult, <-chan error) {
	p.terminalCalls++
	return p.takeResult(&p.terminalResults, forward.Enqueued), nil
}

func (p *lifecyclePumpSpy) CloseSession(protocol.SessionID) forward.EnqueueResult {
	p.closeCalls++
	return forward.Enqueued
}

func (p *lifecyclePumpSpy) EnqueueConnection(bool, []byte) forward.EnqueueResult {
	p.connectionCalls++
	return forward.Enqueued
}

func (p *lifecyclePumpSpy) takeResult(results *[]forward.EnqueueResult, fallback forward.EnqueueResult) forward.EnqueueResult {
	if len(*results) == 0 {
		return fallback
	}
	result := (*results)[0]
	*results = (*results)[1:]
	return result
}

func closeLifecyclePump(p *forward.Pump) {
	p.Close()
	<-p.Done()
}

func sendLifecycleDetached(h *Hub, c *conn, id protocol.SessionID) bool {
	observation := reserveLifecycleUnknown(h, c, id)
	return h.sendDetachedSessionError(c, id, protocol.ErrCodeUnknownSession, "unknown", observation)
}

func reserveLifecycleUnknown(h *Hub, c *conn, id protocol.SessionID) unknownSessionObservation {
	h.mu.Lock()
	defer h.mu.Unlock()
	return c.unknownSessions.observe(id, maxDetachedSessionErrors)
}

func TestSenderIssuedIDRecognitionOutlivesPumpReceipt(t *testing.T) {
	h := NewHub(Config{})
	var issued senderSessionIDs
	maskSource := bytes.NewReader(make([]byte, protocol.SessionIDBytes))
	slowID, ok := issued.next(maskSource)
	if !ok {
		t.Fatal("issue slow session ID")
	}
	goodID, ok := issued.next(maskSource)
	if !ok {
		t.Fatal("issue healthy session ID")
	}

	senderWriter := newLifecycleGatedWriter()
	senderPump := forward.NewPump(senderWriter, forward.Options{})
	defer func() {
		senderWriter.finish()
		closeLifecyclePump(senderPump)
	}()
	goodWriter := newLifecycleRecordingWriter()
	goodPump := forward.NewPump(goodWriter, forward.Options{})
	defer closeLifecyclePump(goodPump)
	slowWriter := newLifecycleGatedWriter()
	slowPump := forward.NewPump(slowWriter, forward.Options{SessionQueueFrames: 1})
	defer func() {
		slowWriter.finish()
		closeLifecyclePump(slowPump)
	}()

	sender := &conn{
		pump:       senderPump,
		sessionIDs: issued,
	}
	goodReceiver := &conn{pump: goodPump}
	slowReceiver := &conn{pump: slowPump}
	// This white-box test owns the pump directly; suppress the asynchronous WS
	// close that production starts after the same overflow transition.
	slowReceiver.closeOnce.Do(func() {})
	slowSession := &receiverSession{id: slowID, recv: slowReceiver}
	goodSession := &receiverSession{id: goodID, recv: goodReceiver}
	sh := &share{
		id:     shareX,
		sender: sender,
		sessions: map[protocol.SessionID]*receiverSession{
			slowID: slowSession,
			goodID: goodSession,
		},
	}
	h.shares[shareX] = sh
	for _, opened := range []struct {
		pump *forward.Pump
		id   protocol.SessionID
	}{
		{senderPump, slowID},
		{senderPump, goodID},
		{slowPump, slowID},
		{goodPump, goodID},
	} {
		if result := opened.pump.OpenSession(opened.id); result != forward.Enqueued {
			t.Fatalf("open session %s: %v", opened.id, result)
		}
	}

	if result := slowPump.EnqueueForward(slowID, []byte("in-flight")); result != forward.Enqueued {
		t.Fatalf("enqueue in-flight slow frame: %v", result)
	}
	if write := slowWriter.awaitEntered(t); !write.binary {
		t.Fatal("slow forward frame was written as text")
	}
	if result := slowPump.EnqueueForward(slowID, []byte("queued")); result != forward.Enqueued {
		t.Fatalf("enqueue queued slow frame: %v", result)
	}
	overflowFrame := protocol.EncodeForwardFrame(slowID, []byte("overflow"))
	if !h.routeSenderForward(sender, sh, slowID, overflowFrame) {
		t.Fatal("overflow transition closed the sender")
	}
	if write := senderWriter.awaitEntered(t); write.binary {
		t.Fatal("session error terminal was written as binary")
	}

	staleFrame := protocol.EncodeForwardFrame(slowID, []byte("already-in-flight"))
	for range maxDetachedSessionErrors + 1 {
		if !h.routeSenderForward(sender, sh, slowID, staleFrame) {
			t.Fatal("pending-terminal frame closed the sender")
		}
	}
	staleSignal := protocol.NewSignal(slowID.String(), protocol.SignalKindOffer, json.RawMessage(`{}`))
	for range maxDetachedSessionErrors + 1 {
		if !h.relayControlToReceiver(sender, sh, slowID.String(), staleSignal) {
			t.Fatal("pending-terminal signal closed the sender")
		}
	}
	if got := sender.unknownSessions.count(); got != 0 {
		t.Fatalf("terminal frames consumed %d never-known diagnostics", got)
	}

	senderWriter.finish()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	if !senderPump.WaitIdle(ctx) {
		cancel()
		t.Fatal("terminal receipt did not complete")
	}
	cancel()
	for range maxDetachedSessionErrors + 1 {
		if !h.routeSenderForward(sender, sh, slowID, staleFrame) {
			t.Fatal("post-receipt terminal frame closed the sender")
		}
	}
	for range maxDetachedSessionErrors + 1 {
		if !h.relayControlToReceiver(sender, sh, slowID.String(), staleSignal) {
			t.Fatal("post-receipt terminal signal closed the sender")
		}
	}
	if got := sender.unknownSessions.count(); got != 0 {
		t.Fatalf("post-receipt terminal frames consumed %d diagnostics", got)
	}

	goodFrame := protocol.EncodeForwardFrame(goodID, []byte("still-alive"))
	if !h.routeSenderForward(sender, sh, goodID, goodFrame) {
		t.Fatal("healthy session route closed the sender")
	}
	select {
	case write := <-goodWriter.writes:
		if !write.binary || !bytes.Equal(write.data, goodFrame) {
			t.Fatalf("healthy write = binary:%v data:%x", write.binary, write.data)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("healthy session frame was not delivered")
	}
}

func TestFutureDetachedDiagnosticCannotPoisonLaterSession(t *testing.T) {
	tests := []struct {
		name       string
		concurrent bool
	}{
		{name: "diagnostic terminal pending before issuance"},
		{name: "issuance while diagnostic pump open is gated", concurrent: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := NewHub(Config{Rand: bytes.NewReader(make([]byte, protocol.SessionIDBytes))})
			forgedID := lifecycleSessionID(0, 2)

			senderWriter := newLifecycleGatedWriter()
			senderPump := forward.NewPump(senderWriter, forward.Options{})
			defer func() {
				senderWriter.finish()
				closeLifecyclePump(senderPump)
			}()
			openGate := newLifecycleOpenGatePump(senderPump, forgedID, tt.concurrent)
			defer openGate.releaseOpen()
			receiverWriter := newLifecycleRecordingWriter()
			receiverPump := forward.NewPump(receiverWriter, forward.Options{})
			defer closeLifecyclePump(receiverPump)

			sender := &conn{pump: openGate}
			receiver := &conn{pump: receiverPump}
			sh := &share{
				id:       shareX,
				sender:   sender,
				sessions: make(map[protocol.SessionID]*receiverSession),
			}
			h.shares[shareX] = sh

			first := h.openSession(sh, receiver)
			if first == nil || first.id != lifecycleSessionID(0, 1) {
				t.Fatalf("first session = %+v", first)
			}
			h.endSession(sh, first, false)

			forgedFrame := protocol.EncodeForwardFrame(forgedID, []byte("forged-future"))
			var active *receiverSession
			var diagnosticWrite lifecycleWrite
			if tt.concurrent {
				routed := make(chan bool, 1)
				go func() { routed <- h.routeSenderForward(sender, sh, forgedID, forgedFrame) }()
				select {
				case <-openGate.entered:
				case <-time.After(2 * time.Second):
					t.Fatal("detached diagnostic did not reserve before pump open")
				}
				active = h.openSession(sh, receiver)
				openGate.releaseOpen()
				select {
				case ok := <-routed:
					if !ok {
						t.Fatal("forged diagnostic closed the sender")
					}
				case <-time.After(2 * time.Second):
					t.Fatal("detached diagnostic did not finish enqueue")
				}
			} else {
				if !h.routeSenderForward(sender, sh, forgedID, forgedFrame) {
					t.Fatal("forged diagnostic closed the sender")
				}
				diagnosticWrite = senderWriter.awaitEntered(t)
				active = h.openSession(sh, receiver)
			}
			if active == nil {
				t.Fatal("later session was not opened")
			}
			if active.id == forgedID || active.id != lifecycleSessionID(0, 3) {
				t.Fatalf("later session ID = %s, forged = %s", active.id, forgedID)
			}

			if tt.concurrent {
				diagnosticWrite = senderWriter.awaitEntered(t)
			}
			if diagnosticWrite.binary {
				t.Fatal("detached diagnostic was written as binary")
			}
			diagnostic, err := protocol.Decode(diagnosticWrite.data)
			if err != nil {
				t.Fatal(err)
			}
			diagnosticError, ok := diagnostic.(*protocol.Error)
			if !ok || diagnosticError.SessionID != forgedID.String() {
				t.Fatalf("detached diagnostic = %#v", diagnostic)
			}
			signal := protocol.NewSignal(active.id.String(), protocol.SignalKindOffer, json.RawMessage(`{}`))
			if result := sender.sendSessionControl(active.id, signal); result != forward.Enqueued {
				t.Fatalf("sender lane was not usable before publication: %v", result)
			}
			healthyFrame := protocol.EncodeForwardFrame(active.id, []byte("healthy"))
			if !h.routeSenderForward(sender, sh, active.id, healthyFrame) {
				t.Fatal("healthy forward route closed the sender")
			}
			select {
			case write := <-receiverWriter.writes:
				if !write.binary || !bytes.Equal(write.data, healthyFrame) {
					t.Fatalf("healthy receiver write = binary:%v data:%x", write.binary, write.data)
				}
			case <-time.After(2 * time.Second):
				t.Fatal("healthy receiver lane did not route")
			}

			senderWriter.finish()
			controlWrite := senderWriter.awaitEntered(t)
			if controlWrite.binary {
				t.Fatal("healthy control was written as binary")
			}
			decoded, err := protocol.Decode(controlWrite.data)
			if err != nil {
				t.Fatal(err)
			}
			gotSignal, ok := decoded.(*protocol.Signal)
			if !ok || gotSignal.SessionID != active.id.String() {
				t.Fatalf("healthy control = %#v", decoded)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			if !senderPump.WaitIdle(ctx) {
				cancel()
				t.Fatal("sender pump did not drain")
			}
			cancel()

			if !h.routeSenderForward(sender, sh, forgedID, forgedFrame) {
				t.Fatal("retired forged ID closed the sender after receipt")
			}
			if calls := openGate.targetCalls(); calls != 1 {
				t.Fatalf("forged ID pump opens = %d, want 1", calls)
			}
			if got := sender.unknownSessions.count(); got != 1 {
				t.Fatalf("reserved forged IDs = %d, want 1", got)
			}
		})
	}
}

func TestOpenSessionPublishesOnlyAfterBothPumpLanesOpen(t *testing.T) {
	tests := []struct {
		name              string
		senderResult      forward.EnqueueResult
		receiverResult    forward.EnqueueResult
		wantSession       bool
		wantReceiverOpens int
		wantSenderCloses  int
	}{
		{name: "sender pump closed", senderResult: forward.PumpClosed},
		{name: "sender lane terminal", senderResult: forward.SessionTerminated},
		{name: "receiver pump closed", senderResult: forward.Enqueued, receiverResult: forward.PumpClosed, wantReceiverOpens: 1, wantSenderCloses: 1},
		{name: "receiver lane terminal", senderResult: forward.Enqueued, receiverResult: forward.SessionTerminated, wantReceiverOpens: 1, wantSenderCloses: 1},
		{name: "both lanes open", senderResult: forward.Enqueued, receiverResult: forward.Enqueued, wantSession: true, wantReceiverOpens: 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := NewHub(Config{Rand: bytes.NewReader(make([]byte, protocol.SessionIDBytes))})
			senderPump := &lifecyclePumpSpy{openResults: []forward.EnqueueResult{tt.senderResult}}
			receiverPump := &lifecyclePumpSpy{openResults: []forward.EnqueueResult{tt.receiverResult}}
			sender := &conn{pump: senderPump}
			sh := &share{id: shareX, sender: sender, sessions: make(map[protocol.SessionID]*receiverSession)}
			assertUnpublished := func() {
				if len(sh.sessions) != 0 {
					t.Fatalf("session published before both lanes opened: %+v", sh.sessions)
				}
			}
			senderPump.onOpen = assertUnpublished
			receiverPump.onOpen = assertUnpublished
			h.shares[shareX] = sh

			sess := h.openSession(sh, &conn{pump: receiverPump})
			if (sess != nil) != tt.wantSession {
				t.Fatalf("session = %+v, want published %v", sess, tt.wantSession)
			}
			if len(sh.sessions) != 0 && !tt.wantSession {
				t.Fatalf("failed open left Hub residue: %+v", sh.sessions)
			}
			if senderPump.openCalls != 1 || receiverPump.openCalls != tt.wantReceiverOpens {
				t.Fatalf("open calls = sender:%d receiver:%d", senderPump.openCalls, receiverPump.openCalls)
			}
			if senderPump.closeCalls != tt.wantSenderCloses {
				t.Fatalf("sender cleanup calls = %d, want %d", senderPump.closeCalls, tt.wantSenderCloses)
			}
			if sender.sessionIDs.highWater != 1 {
				t.Fatalf("retired high-water mark = %d, want 1", sender.sessionIDs.highWater)
			}
		})
	}
}

func TestDetachedDiagnosticCoalescesByDistinctID(t *testing.T) {
	h := NewHub(Config{})
	writer := newLifecycleGatedWriter()
	pump := forward.NewPump(writer, forward.Options{})
	defer func() {
		writer.finish()
		closeLifecyclePump(pump)
	}()
	c := &conn{pump: pump}
	id := protocol.SessionID{9}
	if !sendLifecycleDetached(h, c, id) {
		t.Fatal("first detached diagnostic closed the connection")
	}
	writer.awaitEntered(t)
	for range maxDetachedSessionErrors + 1 {
		if !sendLifecycleDetached(h, c, id) {
			t.Fatal("repeated detached diagnostic closed the connection")
		}
	}
	if got := c.unknownSessions.count(); got != 1 {
		t.Fatalf("distinct diagnostic count = %d, want 1", got)
	}

	writer.finish()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if !pump.WaitIdle(ctx) {
		t.Fatal("detached diagnostic did not drain")
	}
	if !sendLifecycleDetached(h, c, id) {
		t.Fatal("post-receipt duplicate closed the connection")
	}
	if got := c.unknownSessions.count(); got != 1 {
		t.Fatalf("post-receipt distinct count = %d, want 1", got)
	}
}

func TestSenderSessionIDsRecognizeEntireConnectionLifetime(t *testing.T) {
	var issued senderSessionIDs
	maskSource := bytes.NewReader(make([]byte, protocol.SessionIDBytes))
	const sessions = 64
	ids := make([]protocol.SessionID, 0, sessions)
	for range sessions {
		id, ok := issued.next(maskSource)
		if !ok {
			t.Fatal("session ID issuance failed")
		}
		ids = append(ids, id)
	}
	for i, id := range ids {
		if !issued.recognizes(id) {
			t.Fatalf("issued ID %d was forgotten", i)
		}
	}
	if issued.recognizes(lifecycleSessionID(issued.mask, sessions+1)) {
		t.Fatal("future sequence was classified as issued")
	}
	if issued.highWater != sessions {
		t.Fatalf("constant-size high-water mark = %d, want %d", issued.highWater, sessions)
	}
}

func TestUnknownSessionTrackerIsBoundedAndRollbackable(t *testing.T) {
	var unknown unknownSessionTracker
	first := protocol.SessionID{1}
	if got := unknown.observe(first, maxDetachedSessionErrors); got != unknownSessionFirst {
		t.Fatalf("first observation = %v", got)
	}
	if got := unknown.observe(first, maxDetachedSessionErrors); got != unknownSessionRepeated {
		t.Fatalf("repeated observation = %v", got)
	}
	for i := 2; i <= maxDetachedSessionErrors; i++ {
		if got := unknown.observe(protocol.SessionID{byte(i)}, maxDetachedSessionErrors); got != unknownSessionFirst {
			t.Fatalf("distinct observation %d = %v", i, got)
		}
	}
	if got := unknown.observe(protocol.SessionID{0xff}, maxDetachedSessionErrors); got != unknownSessionLimitExceeded {
		t.Fatalf("overflow observation = %v", got)
	}
	if got := unknown.count(); got != maxDetachedSessionErrors {
		t.Fatalf("bounded distinct IDs = %d", got)
	}
	unknown.rollback(first)
	if got := unknown.count(); got != maxDetachedSessionErrors-1 {
		t.Fatalf("rollback count = %d, want %d", got, maxDetachedSessionErrors-1)
	}
}

func TestDetachedDiagnosticDuplicateNeverTouchesPump(t *testing.T) {
	h := NewHub(Config{})
	id := protocol.SessionID{9}
	spy := &lifecyclePumpSpy{}
	c := &conn{pump: spy}
	if got := reserveLifecycleUnknown(h, c, id); got != unknownSessionFirst {
		t.Fatalf("preseed observation = %v", got)
	}
	for range 100 {
		if !sendLifecycleDetached(h, c, id) {
			t.Fatal("duplicate closed the connection")
		}
	}
	if spy.openCalls != 0 || spy.terminalCalls != 0 || spy.closeCalls != 0 {
		t.Fatalf("duplicate pump calls = open:%d terminal:%d close:%d", spy.openCalls, spy.terminalCalls, spy.closeCalls)
	}
}

func TestDetachedDiagnosticFirstObservationEmitsExactlyOnce(t *testing.T) {
	h := NewHub(Config{})
	spy := &lifecyclePumpSpy{}
	c := &conn{pump: spy}
	id := protocol.SessionID{10}
	for range 2 {
		if !sendLifecycleDetached(h, c, id) {
			t.Fatal("diagnostic closed the connection")
		}
	}
	if spy.openCalls != 1 || spy.terminalCalls != 1 || spy.closeCalls != 0 {
		t.Fatalf("pump calls = open:%d terminal:%d close:%d", spy.openCalls, spy.terminalCalls, spy.closeCalls)
	}
	if got := c.unknownSessions.count(); got != 1 {
		t.Fatalf("observed IDs = %d, want 1", got)
	}
}

func TestDetachedDiagnosticFailuresRollbackFirstObservation(t *testing.T) {
	tests := []struct {
		name           string
		openResult     forward.EnqueueResult
		terminalResult forward.EnqueueResult
		wantAlive      bool
		wantClose      int
	}{
		{name: "open pump closed", openResult: forward.PumpClosed, terminalResult: forward.Enqueued, wantAlive: false},
		{name: "enqueue lost session", openResult: forward.Enqueued, terminalResult: forward.UnknownSession, wantAlive: true, wantClose: 1},
		{name: "enqueue pump closed", openResult: forward.Enqueued, terminalResult: forward.PumpClosed, wantAlive: false, wantClose: 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := NewHub(Config{})
			spy := &lifecyclePumpSpy{
				openResults:     []forward.EnqueueResult{tt.openResult, forward.Enqueued},
				terminalResults: []forward.EnqueueResult{tt.terminalResult, forward.Enqueued},
			}
			c := &conn{pump: spy}
			id := protocol.SessionID{11}
			if got := sendLifecycleDetached(h, c, id); got != tt.wantAlive {
				t.Fatalf("first result = %v, want %v", got, tt.wantAlive)
			}
			if got := c.unknownSessions.count(); got != 0 {
				t.Fatalf("failed observation retained %d IDs", got)
			}
			if spy.closeCalls != tt.wantClose {
				t.Fatalf("close calls = %d, want %d", spy.closeCalls, tt.wantClose)
			}
			if !sendLifecycleDetached(h, c, id) {
				t.Fatal("retry after rollback did not establish a diagnostic")
			}
			if got := c.unknownSessions.count(); got != 1 {
				t.Fatalf("retry observation count = %d, want 1", got)
			}
		})
	}
}

func TestDetachedDiagnosticExistingTerminalStaysObserved(t *testing.T) {
	h := NewHub(Config{})
	spy := &lifecyclePumpSpy{openResults: []forward.EnqueueResult{forward.SessionTerminated}}
	c := &conn{pump: spy}
	id := protocol.SessionID{12}
	for range 2 {
		if !sendLifecycleDetached(h, c, id) {
			t.Fatal("existing terminal closed the connection")
		}
	}
	if spy.openCalls != 1 || spy.terminalCalls != 0 || c.unknownSessions.count() != 1 {
		t.Fatalf("existing terminal state = open:%d terminal:%d observed:%d", spy.openCalls, spy.terminalCalls, c.unknownSessions.count())
	}
}

func TestDetachedDiagnosticLimitDoesNotOpenSession(t *testing.T) {
	h := NewHub(Config{})
	spy := &lifecyclePumpSpy{}
	c := &conn{pump: spy}
	c.closeOnce.Do(func() {})
	for i := range maxDetachedSessionErrors {
		id := protocol.SessionID{byte(i + 1)}
		if got := reserveLifecycleUnknown(h, c, id); got != unknownSessionFirst {
			t.Fatalf("preseed %d = %v", i, got)
		}
	}
	if sendLifecycleDetached(h, c, protocol.SessionID{0xff}) {
		t.Fatal("limit-exceeded ID kept the connection alive")
	}
	if spy.openCalls != 0 || spy.terminalCalls != 0 || spy.closeCalls != 0 {
		t.Fatalf("limit path touched session pump: open:%d terminal:%d close:%d", spy.openCalls, spy.terminalCalls, spy.closeCalls)
	}
	if spy.connectionCalls != 1 {
		t.Fatalf("fatal connection diagnostic calls = %d, want 1", spy.connectionCalls)
	}
}

func TestIssuedSessionRecognitionSurvivesSequentialChurn(t *testing.T) {
	admissionConfig := admission.DefaultConfig()
	const concurrentConnectionBound = 2
	admissionConfig.MaxConnections = concurrentConnectionBound
	controller, err := admission.NewController(admissionConfig)
	if err != nil {
		t.Fatal(err)
	}
	h := NewHub(Config{
		Admission: controller,
		Rand:      bytes.NewReader(make([]byte, protocol.SessionIDBytes)),
	})

	senderPump := forward.NewPump(lifecycleDiscardWriter{}, forward.Options{})
	defer closeLifecyclePump(senderPump)
	receiverWriter := newLifecycleRecordingWriter()
	receiverPump := forward.NewPump(receiverWriter, forward.Options{})
	defer closeLifecyclePump(receiverPump)
	sender := &conn{pump: senderPump}
	// The test drives the fatal policy directly without a WebSocket close shell.
	sender.closeOnce.Do(func() {})
	receiver := &conn{pump: receiverPump}
	sh := &share{
		id:       shareX,
		sender:   sender,
		sessions: make(map[protocol.SessionID]*receiverSession),
	}
	h.shares[shareX] = sh

	const sequentialSessions = concurrentConnectionBound + 3
	issuedIDs := make([]protocol.SessionID, 0, sequentialSessions)
	var healthy *receiverSession
	for i := range sequentialSessions {
		sess := h.openSession(sh, receiver)
		if sess == nil {
			t.Fatalf("open sequential session %d", i)
		}
		issuedIDs = append(issuedIDs, sess.id)
		if i < sequentialSessions-1 {
			h.endSession(sh, sess, false)
			continue
		}
		healthy = sess
	}
	if sender.sessionIDs.highWater != sequentialSessions {
		t.Fatalf("issued high-water mark = %d, want %d", sender.sessionIDs.highWater, sequentialSessions)
	}

	for _, id := range issuedIDs[:len(issuedIDs)-1] {
		frame := protocol.EncodeForwardFrame(id, []byte("delayed"))
		if !h.routeSenderForward(sender, sh, id, frame) {
			t.Fatalf("delayed issued frame %s closed the sender", id)
		}
	}
	if got := sender.unknownSessions.count(); got != 0 {
		t.Fatalf("delayed issued IDs consumed %d abuse slots", got)
	}

	healthyFrame := protocol.EncodeForwardFrame(healthy.id, []byte("healthy"))
	if !h.routeSenderForward(sender, sh, healthy.id, healthyFrame) {
		t.Fatal("healthy active session closed the sender")
	}
	select {
	case write := <-receiverWriter.writes:
		if !write.binary || !bytes.Equal(write.data, healthyFrame) {
			t.Fatalf("healthy write = binary:%v data:%x", write.binary, write.data)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("healthy session frame was not delivered")
	}

	for i := range maxDetachedSessionErrors + 1 {
		neverIssued := lifecycleSessionID(sender.sessionIDs.mask, uint64(sequentialSessions+i+1))
		alive := h.routeSenderForward(sender, sh, neverIssued, protocol.EncodeForwardFrame(neverIssued, []byte("unknown")))
		if i < maxDetachedSessionErrors && !alive {
			t.Fatalf("never-issued ID %d exhausted policy early", i)
		}
		if i == maxDetachedSessionErrors && alive {
			t.Fatal("distinct never-issued IDs did not trip abuse policy")
		}
	}
	if got := sender.unknownSessions.count(); got != maxDetachedSessionErrors {
		t.Fatalf("never-issued abuse count = %d, want %d", got, maxDetachedSessionErrors)
	}
}

func lifecycleSessionID(mask, sequence uint64) protocol.SessionID {
	var id protocol.SessionID
	binary.BigEndian.PutUint64(id[:], mask^sequence)
	return id
}
