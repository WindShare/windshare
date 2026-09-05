package main

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

const installerTimeout = 60 * time.Second
const staleInstallerBinary = "STALE_UNTRACKED_BINARY"

type installerPlatform struct {
	name, shell, script, binary string
}

func TestInstallersSourceAuthority(t *testing.T) {
	repository, err := filepath.Abs("../../..")
	if err != nil {
		t.Fatal(err)
	}
	platforms := []installerPlatform{}
	if runtime.GOOS == "windows" {
		shell, err := exec.LookPath("pwsh")
		if err != nil {
			t.Fatal("Windows installer regression requires pwsh:", err)
		}
		platforms = append(platforms, installerPlatform{"windows", shell, "scripts/install/windows/install.ps1", "wind.exe"})
	}
	bash, err := exec.LookPath("bash")
	if runtime.GOOS == "windows" {
		// Windows' WSL launcher is not a POSIX shell for the host Go toolchain.
		bash = filepath.Join(os.Getenv("ProgramFiles"), "Git", "bin", "bash.exe")
		_, err = os.Stat(bash)
	}
	if err == nil {
		platforms = append(platforms, installerPlatform{"posix", bash, "scripts/install/install.sh", "wind"})
	} else if runtime.GOOS != "windows" {
		t.Fatal("POSIX installer regression requires bash:", err)
	} else {
		t.Log("POSIX cross-platform regression unavailable on this Windows host:", err)
	}
	for _, platform := range platforms {
		t.Run(platform.name, func(t *testing.T) {
			for _, scenario := range []string{"checkout", "source-bundle", "verification-failure", "build-failure", "binary-bundle", "missing-distribution"} {
				t.Run(scenario, func(t *testing.T) {
					testInstallerFixture(t, repository, platform, scenario)
				})
			}
		})
	}
}

func testInstallerFixture(t *testing.T, repository string, platform installerPlatform, scenario string) {
	t.Helper()
	root := t.TempDir()
	source := filepath.Join(root, "distribution with spaces")
	destination := filepath.Join(root, "installed app")
	state := filepath.Join(root, "private setup state")
	for _, path := range []string{platform.script, "scripts/install/windows/firewall.psm1"} {
		content, err := os.ReadFile(filepath.Join(repository, filepath.FromSlash(path)))
		if err != nil {
			t.Fatal(err)
		}
		writeInstallerFixture(t, source, path, string(content))
	}
	sourceMode := scenario != "binary-bundle" && scenario != "missing-distribution"
	if scenario != "missing-distribution" {
		writeInstallerFixture(t, source, platform.binary, staleInstallerBinary)
	}
	if sourceMode {
		writeInstallerFixture(t, source, "go.mod", "module installer.fixture\n\ngo 1.23\n")
		writeInstallerFixture(t, source, "scripts/ci/_piondeps/main.go", installerVerifierFixture)
		writeInstallerFixture(t, source, "cmd/wind/main.go", "package main\nimport \"fmt\"\nfunc main() { fmt.Println(\"BUILT_FROM_SOURCE\") }\n")
	}
	if scenario == "checkout" {
		writeInstallerFixture(t, source, ".git/HEAD", "fixture checkout identity\n")
	}
	if scenario == "verification-failure" {
		writeInstallerFixture(t, source, "reject-verification", "")
	}
	if scenario == "build-failure" {
		writeInstallerFixture(t, source, "cmd/wind/main.go", "this cannot compile\n")
	}
	failure := scenario == "verification-failure" || scenario == "build-failure" || scenario == "missing-distribution"
	installed := filepath.Join(destination, platform.binary)
	if failure {
		writeInstallerFixture(t, destination, platform.binary, "PREVIOUS_INSTALLATION")
		writeInstallerFixture(t, state, "connectivity-setup.json", "PREVIOUS_SETUP_STATE")
	}
	output, err := runInstallerFixture(t, platform, source, destination, state, "Skip")
	if failure {
		if err == nil {
			t.Fatalf("installation unexpectedly succeeded: %s", output)
		}
		assertInstallerFile(t, installed, "PREVIOUS_INSTALLATION")
		assertInstallerFile(t, filepath.Join(state, "connectivity-setup.json"), "PREVIOUS_SETUP_STATE")
	} else {
		if err != nil {
			t.Fatalf("install: %v\n%s", err, output)
		}
		if sourceMode {
			command := exec.Command(installed)
			if platform.name == "posix" {
				command = exec.Command(platform.shell, "-c", `exec "$1"`, "installer-fixture", filepath.ToSlash(installed))
			}
			output, err := command.CombinedOutput()
			if err != nil || strings.TrimSpace(string(output)) != "BUILT_FROM_SOURCE" {
				t.Fatalf("installed source executable: %v, %s", err, output)
			}
		} else {
			assertInstallerFile(t, installed, staleInstallerBinary)
		}
		if platform.name == "windows" {
			statusPath := filepath.Join(state, "connectivity-setup.json")
			statusBytes, err := os.ReadFile(statusPath)
			if err != nil {
				t.Fatal(err)
			}
			var status struct {
				State, Reason, Executable string
			}
			if err := json.Unmarshal(statusBytes, &status); err != nil {
				t.Fatal(err)
			}
			if status.State != "declined" || status.Reason != "user-skipped" || status.Executable != installed {
				t.Fatalf("skip status: %s", statusBytes)
			}
			if scenario == "binary-bundle" {
				// A saved choice must avoid an interactive prompt even in a noninteractive install.
				output, err := runInstallerFixture(t, platform, source, destination, state, "Ask")
				if err != nil {
					t.Fatalf("reuse setup choice: %v\n%s", err, output)
				}
				assertInstallerFile(t, statusPath, string(statusBytes))
			}
		}
	}
	if sourceMode {
		assertInstallerFile(t, filepath.Join(source, "verification-ran"), "GOWORK=off")
	}
}

