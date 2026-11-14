// coverage supplies the installed verdict tool with exactly the sources named
// by a root-module profile, keeping build caches outside source discovery.
package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"syscall"

	"golang.org/x/mod/modfile"
)

type verdictRunner func(context.Context, string, []string, io.Writer, io.Writer) error

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := run(ctx, ".", os.Args[1:], os.Stdout, os.Stderr, runInstalled); err != nil {
		fmt.Fprintf(os.Stderr, "coverage: %v\n", err)
		os.Exit(1)
	}
}

func run(ctx context.Context, root string, args []string, stdout, stderr io.Writer, verdict verdictRunner) (result error) {
	flags := flag.NewFlagSet("coverage", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	config := flags.String("config", ".testcoverage.yml", "coverage policy")
	profile := flags.String("profile", "cover.out", "root-module coverage profile")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return errors.New("unexpected positional arguments")
	}
	root, err := filepath.Abs(root)
	if err != nil {
		return err
	}
	goMod, err := os.ReadFile(filepath.Join(root, "go.mod"))
	if err != nil {
		return err
	}
	module := modfile.ModulePath(goMod)
	if module == "" {
		return errors.New("root go.mod has no module directive")
	}
	profilePath := resolvePath(root, *profile)
	input, err := os.Open(profilePath)
	if err != nil {
		return err
	}
	files, scanErr := profileSources(input, module)
	if err := errors.Join(scanErr, input.Close()); err != nil {
		return err
	}
	directory, err := os.MkdirTemp("", "windshare-coverage-sources-")
	if err != nil {
		return err
	}
	defer func() {
		if err := os.RemoveAll(directory); err != nil {
			result = errors.Join(result, fmt.Errorf("remove owned coverage sources %s: %w", directory, err))
		}
	}()
	if err := os.WriteFile(filepath.Join(directory, "go.mod"), goMod, 0600); err != nil {
		return err
	}
	var sourceBytes int64
	for _, file := range files {
		if err := ctx.Err(); err != nil {
			return err
		}
		data, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(file)))
		if err != nil {
			return fmt.Errorf("read profile source %s: %w", file, err)
		}
		target := filepath.Join(directory, filepath.FromSlash(file))
		if err := os.MkdirAll(filepath.Dir(target), 0700); err != nil {
			return err
		}
		if err := os.WriteFile(target, data, 0600); err != nil {
			return err
		}
		sourceBytes += int64(len(data))
	}
	// Keep the verdict and annotations authoritative in the existing tool. A
	// temporary source projection replaces its unbounded repository walk; it
	// does not filter profile blocks, reinterpret thresholds, or cache verdicts.
	if err := json.NewEncoder(stderr).Encode(map[string]any{
		"gate": "coverage", "event": "sources_staged", "operation": filepath.Base(directory),
		"profile": profilePath, "files": len(files), "source_bytes": sourceBytes,
	}); err != nil {
		return err
	}
	return verdict(ctx, root, []string{
		"--config=" + resolvePath(root, *config),
		"--profile=" + profilePath,
		"--source-dir=" + directory,
	}, stdout, stderr)
}

func resolvePath(root, name string) string {
	if filepath.IsAbs(name) {
		return name
	}
	return filepath.Join(root, name)
}

// Only inventory the source paths. The installed tool still validates every
// block and count from the original, unchanged profile.
func profileSources(input io.Reader, module string) ([]string, error) {
	scanner := bufio.NewScanner(input)
	if !scanner.Scan() {
		return nil, errors.Join(errors.New("coverage profile is empty"), scanner.Err())
	}
	switch scanner.Text() {
	case "mode: set", "mode: count", "mode: atomic":
	default:
		return nil, errors.New("invalid coverage profile mode")
	}
	seen := make(map[string]bool)
	for scanner.Scan() {
		line := scanner.Text()
		delimiter := strings.LastIndexByte(line, ':')
		if delimiter < 0 {
			return nil, fmt.Errorf("coverage block has no source path: %q", line)
		}
		name := line[:delimiter]
		file, owned := strings.CutPrefix(name, module+"/")
		if !owned || !strings.HasSuffix(file, ".go") || path.Clean(file) != file ||
			!filepath.IsLocal(filepath.FromSlash(file)) {
			return nil, fmt.Errorf("coverage source is outside the root module: %q", name)
		}
		seen[file] = true
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("read coverage profile: %w", err)
	}
	files := make([]string, 0, len(seen))
	for file := range seen {
		files = append(files, file)
	}
	sort.Strings(files)
	return files, nil
}

func runInstalled(ctx context.Context, root string, args []string, stdout, stderr io.Writer) error {
	command := exec.CommandContext(ctx, "go-test-coverage", args...)
	command.Dir, command.Stdout, command.Stderr = root, stdout, stderr
	return command.Run()
}
