package observation

type Level uint8

const (
	LevelDebug Level = iota + 1
	LevelInfo
	LevelWarning
	LevelError
)

func (value Level) Name() (string, bool) {
	switch value {
	case LevelDebug:
		return "debug", true
	case LevelInfo:
		return "info", true
	case LevelWarning:
		return "warning", true
	case LevelError:
		return "error", true
	default:
		return "", false
	}
}

type Transport uint8

const (
	TransportRelay Transport = iota + 1
	TransportWebRTC
)

func (value Transport) Name() (string, bool) {
	switch value {
	case TransportRelay:
		return "relay", true
	case TransportWebRTC:
		return "webrtc", true
	default:
		return "", false
	}
}

func (value Transport) Valid() bool { _, ok := value.Name(); return ok }

type ContentPath uint8

const (
	ContentPathRelay ContentPath = iota + 1
	ContentPathDirect
	ContentPathDirectAndRelay
)

func (value ContentPath) Name() (string, bool) {
	switch value {
	case ContentPathRelay:
		return "relay", true
	case ContentPathDirect:
		return "direct", true
	case ContentPathDirectAndRelay:
		return "direct_and_relay", true
	default:
		return "", false
	}
}

func (value ContentPath) Valid() bool { _, ok := value.Name(); return ok }

type DiscoveryStatus uint8

const (
	DiscoveryOpen DiscoveryStatus = iota + 1
	DiscoveryComplete
	DiscoveryFailed
)

func (value DiscoveryStatus) Name() (string, bool) {
	switch value {
	case DiscoveryOpen:
		return "open", true
	case DiscoveryComplete:
		return "complete", true
	case DiscoveryFailed:
		return "failed", true
	default:
		return "", false
	}
}

func (value DiscoveryStatus) Valid() bool { _, ok := value.Name(); return ok }

type SendDisposition uint8

const (
	SendAccepted SendDisposition = iota + 1
	SendRejected
	SendRetired
)

func (value SendDisposition) Name() (string, bool) {
	switch value {
	case SendAccepted:
		return "accepted", true
	case SendRejected:
		return "rejected", true
	case SendRetired:
		return "retired", true
	default:
		return "", false
	}
}

type ChannelState uint8

const (
	ChannelConnecting ChannelState = iota + 1
	ChannelOpen
	ChannelClosed
)

func (value ChannelState) Name() (string, bool) {
	switch value {
	case ChannelConnecting:
		return "connecting", true
	case ChannelOpen:
		return "open", true
	case ChannelClosed:
		return "closed", true
	default:
		return "", false
	}
}
