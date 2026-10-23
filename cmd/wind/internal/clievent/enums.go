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
