package protocolsession

import (
	"bytes"
	"errors"
	"testing"
)

func TestEnvelopeCryptoLifecycleZeroesAccessibleStateAndRejectsUse(t *testing.T) {
	_, key, binding := loadEnvelopeVector(t, "sender-signed-operation-error")
	sealer, err := NewEnvelopeSealer(
		key,
		binding,
		bytes.NewReader(sequentialBytes(0x31, EnvelopeNonceBytes*2)),
	)
	if err != nil {
		t.Fatal(err)
	}
	first, err := sealer.Seal([]byte("first"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := sealer.NextSequence(); err != nil {
		t.Fatal(err)
	}
	opener, err := NewEnvelopeOpener(key, binding)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := opener.Open(first.Frame); err != nil {
		t.Fatal(err)
	}

	sealer.Stop()
	opener.Stop()
	if _, err := sealer.NextSequence(); !errors.Is(err, ErrEnvelopeClosed) {
		t.Fatalf("stopped sealer sequence error = %v", err)
	}
	if _, err := sealer.Seal(nil); !errors.Is(err, ErrEnvelopeClosed) {
		t.Fatalf("stopped sealer Seal error = %v", err)
	}
	if _, err := opener.NextSequence(); !errors.Is(err, ErrEnvelopeClosed) {
		t.Fatalf("stopped opener sequence error = %v", err)
	}
	if _, err := opener.Open(first.Frame); !errors.Is(err, ErrEnvelopeClosed) {
		t.Fatalf("stopped opener Open error = %v", err)
	}

	sealer.Destroy()
	opener.Destroy()
	if sealer.aead != nil || sealer.binding != (EnvelopeBinding{}) || sealer.nonceSource != nil ||
		sealer.nonce != ([EnvelopeNonceBytes]byte{}) || sealer.nonceReady ||
		sealer.next != 0 || sealer.exhausted || !sealer.stopped.Load() {
		t.Fatalf("destroyed sealer retained state: %+v", sealer)
	}
	if opener.aead != nil || opener.binding != (EnvelopeBinding{}) ||
		opener.next != 0 || opener.exhausted || !opener.stopped.Load() {
		t.Fatalf("destroyed opener retained state: %+v", opener)
	}

	sealer.Close()
	opener.Close()
	(*EnvelopeSealer)(nil).Stop()
	(*EnvelopeSealer)(nil).Destroy()
	(*EnvelopeSealer)(nil).Close()
	(*EnvelopeOpener)(nil).Stop()
	(*EnvelopeOpener)(nil).Destroy()
	(*EnvelopeOpener)(nil).Close()
}

type stopFromNonceSource struct {
	sealer *EnvelopeSealer
	nonce  []byte
}

func (source *stopFromNonceSource) Read(destination []byte) (int, error) {
	copied := copy(destination, source.nonce)
	source.nonce = source.nonce[copied:]
	source.sealer.Stop()
	return copied, nil
}

func TestEnvelopeSealerStopFromNonceCallbackDoesNotRetainNonce(t *testing.T) {
	_, key, binding := loadEnvelopeVector(t, "sender-signed-operation-error")
	source := &stopFromNonceSource{nonce: sequentialBytes(0x51, EnvelopeNonceBytes)}
	sealer, err := NewEnvelopeSealer(key, binding, source)
	if err != nil {
		t.Fatal(err)
	}
	source.sealer = sealer

	if _, err := sealer.NextSequence(); !errors.Is(err, ErrEnvelopeClosed) {
		t.Fatalf("self-stopped nonce acquisition error = %v", err)
	}
	if sealer.nonce != ([EnvelopeNonceBytes]byte{}) || sealer.nonceReady || sealer.next != 0 {
		t.Fatalf("self-stopped sealer retained nonce/sequence: %+v", sealer)
	}
	sealer.Destroy()
}
