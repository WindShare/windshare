package v2

const (
	DeliveryConsumedMagic = "WS2B"
	DeliveryConsumedBytes = deliveryTotalsBytes
)

// DeliveryConsumed reports a cumulative prefix of complete WS2O envelopes taken
// by the destination application. The authenticated connection supplies its
// generation and receive direction; counters never reset within that flow.
type DeliveryConsumed deliveryTotals

func (f DeliveryConsumed) MarshalBinary() ([]byte, error) {
	return deliveryTotals(f).marshal(DeliveryConsumedMagic)
}

func ParseDeliveryConsumed(encoded []byte) (DeliveryConsumed, error) {
	frame, err := parseDeliveryTotals(encoded, DeliveryConsumedMagic)
	return DeliveryConsumed(frame), err
}
