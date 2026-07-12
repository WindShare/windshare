package webrtc

const (
	terminalIntentControl = "terminal-intent"
	terminalAckControl    = "terminal-ack"
	maximumControlBytes   = len(terminalIntentControl)
)

type controlKind uint8

const (
	controlInvalid controlKind = iota
	controlTerminalIntent
	controlTerminalAck
)

func parseControl(data []byte) controlKind {
	switch string(data) {
	case terminalIntentControl:
		return controlTerminalIntent
	case terminalAckControl:
		return controlTerminalAck
	default:
		return controlInvalid
	}
}
