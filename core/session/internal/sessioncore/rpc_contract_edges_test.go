package sessioncore

import (
	"bytes"
	"context"
	"errors"
	"github.com/windshare/windshare/core/session"
	"github.com/windshare/windshare/core/session/contentflow"
	"github.com/windshare/windshare/core/session/protocolsession"
	"io"
	"testing"
)

func TestRPCClientBoundsLifecycleAndCancellation(t *testing.T) {
	runtime, _ := newUnstartedRuntime(t, protocolsession.RoleReceiver)
	runtime.start()
	defer runtime.close()
	receiver := struct {
		*Runtime
		rpc *Requests
	}{runtime, NewRequests(runtime, &deterministicReader{next: 17})}
	if err := receiver.rpc.register(runtime.router); err != nil {
		t.Fatal(err)
	}
	defer receiver.rpc.Close()
	body, _ := protocolsession.EncodeBody(map[uint64]any{0: uint64(1)})
	terminal, _ := protocolsession.NewMessage(protocolsession.MessageSessionTerminal, nil, body)
	if err := receiver.rpc.HandleMessage(context.Background(), terminal); !errors.Is(err, session.ErrOperationMissing) {
		t.Fatalf("message without operation error = %v", err)
	}
	missingID := id16[protocolsession.OperationID](71)
	missing, _ := protocolsession.NewMessage(protocolsession.MessageCatalogResult, &missingID, body)
	if err := receiver.rpc.HandleMessage(context.Background(), missing); err != nil {
		t.Fatalf("late response error = %v", err)
	}
	bounded := &Call{id: missingID, messages: make(chan operationResponse, 1)}
	seedRequest, _ := protocolsession.NewMessage(protocolsession.MessageListChildren, &missingID, body)
	requestAdmission, err := receiver.router.AdmitOutbound(seedRequest, protocolsession.OutboundOperationPermit{})
	if err != nil || !bounded.setAuthority(requestAdmission.Generation, requestAdmission.Operation) {
		t.Fatalf("seed bounded call authority: %v", err)
	}
	responseContext := protocolsession.WithOperationGeneration(context.Background(), requestAdmission.Generation)
	receiver.rpc.mu.Lock()
	receiver.rpc.calls[missingID] = bounded
	receiver.rpc.mu.Unlock()
	if err := receiver.rpc.HandleMessage(responseContext, missing); err != nil {
		t.Fatal(err)
	}
	if err := receiver.rpc.HandleMessage(responseContext, missing); !errors.Is(err, session.ErrOperationOverflow) {
		t.Fatalf("response overflow error = %v", err)
	}
	wrong := &Call{id: missingID, messages: make(chan operationResponse, 1)}
	receiver.rpc.End(wrong)
	receiver.rpc.End(nil)
	receiver.rpc.End(bounded)

	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := receiver.rpc.Begin(cancelled, protocolsession.MessageListChildren, body); !errors.Is(err, context.Canceled) {
		t.Fatalf("begin cancelled error = %v", err)
	}
	errorIDs := NewRequests(runtime, edgeErrorReader{})
	if _, err := errorIDs.Begin(context.Background(), protocolsession.MessageListChildren, body); !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("operation ID source error = %v", err)
	}
	zeroIDs := NewRequests(runtime, bytes.NewReader(make([]byte, protocolsession.IdentityBytes*4)))
	if _, err := zeroIDs.Begin(context.Background(), protocolsession.MessageListChildren, body); !errors.Is(err, session.ErrRuntimeConfig) {
		t.Fatalf("zero operation IDs error = %v", err)
	}
	invalidMessage := NewRequests(runtime, bytes.NewReader(bytes.Repeat([]byte{7}, protocolsession.IdentityBytes)))
	if _, err := invalidMessage.Begin(context.Background(), protocolsession.MessageKind(255), body); err == nil {
		t.Fatal("invalid operation kind was accepted")
	}
	duplicate := NewRequests(runtime, bytes.NewReader(bytes.Repeat([]byte{8}, protocolsession.IdentityBytes)))
	duplicateID := issuedOperationID(8, 1)
	duplicate.calls[duplicateID] = &Call{id: duplicateID}
	if _, err := duplicate.Begin(context.Background(), protocolsession.MessageListChildren, body); !errors.Is(err, session.ErrOperationMissing) {
		t.Fatalf("duplicate local operation error = %v", err)
	}
	if err := receiver.rpc.register(receiver.router); err == nil {
		t.Fatal("duplicate router registration was accepted")
	}
	if err := receiver.rpc.admitCancellation(nil, contentflow.CancelReasonUser); !errors.Is(err, session.ErrOperationMissing) {
		t.Fatalf("nil cancellation call error = %v", err)
	}
	if err := receiver.rpc.admitCancellation(
		&Call{id: missingID}, contentflow.CancelReason(0),
	); !errors.Is(err, session.ErrOperationMissing) {
		t.Fatalf("missing cancellation authority error = %v", err)
	}

	requestBody := body
	call, err := receiver.rpc.Begin(context.Background(), protocolsession.MessageListChildren, requestBody)
	if err != nil {
		t.Fatal(err)
	}

	waitContext, cancelWait := context.WithCancel(context.Background())
	cancelWait()
	if _, err := receiver.rpc.Await(waitContext, call); !errors.Is(err, context.Canceled) {
		t.Fatalf("await cancellation = %v", err)
	}
	receiver.rpc.End(call)

	doneCall := &Call{messages: make(chan operationResponse)}
	runtime.close()
	if _, err := receiver.rpc.Await(context.Background(), doneCall); !errors.Is(err, session.ErrRuntimeClosed) {
		t.Fatalf("await closed runtime = %v", err)
	}
}

type edgeErrorReader struct{}

func (edgeErrorReader) Read([]byte) (int, error) { return 0, io.ErrUnexpectedEOF }
