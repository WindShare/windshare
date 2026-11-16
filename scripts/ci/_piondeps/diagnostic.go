package main

import (
	"fmt"
	"go/build"
	"path/filepath"
	"slices"
)

type diagnosticSession struct {
	Directory       string          `json:"directory"`
	Profile         string          `json:"profile"`
	Build           diagnosticBuild `json:"build"`
	OpenFiles       []string        `json:"open_files"`
	DiagnosticFiles []string        `json:"diagnostic_files"`
}

type diagnosticGroup struct {
	module, profile string
	build           diagnosticBuild
	sources         []diagnosticSource
	packages        map[string]bool
}

// Profiles describe supported configurations, not arbitrary satisfying assignments
// of one file's constraints. Every selected file must have a real configuration;
// diagnostics cover the other active files in its packages under that configuration.
func planDiagnosticSources(
	root string,
	files []string,
	host build.Context,
	ports []goPort,
	profiles []diagnosticProfile,
) ([]diagnosticSession, error) {
	root, err := filepath.Abs(root)
	if err != nil {
		return nil, err
	}
	host.BuildTags = nil
	sources := make([]diagnosticSource, 0, len(files))
	modules := make(map[string]string)
	for _, file := range slices.Sorted(slices.Values(files)) {
		directory := filepath.Dir(filepath.Join(root, filepath.FromSlash(file)))
		module, err := diagnosticModule(root, directory, modules)
		if err != nil {
			return nil, err
		}
		source, err := readDiagnosticSource(root, file, module)
		if err != nil {
			return nil, err
		}
		sources = append(sources, source)
	}
	ports = diagnosticPorts(host, ports)
	profiles = slices.Concat([]diagnosticProfile{{Name: "default"}}, profiles)
	var groups []diagnosticGroup
	covered := make(map[string]bool, len(sources))
	defaultCovered := make(map[string]bool, len(sources))
	for _, profile := range profiles {
		profileCovered := make(map[string]bool, len(sources))
		for _, port := range ports {
			config := diagnosticBuild{
				GOOS: port.goos, GOARCH: port.goarch, Tags: profile.Tags,
				CgoEnabled: port.goos == host.GOOS && port.goarch == host.GOARCH && host.CgoEnabled,
			}
			byModule := make(map[string]int)
			for _, source := range sources {
				if defaultCovered[source.path] || profileCovered[source.path] {
					continue
				}
				matched, err := source.matches(config.context(host))
				if err != nil {
					return nil, err
				}
				if !matched && profile.Name == "default" && source.commandFile {
					ignored := config.context(host)
					ignored.BuildTags = []string{"ignore"}
					matched, err = source.matches(ignored)
					if err != nil {
						return nil, err
					}
				}
				if !matched {
					continue
				}
				index, exists := byModule[source.module]
				if !exists {
					index = len(groups)
					byModule[source.module] = index
					groups = append(groups, diagnosticGroup{
						module: source.module, profile: profile.Name, build: config,
						packages: make(map[string]bool),
					})
				}
				group := &groups[index]
				group.sources = append(group.sources, source)
				group.packages[source.directory] = true
				covered[source.path] = true
				profileCovered[source.path] = true
				if profile.Name == "default" {
					defaultCovered[source.path] = true
				}
			}
		}
	}
	for _, source := range sources {
		if !covered[source.path] {
			return nil, fmt.Errorf("source %s has no supported diagnostic configuration; check its constraints or add a profile to %s", source.path, diagnosticProfilesPath)
		}
	}
	slices.SortStableFunc(groups, func(a, b diagnosticGroup) int {
		// Retain profile/port preference within each owning module.
		if a.module < b.module {
			return -1
		}
		if a.module > b.module {
			return 1
		}
		return 0
	})
	var plan []diagnosticSession
	for _, group := range groups {
		session, err := group.session(root, sources, host)
		if err != nil {
			return nil, err
		}
		plan = append(plan, session)
	}
	return plan, nil
}

func (group diagnosticGroup) session(root string, sources []diagnosticSource, host build.Context) (diagnosticSession, error) {
	directory, err := filepath.Rel(root, group.module)
	if err != nil {
		return diagnosticSession{}, err
	}
	session := diagnosticSession{Directory: filepath.ToSlash(directory), Profile: group.profile, Build: group.build}
	relative := func(path string) string {
		file, _ := filepath.Rel(group.module, filepath.Join(root, filepath.FromSlash(path)))
		return filepath.ToSlash(file)
	}
	selected := make(map[string]bool)
	for _, source := range group.sources {
		file := relative(source.path)
		session.OpenFiles = append(session.OpenFiles, file)
		selected[source.path] = true
	}
	for _, source := range sources {
		if source.module != group.module || !group.packages[source.directory] {
			continue
		}
		matched, err := source.matches(group.build.context(host))
		if err != nil {
			return diagnosticSession{}, err
		}
		if matched || selected[source.path] {
			session.DiagnosticFiles = append(session.DiagnosticFiles, relative(source.path))
		}
	}
	return session, nil
}
