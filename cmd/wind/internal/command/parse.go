package command

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"strings"
)

var (
	// ErrHelp reports that argument parsing stopped at a help flag.
	ErrHelp = flag.ErrHelp
	// ErrInternal reports a programming error such as a nil FlagSet.
	ErrInternal = errors.New("command arguments could not be parsed")
)

// ParseExitCode maps a flag-parsing result to the process exit status contract.
func ParseExitCode(err error) ExitCode {
	switch {
	case err == nil, errors.Is(err, ErrHelp):
		return ExitOK
	case errors.Is(err, ErrInternal):
		return ExitFailure
	default:
		return ExitUsage
	}
}

// RedactToken strips any URL fragment from an echoed CLI token so an
// accidentally pasted share link does not project its #key into error output.
func RedactToken(token string) string {
	prefix, _, hasFragment := strings.Cut(token, "#")
	if !hasFragment {
		return token
	}
	return prefix + "#<redacted>"
}

// NewFlagSet creates a FlagSet with a discard-only output sink because all wind
// diagnostics flow through LineOutput and TerminalCanvas rather than raw stderr.
func NewFlagSet(name string) *flag.FlagSet {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	fs.Usage = func() {}
	return fs
}

// ParseInterleaved preserves standard flag per-option semantics while allowing
// positional arguments and options to appear in any order.
func ParseInterleaved(fs *flag.FlagSet, args []string) ([]string, error) {
	return parseFlags(fs, args, true)
}

// ParseNoPositionals parses options with the same diagnostics as
// ParseInterleaved, but rejects positional arguments immediately so trailing
// tokens after an invalid positional or "--" cannot be reinterpreted as help.
func ParseNoPositionals(fs *flag.FlagSet, args []string) error {
	_, err := parseFlags(fs, args, false)
	return err
}

func parseFlags(fs *flag.FlagSet, args []string, allowPositionals bool) ([]string, error) {
	if fs == nil {
		return nil, ErrInternal
	}
	var positionals []string
	if allowPositionals {
		positionals = make([]string, 0, len(args))
	}
	for len(args) > 0 {
		arg := args[0]
		if arg == "--" {
			if !allowPositionals {
				if len(args) > 1 {
					return nil, fmt.Errorf("unexpected positional argument %q", RedactToken(args[1]))
				}
				return nil, nil
			}
			return append(positionals, args[1:]...), nil
		}
		if len(arg) < 2 || arg[0] != '-' || arg == "-" {
			if !allowPositionals {
				return nil, fmt.Errorf("unexpected positional argument %q", RedactToken(arg))
			}
			positionals = append(positionals, arg)
			args = args[1:]
			continue
		}
		consumed, err := parseFlagOccurrence(fs, args)
		if err != nil {
			return nil, err
		}
		args = args[consumed:]
	}
	return positionals, nil
}

type boolFlag interface {
	IsBoolFlag() bool
}

func parseFlagOccurrence(fs *flag.FlagSet, args []string) (int, error) {
	arg := args[0]
	numMinuses := 1
	if arg[1] == '-' {
		numMinuses++
	}
	nameAndValue := arg[numMinuses:]
	if nameAndValue == "" || nameAndValue[0] == '-' || nameAndValue[0] == '=' {
		return 0, fmt.Errorf("invalid option syntax %q", RedactToken(arg))
	}
	name, value, hasValue := strings.Cut(nameAndValue, "=")
	registered := fs.Lookup(name)
	if registered == nil {
		if name == "h" || name == "help" {
			return 0, ErrHelp
		}
		return 0, fmt.Errorf("unknown option %q", arg[:numMinuses]+RedactToken(name))
	}
	option := formatOptionName(registered.Name)
	if candidate, ok := registered.Value.(boolFlag); ok && candidate.IsBoolFlag() {
		if !hasValue {
			value = "true"
		}
		if err := fs.Set(registered.Name, value); err != nil {
			return 0, formatOptionValueError(option, value, err)
		}
		return 1, nil
	}
	consumed := 1
	if !hasValue {
		if len(args) < 2 {
			return 0, fmt.Errorf("%s requires a value", option)
		}
		value = args[1]
		consumed = 2
	}
	if err := fs.Set(registered.Name, value); err != nil {
		return 0, formatOptionValueError(option, value, err)
	}
	return consumed, nil
}

func formatOptionName(name string) string {
	if len(name) == 1 {
		return "-" + name
	}
	return "--" + name
}

func formatOptionValueError(option, value string, err error) error {
	if containsOptionToken(err.Error(), option) {
		return err
	}
	return fmt.Errorf("invalid value %q for %s: %w", RedactToken(value), option, err)
}

func containsOptionToken(message, option string) bool {
	for rest := message; ; {
		index := strings.Index(rest, option)
		if index < 0 {
			return false
		}
		beforeOK := index == 0 || rest[index-1] == ' '
		after := index + len(option)
		afterOK := after == len(rest) || rest[after] == ' ' || rest[after] == ':' || rest[after] == ',' || rest[after] == ';'
		if beforeOK && afterOK {
			return true
		}
		rest = rest[index+1:]
	}
}

// WriteFlagHelp renders only registration-time metadata. PrintDefaults uses
// DefValue rather than the parsed Value, so help after a positional capability
// cannot reflect that capability back to the terminal.
func WriteFlagHelp(output LineOutput, fs *flag.FlagSet, synopsis string) {
	var help strings.Builder
	fmt.Fprintf(&help, "Usage: %s %s\n", Name, synopsis)
	if defaults := FlagDefaults(fs); defaults != "" {
		help.WriteString("\nOptions:\n")
		help.WriteString(defaults)
	}
	WriteLines(output, help.String())
}

// FlagDefaults renders PrintDefaults for TerminalCanvas. The flag package
// aligns usage text with tabs, which the canvas would show as literal `\t`
// escapes, so layout tabs become the spaces a terminal would have displayed.
func FlagDefaults(fs *flag.FlagSet) string {
	if fs == nil {
		return ""
	}
	output := fs.Output()
	var defaults strings.Builder
	fs.SetOutput(&defaults)
	fs.PrintDefaults()
	fs.SetOutput(output)
	return expandTabStops(defaults.String())
}

func expandTabStops(text string) string {
	const tabWidth = 8
	var expanded strings.Builder
	expanded.Grow(len(text))
	column := 0
	for _, character := range text {
		switch character {
		case '\t':
			padding := tabWidth - column%tabWidth
			expanded.WriteString(strings.Repeat(" ", padding))
			column += padding
		case '\n':
			expanded.WriteRune(character)
			column = 0
		default:
			expanded.WriteRune(character)
			column++
		}
	}
	return expanded.String()
}

// RepeatedFlag collects repeatable string flags (for example, --only a --only b).
type RepeatedFlag []string

func (f *RepeatedFlag) String() string { return strings.Join(*f, ",") }

func (f *RepeatedFlag) Set(v string) error {
	*f = append(*f, v)
	return nil
}
