package v2

const (
	SettlementNoticeMagic = "WS2N"
	SettlementNoticeBytes = deliveryTotalsBytes
	SettlementAckMagic    = "WS2S"
	SettlementAckBytes    = deliveryTotalsBytes
)

// SettlementNotice freezes a retired flow's submitted WS2O totals. The relay
// orders it after those frames, after revoking grants and discarding unsent data.
type SettlementNotice deliveryTotals

func (f SettlementNotice) MarshalBinary() ([]byte, error) {
	return deliveryTotals(f).marshal(SettlementNoticeMagic)
}

func ParseSettlementNotice(encoded []byte) (SettlementNotice, error) {
	frame, err := parseDeliveryTotals(encoded, SettlementNoticeMagic)
	return SettlementNotice(frame), err
}

// SettlementAck confirms the notice's exact totals have been consumed or
// discarded. It cannot reopen a session or stand in for ordinary WS2B feedback.
// The authenticated socket supplies the destination generation and direction.
type SettlementAck deliveryTotals

func (f SettlementAck) MarshalBinary() ([]byte, error) {
	return deliveryTotals(f).marshal(SettlementAckMagic)
}

func ParseSettlementAck(encoded []byte) (SettlementAck, error) {
	frame, err := parseDeliveryTotals(encoded, SettlementAckMagic)
	return SettlementAck(frame), err
}
