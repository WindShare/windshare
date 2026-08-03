//go:build linux || windows

package mutationdomain

import "path/filepath"

const privateRootDirectory = "private-mutation-domain"

func promotedArtifactName(semanticPath string) string {
	extension := filepath.Ext(filepath.Base(semanticPath))
	if len(extension) > 16 {
		return "artifact"
	}
	for _, character := range extension {
		if (character >= 'a' && character <= 'z') || (character >= 'A' && character <= 'Z') ||
			(character >= '0' && character <= '9') || character == '.' || character == '_' || character == '-' {
			continue
		}
		return "artifact"
	}
	return "artifact" + extension
}
