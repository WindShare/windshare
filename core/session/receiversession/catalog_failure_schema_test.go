package receiversession

import (
	"bytes"
	"context"
	"github.com/windshare/windshare/core/session/catalogflow"
	"github.com/windshare/windshare/core/session/protocolsession"
	"testing"
)

func TestMalformedDirectoryRequestUsesSharedFailureSchema(t *testing.T) {
	body, err := protocolsession.EncodeBody(map[uint64]any{0: uint64(1)})
	if err != nil {
		t.Fatal(err)
	}
	fixture := newVerticalFixture(t)
	sender, receiver := connectVerticalPair(t, fixture.senderFactory, fixture.receiverFactory)
	defer sender.Close()
	defer receiver.Close()
	call, err := receiver.rpc.Begin(context.Background(), protocolsession.MessageListChildren, body)
	if err != nil {
		t.Fatal(err)
	}
	defer receiver.rpc.End(call)
	message, err := receiver.rpc.Await(context.Background(), call)
	if err != nil {
		t.Fatal(err)
	}
	if message.Kind() != protocolsession.MessageOperationError {
		t.Fatalf("malformed catalog response kind = %d", message.Kind())
	}
	unsigned, err := protocolsession.SenderControlSemanticBody(message)
	if err != nil {
		t.Fatal(err)
	}
	want, err := protocolsession.EncodeOperationFailure(protocolsession.OperationFailure{
		Scope: protocolsession.OperationScopeDirectory,
		Code:  catalogflow.DirectoryCodePermanentIO, Message: "Catalog operation failed",
	})
	if err != nil || !bytes.Equal(unsigned, want) {
		t.Fatalf("directory operation failure = %x, want %x, err %v", unsigned, want, err)
	}
}
