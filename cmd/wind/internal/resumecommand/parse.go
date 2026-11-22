package resumecommand

import (
	"errors"
	"path/filepath"
	"strconv"

	"github.com/windshare/windshare/cmd/wind/internal/command"
)

// List and discard accept no positional arguments. ParseNoPositionals stops at
// the first positional or "--" so later tokens cannot be reinterpreted as help.
type flagRequestParser struct {
	logger resumeLogger
}

func (parser flagRequestParser) ParseRoot(
	action string,
	args []string,
) (resumeRootRequest, command.ParseOutcome) {
	flags := command.NewFlagSet(action)
	var rootPath resumeRootPathFlag
	flags.Var(&rootPath, "o", "output directory (required)")
	outcome := command.ParseNoPositionals(flags, args)
	if outcome != command.ParseReady {
		if outcome != command.ParseHelp {
			parser.logger.Logf("%s: %s", action, outcome.Diagnostic())
		}
		return resumeRootRequest{}, outcome
	}
	if !rootPath.set {
		parser.logger.Logf("%s: -o is required", action)
		return resumeRootRequest{}, command.ParseMissingOptionValue
	}
	absolute, err := absoluteResumeRoot(rootPath.value)
	if err != nil {
		parser.logger.Logf("%s: output directory is invalid", action)
		return resumeRootRequest{}, command.ParseInvalidOptionValue
	}
	return resumeRootRequest{rootPath: absolute}, command.ParseReady
}

func (parser flagRequestParser) ParseDiscard(args []string) (resumeDiscardRequest, command.ParseOutcome) {
	flags := command.NewFlagSet("resume discard")
	var rootPath resumeRootPathFlag
	flags.Var(&rootPath, "o", "output directory (required)")
	var item resumeItemNumberFlag
	flags.Var(&item, "item", "one current inventory item number (required)")
	outcome := command.ParseNoPositionals(flags, args)
	if outcome != command.ParseReady {
		if outcome != command.ParseHelp {
			parser.logger.Logf("resume discard: %s", outcome.Diagnostic())
		}
		return resumeDiscardRequest{}, outcome
	}
	if !rootPath.set {
		parser.logger.Logf("resume discard: -o is required")
		return resumeDiscardRequest{}, command.ParseMissingOptionValue
	}
	if !item.set {
		parser.logger.Logf("resume discard: --item is required")
		return resumeDiscardRequest{}, command.ParseMissingOptionValue
	}
	absolute, err := absoluteResumeRoot(rootPath.value)
	if err != nil {
		parser.logger.Logf("resume discard: output directory is invalid")
		return resumeDiscardRequest{}, command.ParseInvalidOptionValue
	}
	return resumeDiscardRequest{rootPath: absolute, itemNumber: item.value}, command.ParseReady
}

type resumeRootPathFlag struct {
	set   bool
	value string
}

func (root *resumeRootPathFlag) String() string {
	if root == nil || !root.set {
		return ""
	}
	return root.value
}

func (root *resumeRootPathFlag) Set(value string) error {
	if root == nil || root.set {
		return errors.New("exactly one -o is allowed")
	}
	if value == "" {
		return errors.New("-o must name an output directory")
	}
	root.set = true
	root.value = value
	return nil
}

type resumeItemNumberFlag struct {
	set   bool
	value int
}

func (item *resumeItemNumberFlag) String() string {
	if item == nil || !item.set {
		return ""
	}
	return strconv.Itoa(item.value)
}

func (item *resumeItemNumberFlag) Set(value string) error {
	if item == nil || item.set {
		return errors.New("exactly one --item is allowed")
	}
	// IntSize-1 bits keeps the ordinal within int, so it cannot wrap to a
	// non-positive or different item number when converted below.
	parsed, err := strconv.ParseUint(value, 10, strconv.IntSize-1)
	if err != nil || parsed == 0 || strconv.FormatUint(parsed, 10) != value {
		return errors.New("--item must be one canonical positive decimal number")
	}
	item.set = true
	item.value = int(parsed)
	return nil
}

func absoluteResumeRoot(rootPath string) (string, error) {
	if rootPath == "" {
		return "", errors.New("output directory is empty")
	}
	return filepath.Abs(rootPath)
}
