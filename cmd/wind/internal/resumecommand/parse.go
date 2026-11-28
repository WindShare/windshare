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
) (resumeRootRequest, error) {
	flags := command.NewFlagSet(action)
	var rootPath resumeRootPathFlag
	flags.Var(&rootPath, "o", "output directory (required)")
	if err := command.ParseNoPositionals(flags, args); err != nil {
		if !errors.Is(err, command.ErrHelp) {
			parser.logger.Logf("%s: %v", action, err)
		}
		return resumeRootRequest{}, err
	}
	if !rootPath.set {
		err := errors.New("-o is required")
		parser.logger.Logf("%s: %v", action, err)
		return resumeRootRequest{}, err
	}
	absolute, err := absoluteResumeRoot(rootPath.value)
	if err != nil {
		err := errors.New("output directory is invalid")
		parser.logger.Logf("%s: %v", action, err)
		return resumeRootRequest{}, err
	}
	return resumeRootRequest{rootPath: absolute}, nil
}

func (parser flagRequestParser) ParseDiscard(args []string) (resumeDiscardRequest, error) {
	flags := command.NewFlagSet("resume discard")
	var rootPath resumeRootPathFlag
	flags.Var(&rootPath, "o", "output directory (required)")
	var item resumeItemNumberFlag
	flags.Var(&item, "item", "one current inventory item number (required)")
	if err := command.ParseNoPositionals(flags, args); err != nil {
		if !errors.Is(err, command.ErrHelp) {
			parser.logger.Logf("resume discard: %v", err)
		}
		return resumeDiscardRequest{}, err
	}
	if !rootPath.set {
		err := errors.New("-o is required")
		parser.logger.Logf("resume discard: %v", err)
		return resumeDiscardRequest{}, err
	}
	if !item.set {
		err := errors.New("--item is required")
		parser.logger.Logf("resume discard: %v", err)
		return resumeDiscardRequest{}, err
	}
	absolute, err := absoluteResumeRoot(rootPath.value)
	if err != nil {
		err := errors.New("output directory is invalid")
		parser.logger.Logf("resume discard: %v", err)
		return resumeDiscardRequest{}, err
	}
	return resumeDiscardRequest{rootPath: absolute, itemNumber: item.value}, nil
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
