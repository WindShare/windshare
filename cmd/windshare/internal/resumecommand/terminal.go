package resumecommand

import (
	"bufio"
	"context"
	"errors"
	"io"
	"strings"

	"golang.org/x/term"
)

type stdioResumeConfirmationTerminal struct {
	input       io.Reader
	output      io.Writer
	interactive bool
}

func newStdioResumeConfirmationTerminal(
	input io.Reader,
	rawOutput io.Writer,
	serializedOutput io.Writer,
) stdioResumeConfirmationTerminal {
	inputFD, inputIsFile := input.(interface{ Fd() uintptr })
	outputFD, outputIsFile := rawOutput.(interface{ Fd() uintptr })
	return stdioResumeConfirmationTerminal{
		input:  input,
		output: serializedOutput,
		interactive: inputIsFile && outputIsFile &&
			term.IsTerminal(int(inputFD.Fd())) && term.IsTerminal(int(outputFD.Fd())),
	}
}

func (terminal stdioResumeConfirmationTerminal) Interactive() bool {
	return terminal.interactive
}

func (terminal stdioResumeConfirmationTerminal) ReadLine(
	ctx context.Context,
	prompt string,
) (string, error) {
	if !terminal.interactive || terminal.input == nil || terminal.output == nil {
		return "", errResumeTerminalRequired
	}
	if err := ctx.Err(); err != nil {
		return "", err
	}
	written, err := io.WriteString(terminal.output, prompt)
	if err != nil {
		return "", err
	}
	if written != len(prompt) {
		return "", io.ErrShortWrite
	}
	line, err := bufio.NewReader(terminal.input).ReadString('\n')
	if err != nil {
		return "", errors.Join(errResumeConfirmationInput, err)
	}
	line = strings.TrimSuffix(line, "\n")
	line = strings.TrimSuffix(line, "\r")
	return line, nil
}
