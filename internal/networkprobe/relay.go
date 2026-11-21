package networkprobe

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"net/http"
	"time"

	"github.com/windshare/windshare/core/catalog"
	"github.com/windshare/windshare/core/content"
	"github.com/windshare/windshare/core/framechannel"
	"github.com/windshare/windshare/core/link"
	"github.com/windshare/windshare/core/senderobject"
	v2 "github.com/windshare/windshare/relay/protocol/v2"
	"github.com/windshare/windshare/transport/relayv2"
)

type SyntheticShare struct {
	key        ed25519.PrivateKey
	init       v2.RegisterInit
	descriptor []byte
}

func (s SyntheticShare) ID() v2.ShareID { return s.init.ShareID }
func NewShare() (SyntheticShare, error) {
	pub, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return SyntheticShare{}, err
	}
	var secret [link.ReadSecretBytes]byte
	rand.Read(secret[:])
	identity, err := link.NewSenderAuthenticated(secret[:], pub, nil)
	if err != nil {
		return SyntheticShare{}, err
	}
	raw, err := base64.RawURLEncoding.DecodeString(identity.ShareID)
	if err != nil {
		return SyntheticShare{}, err
	}
	id, err := v2.ShareIDFromBytes(raw)
	if err != nil {
		return SyntheticShare{}, err
	}
	var instance v2.ShareInstance
	rand.Read(instance[:])
	tree, err := content.NewKeyTree(secret[:], catalog.ShareInstance(instance))
	if err != nil {
		return SyntheticShare{}, err
	}
	defer tree.Destroy()
	descriptorKey, err := tree.DescriptorKey(identity.PKHash)
	if err != nil {
		return SyntheticShare{}, err
	}
	defer descriptorKey.Destroy()
	var nonce [senderobject.NonceBytes]byte
	rand.Read(nonce[:])
	descriptor, err := senderobject.SealDescriptor(identity.PKHash, id[:], descriptorKey.Bytes(), key, nonce[:], []byte{0xa1, 0, 1})
	if err != nil {
		return SyntheticShare{}, err
	}
	var token v2.ResumeToken
	rand.Read(token[:])
	init, err := relayv2.NewFreshRegisterInit(id, instance, v2.PKHash(identity.PKHash), descriptor, token)
	return SyntheticShare{key, init, descriptor}, err
}

func (s SyntheticShare) exchange(ctx context.Context, client *http.Client, base, origin, ticket string) (result error) {
	dial := relayv2.DialOptions{HTTPClient: client, Header: http.Header{"Origin": {origin}, "Authorization": {"Bearer " + ticket}}}
	sender, err := relayv2.DialSender(ctx, relayv2.SenderConfig{RelayBaseURL: base, Init: s.init, SenderPrivateKey: s.key, Descriptor: s.descriptor, Dial: dial})
	if err != nil {
		return err
	}
	defer func() {
		_ = sender.Close()
		cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), 3*time.Second)
		defer cancel()
		var stop v2.StopID
		rand.Read(stop[:])
		result = errors.Join(result, relayv2.Stop(cleanup, relayv2.StopConfig{RelayBaseURL: base, ShareID: s.init.ShareID, ShareInstance: s.init.ShareInstance,
			PKHash: s.init.PKHash, StopID: stop, SenderPrivateKey: s.key, Dial: relayv2.DialOptions{HTTPClient: client, Header: http.Header{"Origin": {origin}}}}))
	}()
	dial.Header = http.Header{"Origin": {origin}}
	receiver, err := relayv2.DialReceiver(ctx, relayv2.ReceiverConfig{RelayBaseURL: base, ShareID: s.init.ShareID, Dial: dial})
	if err != nil {
		return err
	}
	defer receiver.Close()
	if !bytes.Equal(receiver.Descriptor(), s.descriptor) {
		return errors.New("synthetic descriptor changed")
	}
	var challenge [32]byte
	rand.Read(challenge[:])
	if err = receiver.Channel().Send(ctx, challenge[:]); err != nil {
		return err
	}
	channel, err := sender.Accept(ctx)
	if err != nil {
		return err
	}
	if err = receive(ctx, channel.Recv(), challenge[:]); err != nil {
		return err
	}
	if err = channel.Send(ctx, challenge[:]); err != nil {
		return err
	}
	return receive(ctx, receiver.Channel().Recv(), challenge[:])
}

func receive(ctx context.Context, frames <-chan framechannel.Frame, want []byte) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	case frame := <-frames:
		if !bytes.Equal(frame, want) {
			return errors.New("synthetic relay transfer mismatch")
		}
		return nil
	}
}
