package resumecommand

import (
	"errors"
	"flag"
	"io"
	"path/filepath"
	"strconv"
)

type flagRequestParser struct {
	output io.Writer
	logger resumeLogger
}

func (parser flagRequestParser) ParseRoot(
	action string,
	args []string,
) (resumeRootRequest, bool) {
	flags := parser.newFlagSet(action)
	rootPath := flags.String("o", "", "output directory (required)")
	positional, err := parseInterleaved(flags, args)
	if err != nil {
		return resumeRootRequest{}, false
	}
	if len(positional) != 0 {
		parser.logger.Logf("%s: positional arguments are not accepted", action)
		return resumeRootRequest{}, false
	}
	if *rootPath == "" {
		parser.logger.Logf("%s: -o is required", action)
		return resumeRootRequest{}, false
	}
	absolute, err := absoluteResumeRoot(*rootPath)
	if err != nil {
		parser.logger.Logf("%s: output directory %q is invalid: %v", action, *rootPath, err)
		return resumeRootRequest{}, false
	}
	return resumeRootRequest{rootPath: absolute}, true
}

func (parser flagRequestParser) ParseDiscard(args []string) (resumeDiscardRequest, bool) {
	flags := parser.newFlagSet("resume discard")
	rootPath := flags.String("o", "", "output directory (required)")
	var item resumeItemNumberFlag
	flags.Var(&item, "item", "one current inventory item number (required)")
	positional, err := parseInterleaved(flags, args)
	if err != nil {
		return resumeDiscardRequest{}, false
	}
	if len(positional) != 0 {
		parser.logger.Logf("resume discard: positional arguments are not accepted")
		return resumeDiscardRequest{}, false
	}
	if *rootPath == "" {
		parser.logger.Logf("resume discard: -o is required")
		return resumeDiscardRequest{}, false
	}
	if !item.set {
		parser.logger.Logf("resume discard: --item is required")
		return resumeDiscardRequest{}, false
	}
	absolute, err := absoluteResumeRoot(*rootPath)
	if err != nil {
		parser.logger.Logf("resume discard: output directory %q is invalid: %v", *rootPath, err)
		return resumeDiscardRequest{}, false
	}
	return resumeDiscardRequest{rootPath: absolute, itemNumber: item.value}, true
}

func (parser flagRequestParser) newFlagSet(name string) *flag.FlagSet {
	flags := flag.NewFlagSet(name, flag.ContinueOnError)
	flags.SetOutput(parser.output)
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
