package v2

import "encoding/binary"

type DescriptorUpload struct{ Object []byte }

func (f DescriptorUpload) MarshalBinary() ([]byte, error) {
	return encodeVariable("WS2U", nil, f.Object, DescriptorUploadHeaderBytes)
}

func ParseDescriptorUpload(encoded []byte) (DescriptorUpload, error) {
	object, err := parseVariable(encoded, "WS2U", DescriptorUploadHeaderBytes, 8)
	if err != nil {
		return DescriptorUpload{}, err
	}
	return DescriptorUpload{Object: cloneBytes(object)}, nil
}

type DescriptorDelivery struct {
	RelaySessionID RelaySessionID
	Object         []byte
}

func (f DescriptorDelivery) MarshalBinary() ([]byte, error) {
	if !nonzero(f.RelaySessionID[:]) {
		return nil, ErrIdentity
	}
	return encodeVariable("WS2D", f.RelaySessionID[:], f.Object, DescriptorDeliveryHeaderBytes)
}

func ParseDescriptorDelivery(encoded []byte) (DescriptorDelivery, error) {
	if len(encoded) < DescriptorDeliveryHeaderBytes || !reservedPrefix(encoded, "WS2D") {
		return DescriptorDelivery{}, ErrMalformed
	}
	var frame DescriptorDelivery
	copy(frame.RelaySessionID[:], encoded[8:8+RelaySessionIDBytes])
	if !nonzero(frame.RelaySessionID[:]) {
		return DescriptorDelivery{}, ErrIdentity
	}
	object, err := parseVariable(encoded, "WS2D", DescriptorDeliveryHeaderBytes, 8+RelaySessionIDBytes)
	if err != nil {
		return DescriptorDelivery{}, err
	}
	frame.Object = cloneBytes(object)
	return frame, nil
}

// OpaqueRoute is the only post-join relay envelope. The authenticated
// WebSocket role supplies direction; no inner message discriminator is visible.
type OpaqueRoute struct {
	RelaySessionID RelaySessionID
	Ciphertext     []byte
}

func (f OpaqueRoute) MarshalBinary() ([]byte, error) {
	if !nonzero(f.RelaySessionID[:]) || len(f.Ciphertext) == 0 || len(f.Ciphertext) > MaxOpaqueCiphertextBytes {
		return nil, ErrMalformed
	}
	result := appendReservedPrefix(nil, "WS2O")
	result = append(result, f.RelaySessionID[:]...)
	result = binary.BigEndian.AppendUint32(result, uint32(len(f.Ciphertext)))
	return append(result, f.Ciphertext...), nil
}

func ParseOpaqueRoute(encoded []byte) (OpaqueRoute, error) {
	if len(encoded) < OpaqueRouteHeaderBytes || !reservedPrefix(encoded, "WS2O") {
		return OpaqueRoute{}, ErrMalformed
	}
	var frame OpaqueRoute
	copy(frame.RelaySessionID[:], encoded[8:8+RelaySessionIDBytes])
	length := binary.BigEndian.Uint32(encoded[8+RelaySessionIDBytes : OpaqueRouteHeaderBytes])
	if !nonzero(frame.RelaySessionID[:]) || length == 0 || length > MaxOpaqueCiphertextBytes ||
		uint64(OpaqueRouteHeaderBytes)+uint64(length) != uint64(len(encoded)) {
		return OpaqueRoute{}, ErrMalformed
	}
	frame.Ciphertext = cloneBytes(encoded[OpaqueRouteHeaderBytes:])
	return frame, nil
}

func encodeVariable(magic string, identity, object []byte, headerBytes int) ([]byte, error) {
	if len(object) == 0 || len(object) > MaxDescriptorBytes {
		return nil, ErrMalformed
	}
	result := appendReservedPrefix(nil, magic)
	result = append(result, identity...)
	result = binary.BigEndian.AppendUint32(result, uint32(len(object)))
	if len(result) != headerBytes {
		panic("relay v2: invalid variable-frame header")
	}
	return append(result, object...), nil
}

func parseVariable(encoded []byte, magic string, headerBytes, lengthOffset int) ([]byte, error) {
	if len(encoded) < headerBytes || !reservedPrefix(encoded, magic) {
		return nil, ErrMalformed
	}
	length := binary.BigEndian.Uint32(encoded[lengthOffset:headerBytes])
	if length == 0 || length > MaxDescriptorBytes || uint64(headerBytes)+uint64(length) != uint64(len(encoded)) {
		return nil, ErrMalformed
	}
	return encoded[headerBytes:], nil
}
