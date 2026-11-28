package resumecommand

import (
	"errors"
	"path/filepath"
	"strconv"

	"github.com/windshare/windshare/cmd/wind/internal/command"
)

// List and discard accept no positional arguments. ParseNoPositionals stops at
// the first positional or "--" so later tokens cannot be reinterpreted as help.
type flagRequestParser struct{}

func (flagRequestParser) ParseRoot(
	action string,
	args []string,
) (resumeRootRequest, error) {
	flags := command.NewFlagSet(action)
	var rootPath resumeRootPathFlag
	flags.Var(&rootPath, "o", "output directory (required)")
	if err := command.ParseNoPositionals(flags, args); err != nil {
		return resumeRootRequest{}, err
	}
	if !rootPath.set {
		return resumeRootRequest{}, errors.New("-o is required")
	}
	absolute, err := absoluteResumeRoot(rootPath.value)
	if err != nil {
		return resumeRootRequest{}, errors.New("output directory is invalid")
	}
	return resumeRootRequest{rootPath: absolute}, nil
}

func (flagRequestParser) ParseDiscard(args []string) (resumeDiscardRequest, error) {
	flags := command.NewFlagSet("resume discard")
	var rootPath resumeRootPathFlag
	flags.Var(&rootPath, "o", "output directory (required)")
	var item resumeItemNumberFlag
	flags.Var(&item, "item", "one current inventory item number (required)")
	if err := command.ParseNoPositionals(flags, args); err != nil {
		return resumeDiscardRequest{}, err
	}
	if !rootPath.set {
		return resumeDiscardRequest{}, errors.New("-o is required")
	}
	if !item.set {
		return resumeDiscardRequest{}, errors.New("--item is required")
	}
	absolute, err := absoluteResumeRoot(rootPath.value)
	if err != nil {
		return resumeDiscardRequest{}, errors.New("output directory is invalid")
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
		return command.OptionError("exactly one -o is allowed")
	}
	if value == "" {
		return command.OptionError("-o must name an output directory")
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
		return command.OptionError("exactly one --item is allowed")
	}
	// IntSize-1 bits keeps the ordinal within int, so it cannot wrap to a
	// non-positive or different item number when converted below.
	parsed, err := strconv.ParseUint(value, 10, strconv.IntSize-1)
	if err != nil || parsed == 0 || strconv.FormatUint(parsed, 10) != value {
		return command.OptionError("--item must be one canonical positive decimal number")
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
