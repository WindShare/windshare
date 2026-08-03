package mutationdomain

import (
	"bytes"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/windshare/windshare/internal/perfevidence"
)

func (state *helperState) rewriteCommand(command perfevidence.MutationDomainCommand) (
	perfevidence.MutationDomainCommand,
	error,
) {
	result := command
	var admitted bool
	result.Executable, admitted = state.rewriteInputPath(command.Executable)
	if !admitted {
		return perfevidence.MutationDomainCommand{}, fmt.Errorf(
			"isolated executable %s is outside the admitted immutable inputs", command.Executable,
		)
	}
	result.Directory, admitted = state.rewriteInputPath(command.Directory)
	if !admitted {
		return perfevidence.MutationDomainCommand{}, fmt.Errorf(
			"isolated working directory %s is outside the admitted immutable inputs", command.Directory,
		)
	}
	commandOutputs := make(map[string]string, len(command.Outputs))
	for _, output := range command.Outputs {
		hostPath := filepath.Clean(output.HostPath)
		if !filepath.IsAbs(hostPath) || hostPath != output.HostPath || output.MaxBytes < 0 {
			return perfevidence.MutationDomainCommand{}, fmt.Errorf("isolated output path %q is not canonical", output.HostPath)
		}
		if state.generation == ^uint64(0) {
			return perfevidence.MutationDomainCommand{}, errors.New("isolated output generation space was exhausted")
		}
		isolated := filepath.Join(
			state.privateRoot, privateOutputDirectory,
			hashBytes(fmt.Appendf(nil, "%s\x00%d", hostPath, state.generation)),
		)
		state.generation++
		if _, exists := commandOutputs[hostPath]; exists {
			return perfevidence.MutationDomainCommand{}, fmt.Errorf("isolated output %s is duplicated", hostPath)
		}
		if _, exists := state.outputs[hostPath]; exists {
			return perfevidence.MutationDomainCommand{}, fmt.Errorf("isolated output %s was already produced", hostPath)
		}
		if _, exists := state.promoted[hostPath]; exists {
			return perfevidence.MutationDomainCommand{}, fmt.Errorf("immutable output %s was already promoted", hostPath)
		}
		commandOutputs[hostPath] = isolated
	}
	result.Arguments = append([]string(nil), command.Arguments...)
	for index := range result.Arguments {
		result.Arguments[index] = state.rewriteText(result.Arguments[index], commandOutputs)
	}
	result.Environment = make([]string, 0, len(command.Environment)+1)
	mutable := map[string]string{
		"GOCACHE":  filepath.Join(state.privateRoot, privateCacheDirectory),
		"GOTMPDIR": filepath.Join(state.privateRoot, privateTemporaryDirectory),
		"TEMP":     filepath.Join(state.privateRoot, privateTemporaryDirectory),
		"TMP":      filepath.Join(state.privateRoot, privateTemporaryDirectory),
		"TMPDIR":   filepath.Join(state.privateRoot, privateTemporaryDirectory),
	}
	seen := make(map[string]bool)
	for _, entry := range command.Environment {
		name, value, found := strings.Cut(entry, "=")
		if !found {
			return perfevidence.MutationDomainCommand{}, fmt.Errorf("invalid isolated environment entry %q", entry)
		}
		if replacement, ok := mutable[strings.ToUpper(name)]; ok {
			value = replacement
			seen[strings.ToUpper(name)] = true
		} else {
			value = state.rewriteText(value, commandOutputs)
		}
		result.Environment = append(result.Environment, name+"="+value)
	}
	for name, value := range mutable {
		if !seen[name] {
			result.Environment = append(result.Environment, name+"="+value)
		}
	}
	sort.Strings(result.Environment)
	maps.Copy(state.outputs, commandOutputs)
	return result, nil
}

func (state *helperState) rewriteInputPath(path string) (string, bool) {
	clean := filepath.Clean(path)
	if promoted := state.promoted[clean]; promoted != nil {
		return promoted.path(), true
	}
	bestSource := ""
	bestDestination := ""
	bestRelative := ""
	for source, destination := range state.pathMap {
		relative, err := filepath.Rel(source, clean)
		if err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
			continue
		}
		if len(source) > len(bestSource) {
			bestSource = source
			bestDestination = destination
			bestRelative = relative
		}
	}
	if bestSource == "" {
		return "", false
	}
	return filepath.Join(bestDestination, bestRelative), true
}

type helperPathMapping struct {
	source      string
	destination string
}

func (state *helperState) rewriteText(value string, additions map[string]string) string {
	bySource := make(map[string]helperPathMapping, len(state.pathMap)+len(state.promoted)+len(state.outputs)+len(additions))
	add := func(source, destination string) {
		key := source
		if runtime.GOOS == "windows" {
			key = strings.ToLower(key)
		}
		bySource[key] = helperPathMapping{source: source, destination: destination}
	}
	for source, destination := range state.pathMap {
		add(source, destination)
	}
	for source, input := range state.promoted {
		add(source, input.path())
	}
	for source, destination := range state.outputs {
		add(source, destination)
	}
	for source, destination := range additions {
		add(source, destination)
	}
	mappings := make([]helperPathMapping, 0, len(bySource))
	for _, mapping := range bySource {
		mappings = append(mappings, mapping)
	}
	result, _ := rewritePathMappings(value, mappings, 0)
	return result
}