const installerVerifierFixture = `package main
import ("os")
func main() {
	if os.Getenv("GOWORK") != "off" { panic("ambient workspace leaked") }
	if err := os.WriteFile("verification-ran", []byte("GOWORK=off"), 0600); err != nil { panic(err) }
	if _, err := os.Stat("reject-verification"); err == nil { os.Exit(17) }
}
`

func runInstallerFixture(t *testing.T, platform installerPlatform, source, destination, state, choice string) ([]byte, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), installerTimeout)
	defer cancel()
	arguments := []string{filepath.Join(source, filepath.FromSlash(platform.script)), destination}
	if platform.name == "windows" {
		arguments = []string{"-NoLogo", "-NoProfile", "-NonInteractive", "-File", arguments[0],
			"-Destination", destination, "-StateDirectory", state, "-Firewall", choice}
	} else {
		for i := range arguments {
			arguments[i] = filepath.ToSlash(arguments[i])
		}
	}
	command := exec.CommandContext(ctx, platform.shell, arguments...)
	command.Dir = source
	command.Env = buildEnvironment()
	// A source installer must explicitly isolate itself from an ambient workspace.
	command.Env = append(command.Env, "GOWORK="+filepath.Join(source, "nonexistent.work"))
	return command.CombinedOutput()
}

func writeInstallerFixture(t *testing.T, root, path, content string) {
	t.Helper()
	destination := filepath.Join(root, filepath.FromSlash(path))
	if err := os.MkdirAll(filepath.Dir(destination), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(destination, []byte(content), 0o755); err != nil {
		t.Fatal(err)
	}
}

func assertInstallerFile(t *testing.T, path, expected string) {
	t.Helper()
	actual, err := os.ReadFile(path)
	if err != nil || string(actual) != expected {
		t.Fatalf("%s: got %q, error %v; want %q", path, actual, err, expected)
	}
}
