package protocolsession

import (
	"errors"
	"fmt"
)

type SenderControlSemanticValidator interface {
	ValidateSenderControl(MessageKind, OperationID, []byte) error
}

type SenderControlSemanticValidatorFunc func(MessageKind, OperationID, []byte) error

func (validate SenderControlSemanticValidatorFunc) ValidateSenderControl(
	kind MessageKind,
	operationID OperationID,
	semantic []byte,
) error {
	if validate == nil {
		return ErrControlSemantic
	}
	return validate(kind, operationID, semantic)
}

// SenderControlSemanticRule binds one signed sender control kind to its typed
// decoder. Keeping this registry at the authentication boundary prevents a new
// final kind from silently bypassing semantic validation before routing.
type SenderControlSemanticRule struct {
	Kind     MessageKind
	Validate SenderControlSemanticValidatorFunc
}

type SenderControlSemanticRegistry struct {
	validators map[MessageKind]SenderControlSemanticValidatorFunc
}

func NewSenderControlSemanticRegistry(
	rules ...SenderControlSemanticRule,
) (*SenderControlSemanticRegistry, error) {
	if len(rules) == 0 {
		return nil, ErrControlSemantic
	}
	validators := make(map[MessageKind]SenderControlSemanticValidatorFunc, len(rules))
	for _, rule := range rules {
		if rule.Validate == nil {
			return nil, ErrControlSemantic
		}
		if _, err := senderControlDomain(rule.Kind); err != nil {
			return nil, errors.Join(ErrControlSemantic, err)
		}
		if _, exists := validators[rule.Kind]; exists {
			return nil, ErrControlSemantic
		}
		validators[rule.Kind] = rule.Validate
	}
	return &SenderControlSemanticRegistry{validators: validators}, nil
}

func (registry *SenderControlSemanticRegistry) ValidateSenderControl(
	kind MessageKind,
	operationID OperationID,
	semantic []byte,
) error {
	if registry == nil {
		return ErrControlSemantic
	}
	validate := registry.validators[kind]
	if validate == nil {
		return ErrControlSemantic
	}
	return validate(kind, operationID, semantic)
}

// Core control schemas are owned here so every authenticator validates them
// even when the composing runtime has no catalog, content, or peer dependency.
func validateSenderControlSemantic(
	external SenderControlSemanticValidator,
	kind MessageKind,
	operationID OperationID,
	semantic []byte,
) error {
	var err error
	switch kind {
	case MessageOperationError:
		_, err = DecodeOperationFailure(semantic)
	case MessageScanProgress:
		_, err = DecodeScanProgress(semantic)
	case MessageSessionTerminal:
		_, err = DecodeSessionTerminal(semantic)
	default:
		if external == nil {
			return ErrControlSemantic
		}
		err = external.ValidateSenderControl(kind, operationID, semantic)
	}
	if err != nil {
		return fmt.Errorf("%w: %w", ErrControlSemantic, err)
	}
	return nil
}
