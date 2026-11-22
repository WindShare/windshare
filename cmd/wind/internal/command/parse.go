package command

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"strings"
)

// ParseOutcome classifies flag parsing outcomes into a closed set so rejected
// command-line values (such as accidentally pasted capability links or keys)
// are never reflected back to terminal output.
type ParseOutcome uint8

const (
	ParseReady ParseOutcome = iota
	ParseHelp
	ParseUnknownOption
	ParseMissingOptionValue
	ParseInvalidOptionValue
	ParseInvalidOptionSyntax
	ParseUnexpectedPositional
	ParseInternalFailure
)

// Diagnostic returns a closed, value-free human diagnostic for a non-ready parse outcome.
func (outcome ParseOutcome) Diagnostic() string {
	switch outcome {
	case ParseUnknownOption:
		return "unknown option"
	case ParseMissingOptionValue:
		return "option value is required"
	case ParseInvalidOptionValue:
		return "option value is invalid"
	case ParseInvalidOptionSyntax:
		return "option syntax is invalid"
	case ParseUnexpectedPositional:
		return "positional arguments are not accepted"
	default:
		return "command arguments could not be parsed"
	}
}

// ExitCode maps a parse outcome to the process exit status contract.
func (outcome ParseOutcome) ExitCode() ExitCode {
	switch outcome {
	case ParseReady, ParseHelp:
		return ExitOK
	case ParseInternalFailure:
		return ExitFailure
	default:
		return ExitUsage
	}
}

// NewFlagSet creates a FlagSet with a discard-only output sink because the
// standard library quotes rejected option values in its error messages.
// Human diagnostics are produced from ParseOutcome instead.
func NewFlagSet(name string) *flag.FlagSet {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	fs.Usage = func() {}
	return fs
}

// ParseInterleaved preserves the standard flag package's per-option semantics
// while allowing positional arguments and options to appear in any order.
// Parsing one occurrence at a time lets the boundary classify failures from
// option structure without inspecting value-bearing error text.
func ParseInterleaved(fs *flag.FlagSet, args []string) ([]string, ParseOutcome) {
	return parseFlags(fs, args, true)
}

// ParseNoPositionals parses options with the same closed diagnostics as
// ParseInterleaved, but rejects positional arguments immediately so trailing
// tokens after an invalid positional or "--" cannot be reinterpreted as help.
func ParseNoPositionals(fs *flag.FlagSet, args []string) ParseOutcome {
	_, outcome := parseFlags(fs, args, false)
	return outcome
}

func parseFlags(fs *flag.FlagSet, args []string, allowPositionals bool) ([]string, ParseOutcome) {
	if fs == nil {
		return nil, ParseInternalFailure
	}
	var positionals []string
	if allowPositionals {
		positionals = make([]string, 0, len(args))
	}
	for len(args) > 0 {
		if args[0] == "--" {
			if !allowPositionals {
				if len(args) > 1 {
					return nil, ParseUnexpectedPositional
				}
				return nil, ParseReady
			}
			return append(positionals, args[1:]...), ParseReady
		}
		occurrence := inspectFlagOccurrence(fs, args)
		if occurrence.positional {
			if !allowPositionals {
				return nil, ParseUnexpectedPositional
			}
			positionals = append(positionals, args[0])
			args = args[1:]
			continue
		}
		err := fs.Parse(args[:occurrence.width])
		if errors.Is(err, flag.ErrHelp) {
			return nil, ParseHelp
		}
		if err != nil {
			return nil, occurrence.failure
		}
		args = args[occurrence.width:]
	}
	return positionals, ParseReady
}

type flagOccurrence struct {
	width      int
	failure    ParseOutcome
	positional bool
}

type boolFlag interface {
	IsBoolFlag() bool
}

func inspectFlagOccurrence(fs *flag.FlagSet, args []string) flagOccurrence {
	argument := args[0]
	if len(argument) < 2 || argument[0] != '-' || argument == "-" {
		return flagOccurrence{width: 1, positional: true}
	}
	numMinuses := 1
	if argument[1] == '-' {
		numMinuses++
		if len(argument) == numMinuses {
			return flagOccurrence{width: 1, positional: true}
		}
	}
	nameAndValue := argument[numMinuses:]
	if nameAndValue == "" || nameAndValue[0] == '-' || nameAndValue[0] == '=' {
		return flagOccurrence{width: 1, failure: ParseInvalidOptionSyntax}
	}
	name, _, hasValue := strings.Cut(nameAndValue, "=")
	registered := fs.Lookup(name)
	if registered == nil {
		return flagOccurrence{width: 1, failure: ParseUnknownOption}
	}
	if candidate, ok := registered.Value.(boolFlag); ok && candidate.IsBoolFlag() {
		return flagOccurrence{width: 1, failure: ParseInvalidOptionValue}
	}
	if hasValue {
		return flagOccurrence{width: 1, failure: ParseInvalidOptionValue}
	}
	if len(args) == 1 {
		return flagOccurrence{width: 1, failure: ParseMissingOptionValue}
	}
	return flagOccurrence{width: 2, failure: ParseInvalidOptionValue}
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