func (state *helperState) restoreCapturedPaths(
	content []byte,
	command perfevidence.MutationDomainCommand,
) ([]byte, error) {
	bySource := make(map[string]helperPathMapping, len(state.pathMap)+len(state.promoted)+len(state.outputs)+2)
	add := func(source, destination string) {
		if source == "" || destination == "" {
			return
		}
		key := source
		if runtime.GOOS == "windows" {
			key = strings.ToLower(key)
		}
		bySource[key] = helperPathMapping{source: source, destination: destination}
		if runtime.GOOS == "windows" {
			escapedSource := strings.ReplaceAll(source, `\`, `\\`)
			escapedDestination := strings.ReplaceAll(destination, `\`, `\\`)
			bySource[strings.ToLower(escapedSource)] = helperPathMapping{
				source: escapedSource, destination: escapedDestination,
			}
			forwardSource := filepath.ToSlash(source)
			bySource[strings.ToLower(forwardSource)] = helperPathMapping{
				source: forwardSource, destination: filepath.ToSlash(destination),
			}
		}
	}
	for source, destination := range state.pathMap {
		add(destination, source)
	}
	for source, input := range state.promoted {
		add(input.path(), source)
	}
	for source, destination := range state.outputs {
		add(destination, source)
	}
	environment := make(map[string]string, len(command.Environment))
	for _, entry := range command.Environment {
		name, value, found := strings.Cut(entry, "=")
		if found {
			environment[strings.ToUpper(name)] = value
		}
	}
	add(filepath.Join(state.privateRoot, privateCacheDirectory), environment["GOCACHE"])
	temporary := environment["GOTMPDIR"]
	if temporary == "" {
		for _, name := range []string{"TEMP", "TMP", "TMPDIR"} {
			if environment[name] != "" {
				temporary = environment[name]
				break
			}
		}
	}
	add(filepath.Join(state.privateRoot, privateTemporaryDirectory), temporary)
	mappings := make([]helperPathMapping, 0, len(bySource))
	for _, mapping := range bySource {
		mappings = append(mappings, mapping)
	}
	restored, err := rewriteCapturedPathBytes(content, mappings, maximumCapturedBytes)
	if err != nil {
		return content, fmt.Errorf("restore isolated output path semantics: %w", err)
	}
	return restored, nil
}

func rewriteCapturedPathBytes(content []byte, mappings []helperPathMapping, maximumBytes int) ([]byte, error) {
	if maximumBytes > 0 && len(content) > maximumBytes {
		return nil, errors.New("captured isolated output exceeded its bound")
	}
	// Diagnostics are byte streams. Invalid UTF-8 cannot safely participate in
	// semantic path rewriting, so preserve the already-bounded evidence exactly.
	if !utf8.Valid(content) {
		return bytes.Clone(content), nil
	}
	rewritten, err := rewritePathMappings(string(content), mappings, maximumBytes)
	if err != nil {
		return nil, err
	}
	return []byte(rewritten), nil
}

func rewritePathMappings(value string, mappings []helperPathMapping, maximumBytes int) (string, error) {
	if !utf8.ValidString(value) {
		return "", errors.New("path rewrite input is not valid UTF-8")
	}
	ordered := append([]helperPathMapping(nil), mappings...)
	sort.Slice(ordered, func(left, right int) bool {
		if len(ordered[left].source) != len(ordered[right].source) {
			return len(ordered[left].source) > len(ordered[right].source)
		}
		if ordered[left].source != ordered[right].source {
			return ordered[left].source < ordered[right].source
		}
		return ordered[left].destination < ordered[right].destination
	})
	canonical := ordered[:0]
	seen := make(map[string]struct{}, len(ordered))
	for _, mapping := range ordered {
		if mapping.source == "" || !utf8.ValidString(mapping.source) || !utf8.ValidString(mapping.destination) {
			continue
		}
		key := mapping.source
		if runtime.GOOS == "windows" {
			key = strings.ToLower(key)
		}
		if _, duplicate := seen[key]; duplicate {
			continue
		}
		seen[key] = struct{}{}
		canonical = append(canonical, mapping)
	}
	var result strings.Builder
	result.Grow(len(value))
	for offset := 0; offset < len(value); {
		matched := false
		for _, mapping := range canonical {
			if !pathMappingMatches(value, offset, mapping.source) {
				continue
			}
			if maximumBytes > 0 && result.Len()+len(mapping.destination) > maximumBytes {
				return "", errors.New("restored isolated output exceeded its bound")
			}
			result.WriteString(mapping.destination)
			offset += len(mapping.source)
			matched = true
			break
		}
		if !matched {
			_, width := utf8.DecodeRuneInString(value[offset:])
			if maximumBytes > 0 && result.Len()+width > maximumBytes {
				return "", errors.New("restored isolated output exceeded its bound")
			}
			result.WriteString(value[offset : offset+width])
			offset += width
		}
	}
	return result.String(), nil
}

func pathMappingMatches(value string, offset int, source string) bool {
	if source == "" || offset < 0 || len(value)-offset < len(source) {
		return false
	}
	if !utf8.RuneStart(value[offset]) {
		return false
	}
	candidate := value[offset : offset+len(source)]
	if runtime.GOOS == "windows" {
		if !strings.EqualFold(candidate, source) {
			return false
		}
	} else if candidate != source {
		return false
	}
	if offset > 0 && !pathOperandBoundary(value[offset-1]) {
		return false
	}
	end := offset + len(source)
	if end < len(value) && !utf8.RuneStart(value[end]) {
		return false
	}
	if end == len(value) || os.IsPathSeparator(source[len(source)-1]) {
		return true
	}
	return os.IsPathSeparator(value[end]) || pathOperandBoundary(value[end])
}

func pathOperandBoundary(character byte) bool {
	if character <= ' ' {
		return true
	}
	switch character {
	case '=', ',', ';', ':', '"', '\'', '[', ']', '(', ')', '{', '}':
		return true
	default:
		return false
	}
}
