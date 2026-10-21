package turncontrol

// Quality is a per-incarnation content measurement. Report remains the
// cumulative accounting observation across incarnations; neither is a command
// authorizing retirement. BusyMilliseconds excludes intervals without work.
type Quality struct {
	Authority         Header
	PathID            [IdentityBytes]byte
	Lane              Lane
	NetworkGeneration [IdentityBytes]byte
	SampleSequence    uint64
	ContentBytes      uint64
	BusyMilliseconds  uint64
	PendingBytes      uint64
}

func (m Quality) Header() Header { return m.Authority }
func (Quality) turnMessage()     {}

func validQuality(value Quality) bool {
	return value.PathID != ([IdentityBytes]byte{}) && value.Lane.Valid() && value.SampleSequence != 0
}
