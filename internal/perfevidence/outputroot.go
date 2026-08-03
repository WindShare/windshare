package perfevidence

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

const maximumRepositoryContainmentObjects = 250_000

type directoryIdentity struct {
	volume uint64
	object uint64
}

func validateEvidenceOutputRoot(
	ctx context.Context,
	runner CommandRunner,
	repositoryRoot string,
	outputRoot string,
	runID string,
) error {
	authority, err := openOutputRootAuthority(outputRoot)
	if err != nil {
		return err
	}
	validationErr := validateEvidenceOutputAuthority(ctx, runner, repositoryRoot, authority, runID)
	return errors.Join(validationErr, authority.close())
}

func validateEvidenceOutputAuthority(
	ctx context.Context,
	runner CommandRunner,
	repositoryRoot string,
	authority *outputRootAuthority,
	runID string,
) error {
	if !validRunID(runID) {
		return fmt.Errorf("performance run ID %q is not path-safe", runID)
	}
	outputRelative, inside, repository, err := physicalRepositoryRelative(repositoryRoot, authority)
	if err != nil {
		return fmt.Errorf("validate evidence output authority: %w", err)
	}
	if !inside {
		return nil
	}
	if outputRelative == "." {
		return errors.New("evidence output root must not be the repository root")
	}
	// Git's ignore contract is attached to the exact retained output object.
	// Physical identity traversal makes aliases such as SUBST drives and bind
	// mounts obey the same policy as their repository-native spelling.
	probes := []string{
		filepath.ToSlash(outputRelative) + "/",
		filepath.Join(outputRelative, ".staging-"+runID, snapshotDirectoryName, "workspace", "go.mod"),
		filepath.Join(outputRelative, ".runtime-"+runID, stageOwnerName),
		filepath.Join(outputRelative, strings.Repeat("a", 64), payloadName),
	}
	for _, probe := range probes {
		result, runErr := runner.Run(ctx, Command{
			Executable: "git",
			Arguments: []string{
				"-C", repository, "check-ignore", "--quiet", "--no-index", "--", filepath.ToSlash(probe),
			},
		})
		if result.ExitCode == 1 {
			return fmt.Errorf(
				"evidence output root inside the repository must be Git-ignored; %s is visible to Git",
				filepath.ToSlash(probe),
			)
		}
		if runErr != nil || result.ExitCode != 0 {
			return commandFailure("verify evidence output ignore authority", result, runErr)
		}
	}
	return authority.verifyPath()
}

func physicalRepositoryRelative(
	repositoryRoot string,
	authority *outputRootAuthority,
) (string, bool, string, error) {
	if authority == nil {
		return "", false, "", errors.New("evidence output authority is nil")
	}
	if err := authority.verifyPath(); err != nil {
		return "", false, "", err
	}
	repository, err := resolveDirectoryAuthority(repositoryRoot)
	if err != nil {
		return "", false, "", fmt.Errorf("resolve repository authority: %w", err)
	}
	repositoryDirectories, repositoryIdentity, err := repositoryDirectoryIdentities(repository)
	if err != nil {
		return "", false, "", fmt.Errorf("index repository directory authorities: %w", err)
	}
	current := authority.path
	var components []string
	for {
		identity, identityErr := directoryIdentityAt(current)
		if identityErr != nil {
			return "", false, "", fmt.Errorf("identify output ancestor %s: %w", current, identityErr)
		}
		if repositoryRelative, found := repositoryDirectories[identity]; found {
			relativeParts := append([]string(nil), components...)
			if repositoryRelative != "." {
				relativeParts = append([]string{repositoryRelative}, relativeParts...)
			}
			relative := "."
			if len(relativeParts) != 0 {
				relative = filepath.Join(relativeParts...)
			}
			stable, stableErr := directoryIdentityAt(repository)
			if stableErr != nil || stable != repositoryIdentity {
				return "", false, "", errors.New("repository authority changed during containment validation")
			}
			return relative, true, repository, authority.verifyPath()
		}
		parent := filepath.Dir(current)
		if samePath(parent, current) {
			return "", false, repository, authority.verifyPath()
		}
		components = append([]string{filepath.Base(current)}, components...)
		current = parent
	}
}

func repositoryDirectoryIdentities(
	repository string,
) (map[directoryIdentity]string, directoryIdentity, error) {
	rootIdentity, err := directoryIdentityAt(repository)
	if err != nil {
		return nil, directoryIdentity{}, err
	}
	identities := make(map[directoryIdentity]string)
	err = walkBoundedTree(
		repository, maximumRepositoryContainmentObjects, maximumSnapshotInputDepth,
		func(path, relative string, info os.FileInfo) (bool, error) {
			if isReparsePointInfo(info) {
				return false, nil
			}
			if !info.IsDir() {
				return false, nil
			}
			identity, err := directoryIdentityAt(path)
			if err != nil {
				return false, err
			}
			if _, duplicate := identities[identity]; duplicate && !samePath(path, repository) {
				// Bind-mounted cycles and aliases cannot add a new containment
				// boundary after their inode has already been indexed.
				return false, nil
			}
			identities[identity] = relative
			return true, nil
		})
	if err != nil {
		return nil, directoryIdentity{}, err
	}
	stable, err := directoryIdentityAt(repository)
	if err != nil || stable != rootIdentity {
		return nil, directoryIdentity{}, errors.Join(
			errors.New("repository authority changed during directory identity indexing"), err,
		)
	}
	return identities, rootIdentity, nil
}
