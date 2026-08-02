package perfevidence

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// passthroughMutationDomain exercises the production command/sink contract in
// portable package tests. OS containment itself is proven by mutationdomain's
// platform tests; duplicating that implementation here would create a cycle.
type passthroughMutationDomainFactory struct{}

func (passthroughMutationDomainFactory) Open(
	_ context.Context,
	_ MutationDomainSpec,
) (MutationDomain, error) {
	return passthroughMutationDomain{}, nil
}

type passthroughMutationDomain struct{}

func (passthroughMutationDomain) Run(
	ctx context.Context,
	command MutationDomainCommand,
	sinks map[string]MutationOutputSink,
) (MutationDomainResult, error) {
	privateRoot, err := os.MkdirTemp("", "windshare-test-mutation-")
	if err != nil {
		return MutationDomainResult{ExitCode: -1}, err
	}
	defer func() { _ = os.RemoveAll(privateRoot) }()

	replacements := make(map[string]string, len(command.Outputs))
	for index, output := range command.Outputs {
		privateName := fmt.Sprintf("output-%d%s", index, filepath.Ext(output.HostPath))
		replacements[output.HostPath] = filepath.Join(privateRoot, privateName)
	}
	rewrite := func(value string) string {
		for host, private := range replacements {
			value = strings.ReplaceAll(value, host, private)
		}
		return value
	}
	arguments := append([]string(nil), command.Arguments...)
	for index := range arguments {
		arguments[index] = rewrite(arguments[index])
	}
	environment := append([]string(nil), command.Environment...)
	for index := range environment {
		environment[index] = rewrite(environment[index])
	}
	raw, runErr := (ProcessRunner{}).Run(ctx, Command{
		Executable: rewrite(command.Executable),
		Arguments:  arguments, Directory: rewrite(command.Directory),
		Environment: environment, ReplaceEnvironment: true,
	})
	result := MutationDomainResult{
		Stdout: raw.Stdout, Stderr: raw.Stderr, ProcessID: raw.ProcessID,
		ExitCode: raw.ExitCode, StartedAt: raw.StartedAt, FinishedAt: raw.FinishedAt,
	}
	if runErr != nil || raw.ExitCode != 0 {
		return result, runErr
	}
	for _, output := range command.Outputs {
		content, readErr := os.ReadFile(replacements[output.HostPath])
		if readErr != nil {
			return result, fmt.Errorf("read private test output %s: %w", output.HostPath, readErr)
		}
		if int64(len(content)) > output.MaxBytes {
			return result, fmt.Errorf("private test output %s exceeds %d bytes", output.HostPath, output.MaxBytes)
		}
		sink := sinks[output.HostPath]
		if sink == nil {
			return result, fmt.Errorf("private test output %s has no sink", output.HostPath)
		}
		written, writeErr := sink.WriteContext(ctx, content)
		if writeErr != nil || written != len(content) {
			return result, errors.Join(fmt.Errorf("write private test output %s: wrote %d of %d", output.HostPath, written, len(content)), writeErr)
		}
		digest := sha256.Sum256(content)
		if sealErr := sink.Seal(ctx, int64(len(content)), hex.EncodeToString(digest[:])); sealErr != nil {
			return result, fmt.Errorf("seal private test output %s: %w", output.HostPath, sealErr)
		}
	}
	return result, nil
}

func (passthroughMutationDomain) Close() error { return nil }
