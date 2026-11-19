package catalogflow

import (
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"errors"

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
	binding, err := senderobject.NewDescriptorBinding(config.PKHash, config.ShareIDRaw)
	if err != nil {
		return nil, err
	}
	return senderobject.Seal(binding, config.DescriptorKey, config.SenderPrivateKey, config.Nonce, plaintext)
}

// OpenedDescriptor keeps the authenticated sender identity with the data it
// authenticated, so all receiver components can share its checked key.
type OpenedDescriptor struct {
	Descriptor catalog.ShareDescriptor
	Sender     *senderauth.Verifier
}

func OpenDescriptor(object []byte, pkHash, shareIDRaw, descriptorKey []byte) (OpenedDescriptor, error) {
	binding, err := senderobject.NewDescriptorBinding(pkHash, shareIDRaw)
	if err != nil {
		return OpenedDescriptor{}, err
	}
	var opened OpenedDescriptor
	_, err = senderobject.OpenDescriptorBootstrap(binding, descriptorKey, object, func(plaintext []byte) (*senderauth.Verifier, error) {
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

// CatalogPayloadEncoder freezes canonical catalog bytes as part of committing
// the directory. Every recipient receives those same bytes in its own session.
type CatalogPayloadEncoder struct{}

func (CatalogPayloadEncoder) Seal(input catalog.PageCommitInput) (catalog.SealedPageObject, error) {
	payload, err := encodeCatalogPageInput(input)
	if err != nil {
		return catalog.SealedPageObject{}, err
	}
	return catalog.NewSealedPageObject(payload)
}

func (CatalogPayloadEncoder) SealFailure(record catalog.DirectoryFailureRecord) (catalog.SealedFailureObject, error) {
	failure, err := directoryFailureFromCatalogRecord(record)
	if err != nil {
		return catalog.SealedFailureObject{}, err
	}
	payload, err := EncodeDirectoryFailure(failure)
	if err != nil {
		return catalog.SealedFailureObject{}, err
	}
	return catalog.NewSealedFailureObject(payload)
}

func directoryFailureFromCatalogRecord(record catalog.DirectoryFailureRecord) (DirectoryFailure, error) {
	code, err := directoryCodeForFailureKind(record.Kind())
	if err != nil {
		return DirectoryFailure{}, err
	}
	return NewDirectoryFailure(DirectoryFailure{
		ShareInstance: record.ShareInstance(), DirectoryID: record.DirectoryID(), AttemptID: record.AttemptID(),
		Code: code, Retryable: record.Retryable(), RetryAfter: record.RetryAfter(),
	})
}

func directoryCodeForFailureKind(kind catalog.FailureKind) (uint16, error) {
	switch kind {
	case catalog.FailureKindStale:
		return DirectoryCodeStale, nil
	case catalog.FailureKindPermission:
		return DirectoryCodePermission, nil
	case catalog.FailureKindCollision:
		return DirectoryCodeCollision, nil
	case catalog.FailureKindTooWide:
		return DirectoryCodeTooWide, nil
	case catalog.FailureKindBudget:
		return DirectoryCodeBudget, nil
	case catalog.FailureKindPermanentIO:
		return DirectoryCodePermanentIO, nil
	case catalog.FailureKindTransientIO:
		return DirectoryCodeTransientIO, nil
	default:
		return 0, ErrObjectIdentity
	}
}

// CatalogPayloadDecoder checks the canonical record and its request binding
// after the session has authenticated delivery.
type CatalogPayloadDecoder struct{}

func (CatalogPayloadDecoder) Verify(ctx context.Context, share catalog.ShareInstance, request ListRequest, payload []byte) (VerifiedObject, error) {
	if err := ctx.Err(); err != nil {
		return VerifiedObject{}, err
	}
	if share.IsZero() || len(payload) == 0 || len(payload) > catalog.MaxCatalogPageObjectBytes {
		return VerifiedObject{}, ErrObjectIdentity
	}
	var header struct {
		Schema uint64 `cbor:"0,keyasint"`
	}
	if err := catalogWireDec.Unmarshal(payload, &header); err != nil {
		return VerifiedObject{}, errors.Join(ErrWireObject, err)
	}
	switch header.Schema {
	case wireSchemaVersion:
		digest := sha256.Sum256(payload)
		page, err := decodeCatalogPage(payload, catalog.PageCommitterFunc(func(catalog.PageCommitInput) (catalog.PageCommitment, error) {
			return catalog.NewPageCommitment(digest[:])
		}))
		if err != nil {
			return VerifiedObject{}, err
		}
		generation, pinned := request.Generation()
		if page.ShareInstance() != share || page.DirectoryID() != request.DirectoryID() ||
			page.PageIndex() != request.PageIndex() || pinned && page.Generation() != generation {
			return VerifiedObject{}, ErrObjectIdentity
		}
		return VerifiedPage(page), nil
	case directoryFailureSchema:
		if _, pinned := request.Generation(); request.PageIndex() != 0 || pinned {
			return VerifiedObject{}, ErrObjectIdentity
		}
		failure, err := DecodeDirectoryFailure(payload)
		if err != nil {
			return VerifiedObject{}, err
		}
		if failure.ShareInstance != share || failure.DirectoryID != request.DirectoryID() {
			return VerifiedObject{}, ErrObjectIdentity
		}
		return VerifiedFailure(failure), nil
	default:
		return VerifiedObject{}, ErrWireObject
	}
}
