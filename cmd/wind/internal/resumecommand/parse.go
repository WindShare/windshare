package resumecommand

import (
	"errors"
	"flag"
	"io"
	"path/filepath"
	"strconv"
)

type flagRequestParser struct {
	logger resumeLogger
}

func (parser flagRequestParser) ParseRoot(
	action string,
	args []string,
) (resumeRootRequest, resumeRequestParse) {
	flags := parser.newFlagSet(action)
	var rootPath resumeRootPathFlag
	flags.Var(&rootPath, "o", "output directory (required)")
	positional, err := parseInterleaved(flags, args)
	if errors.Is(err, flag.ErrHelp) {
		return resumeRootRequest{}, resumeRequestHelp
	}
	if err != nil {
		parser.logger.Logf("%s: arguments are invalid", action)
		return resumeRootRequest{}, resumeRequestInvalid
	}
	if len(positional) != 0 {
		parser.logger.Logf("%s: positional arguments are not accepted", action)
		return resumeRootRequest{}, resumeRequestInvalid
	}
	if !rootPath.set {
		parser.logger.Logf("%s: -o is required", action)
		return resumeRootRequest{}, resumeRequestInvalid
	}
	absolute, err := absoluteResumeRoot(rootPath.value)
	if err != nil {
		parser.logger.Logf("%s: output directory is invalid", action)
		return resumeRootRequest{}, resumeRequestInvalid
	}
	return resumeRootRequest{rootPath: absolute}, resumeRequestReady
}

func (parser flagRequestParser) ParseDiscard(args []string) (resumeDiscardRequest, resumeRequestParse) {
	flags := parser.newFlagSet("resume discard")
	var rootPath resumeRootPathFlag
	flags.Var(&rootPath, "o", "output directory (required)")
	var item resumeItemNumberFlag
	flags.Var(&item, "item", "one current inventory item number (required)")
	positional, err := parseInterleaved(flags, args)
	if errors.Is(err, flag.ErrHelp) {
		return resumeDiscardRequest{}, resumeRequestHelp
	}
	if err != nil {
		parser.logger.Logf("resume discard: arguments are invalid")
		return resumeDiscardRequest{}, resumeRequestInvalid
	}
	if len(positional) != 0 {
		parser.logger.Logf("resume discard: positional arguments are not accepted")
		return resumeDiscardRequest{}, resumeRequestInvalid
	}
	if !rootPath.set {
		parser.logger.Logf("resume discard: -o is required")
		return resumeDiscardRequest{}, resumeRequestInvalid
	}
	if !item.set {
		parser.logger.Logf("resume discard: --item is required")
		return resumeDiscardRequest{}, resumeRequestInvalid
	}
	absolute, err := absoluteResumeRoot(rootPath.value)
	if err != nil {
		parser.logger.Logf("resume discard: output directory is invalid")
		return resumeDiscardRequest{}, resumeRequestInvalid
	}
	return resumeDiscardRequest{rootPath: absolute, itemNumber: item.value}, resumeRequestReady
}

func (parser flagRequestParser) newFlagSet(name string) *flag.FlagSet {
	flags := flag.NewFlagSet(name, flag.ContinueOnError)
	// The standard flag package includes rejected values in diagnostics. Resume
	// accepts no secrets, but suppressing reflection still protects a capability
	// accidentally pasted into --item or another unsupported position.
	flags.SetOutput(io.Discard)
	return flags
}

func parseInterleaved(flags *flag.FlagSet, args []string) ([]string, error) {
	var positional []string
	for {
		if err := flags.Parse(args); err != nil {
			return nil, err
		}
		rest := flags.Args()
		if len(rest) == 0 {
			return positional, nil
		}
		positional = append(positional, rest[0])
		args = rest[1:]
	}
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
	parsed, err := strconv.ParseUint(value, 10, strconv.IntSize)
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
