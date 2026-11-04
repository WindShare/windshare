package platformsetup

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"golang.org/x/sys/windows"
)

const maxPolicyOutput = 4 << 20
const processWaitDelay = time.Second

type windowsPlatform struct {
	run        func(context.Context, string) ([]byte, error)
	isElevated func() bool
}

func nativePlatform() windowsPlatform {
	return windowsPlatform{run: runPowerShell, isElevated: func() bool { return windows.GetCurrentProcessToken().IsElevated() }}
}

func (platform windowsPlatform) Inspect(ctx context.Context, executable string) (Snapshot, error) {
	executable, err := longInstallationPath(executable)
	if err != nil {
		return Snapshot{}, err
	}
	output, err := platform.run(ctx, "$Executable = "+quotePowerShell(executable)+"\n"+inspectScript)
	if err != nil {
		return Snapshot{}, err
	}
	var snapshot Snapshot
	if err := json.Unmarshal(output, &snapshot); err != nil {
		return Snapshot{}, fmt.Errorf("decode current firewall policy: %w", err)
	}
	if snapshot.Networks == nil || snapshot.Profiles == nil || snapshot.Rules == nil {
		return Snapshot{}, errors.New("incomplete Windows firewall snapshot")
	}
	return snapshot, nil
}

func (platform windowsPlatform) Apply(ctx context.Context, executable string, action RuleAction, allowElevation bool) Operation {
	if err := ctx.Err(); err != nil {
		return Operation{State: OperationFailed, Detail: err.Error()}
	}
	if action != EnsureRules && action != RemoveRules {
		return Operation{State: OperationFailed, Detail: "invalid rule operation"}
	}
	executable, err := longInstallationPath(executable)
	if err != nil {
		return Operation{State: OperationFailed, Detail: err.Error()}
	}
	script := ruleScript(executable, action)
	if !platform.isElevated() {
		if !allowElevation {
			return Operation{State: PermissionRequired, Detail: "run wind setup network from an administrator terminal"}
		}
		// Only the bounded rule writer acquires administrator authority. It neither
		// starts a transfer engine nor reads or writes the user's preferences.
		script = "$RulesScript = " + quotePowerShell(script) + "\n" + elevateScript
	}
	output, err := platform.run(ctx, script)
	var result struct {
		Code   *int
		Detail string
	}
	if decodeErr := json.Unmarshal(output, &result); decodeErr != nil || result.Code == nil {
		if err == nil {
			err = decodeErr
		}
		if err == nil {
			err = errors.New("missing firewall rule operation result")
		}
		return Operation{State: OperationFailed, Detail: err.Error()}
	}
	switch *result.Code {
	case 0:
		if err != nil {
			return Operation{State: OperationFailed, Detail: err.Error()}
		}
		state := RulesApplied
		if action == RemoveRules {
			state = RulesRemoved
		}
		return Operation{State: state}
	case 1223:
		return Operation{State: AuthorizationDeclined, Detail: "Windows authorization was cancelled; setup preference was not changed"}
	case 5:
		return Operation{State: PermissionRequired, Detail: "Windows denied permission to manage firewall rules"}
	case 10:
		return Operation{State: OperationFailed, Detail: "a WindShare rule name belongs to another owner or executable"}
	default:
		return Operation{State: OperationFailed, Detail: result.Detail}
	}
}

func runPowerShell(ctx context.Context, script string) ([]byte, error) {
	directory, err := windows.GetSystemDirectory()
	if err != nil {
		return nil, err
	}
	command := exec.CommandContext(ctx, filepath.Join(directory, "WindowsPowerShell", "v1.0", "powershell.exe"),
		"-NoLogo", "-NoProfile", "-NonInteractive", "-EncodedCommand", encodePowerShell(script))
	command.SysProcAttr = &syscall.SysProcAttr{HideWindow: true}
	command.WaitDelay = processWaitDelay
	var output, stderr limitedOutput
	command.Stdout, command.Stderr = &output, &stderr
	err = command.Run()
	if ctx.Err() != nil {
		err = ctx.Err()
	}
	if err != nil {
		err = fmt.Errorf("read/apply Windows firewall policy: %w: %s", err, strings.TrimSpace(stderr.String()))
	}
	return bytes.TrimPrefix(output.Bytes(), []byte{0xef, 0xbb, 0xbf}), err
}

type limitedOutput struct{ bytes.Buffer }

func (buffer *limitedOutput) Write(data []byte) (int, error) {
	if buffer.Len()+len(data) > maxPolicyOutput {
		return 0, errors.New("firewall policy output exceeds limit")
	}
	return buffer.Buffer.Write(data)
}
