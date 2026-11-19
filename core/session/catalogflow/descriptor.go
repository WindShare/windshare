package catalogflow

import (
	"crypto/ed25519"

	"github.com/windshare/windshare/core/catalog"
	"github.com/windshare/windshare/core/senderauth"
	"github.com/windshare/windshare/core/senderobject"
)

type DescriptorObjectConfig struct {
	PKHash           []byte
	ShareIDRaw       []byte
	DescriptorKey    []byte
	SenderPrivateKey ed25519.PrivateKey
	Nonce            []byte
}

func SealDescriptor(descriptor catalog.ShareDescriptor, config DescriptorObjectConfig) ([]byte, error) {
	plaintext, err := encodeShareDescriptor(descriptor)
	if err != nil {
		return nil, err
	}
	return senderobject.SealDescriptor(config.PKHash, config.ShareIDRaw, config.DescriptorKey, config.SenderPrivateKey, config.Nonce, plaintext)
}

// OpenedDescriptor keeps the authenticated sender identity with the data it
// authenticated, so all receiver components can share its checked key.
type OpenedDescriptor struct {
	Descriptor catalog.ShareDescriptor
	Sender     *senderauth.Verifier
}

func OpenDescriptor(object []byte, pkHash, shareIDRaw, descriptorKey []byte) (OpenedDescriptor, error) {
	var opened OpenedDescriptor
	_, err := senderobject.OpenDescriptorBootstrap(pkHash, shareIDRaw, descriptorKey, object, func(plaintext []byte) (*senderauth.Verifier, error) {
		descriptor, decodeErr := decodeShareDescriptor(plaintext)
		if decodeErr != nil {
			return nil, decodeErr
		}
		sender, keyErr := senderauth.NewVerifier(ed25519.PublicKey(descriptor.SenderPublicKey()))
		if keyErr != nil {
			return nil, keyErr
		}
		opened = OpenedDescriptor{Descriptor: descriptor, Sender: sender}
		return sender, nil
	})
	if err != nil {
		return OpenedDescriptor{}, err
	}
	return opened, nil
}
