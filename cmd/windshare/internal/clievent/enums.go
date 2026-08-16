package clievent

type Command uint8

const (
	CommandShare Command = iota + 1
	CommandGet
)

func (value Command) Name() (string, bool) {
	switch value {
	case CommandShare:
		return "share", true
	case CommandGet:
		return "get", true
	default:
		return "", false
	}
}

func (value Command) Valid() bool { _, ok := value.Name(); return ok }

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
)

func (value ContentPath) Name() (string, bool) {
	switch value {
	case ContentPathRelay:
		return "relay", true
	case ContentPathDirect:
		return "direct", true
	default:
		return "", false
	}
}

func (value ContentPath) Valid() bool { _, ok := value.Name(); return ok }

type ExitCode uint8

const (
	ExitSuccess ExitCode = iota
	ExitFailure
	ExitUsage
	ExitNetwork
	ExitDrift
)

func (value ExitCode) ProcessCode() (int, bool) {
	if value > ExitDrift {
		return 0, false
	}
	return int(value), true
}

func (value ExitCode) Name() (string, bool) {
	switch value {
	case ExitSuccess:
		return "success", true
	case ExitFailure:
		return "failure", true
	case ExitUsage:
		return "usage", true
	case ExitNetwork:
		return "network", true
	case ExitDrift:
		return "drift", true
	default:
		return "", false
	}
}

func (value ExitCode) Valid() bool { _, ok := value.ProcessCode(); return ok }

type ResultStatus uint8

const (
	ResultSuccess ResultStatus = iota + 1
	ResultPartial
	ResultPaused
	ResultFailed
)

func (value ResultStatus) Name() (string, bool) {
	switch value {
	case ResultSuccess:
		return "success", true
	case ResultPartial:
		return "partial", true
	case ResultPaused:
		return "paused", true
	case ResultFailed:
		return "failed", true
	default:
		return "", false
	}
}

func (value ResultStatus) Valid() bool { _, ok := value.Name(); return ok }

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
