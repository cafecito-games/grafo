package updatecheck

import "fmt"

const (
	// repositoryURL is the published home of the releases this package reads.
	repositoryURL = "https://github.com/cafecito-games/grafo"
	// modulePath is the import path `go install` takes.
	modulePath = "github.com/cafecito-games/grafo/cmd/grafo"
	// caskName is the name the GoReleaser homebrew_casks block publishes.
	caskName = "grafo"
)

// Hint names the command that upgrades an install of the given method to
// version. executablePath is only used by the archive form, which has to say
// which file to replace; an empty path drops that clause.
func Hint(method Method, version, executablePath string) string {
	switch method {
	case MethodHomebrew:
		return "brew upgrade --cask " + caskName
	case MethodGoInstall:
		return "go install " + modulePath + "@latest"
	default:
		return archiveHint(version, executablePath)
	}
}

// archiveHint describes the manual upgrade. There is no safe command to print
// here: replacing the binary may need a privileged write, and grafo cannot know
// whether the reader wants it in the same place, so the asset and the release
// page are named and the decision is left with the reader.
func archiveHint(version, executablePath string) string {
	tag := Comparable(version)
	if tag == "" {
		tag = version
	}
	hint := fmt.Sprintf("download %s from %s/releases/tag/%s",
		archiveName(version), repositoryURL, tag)
	if executablePath != "" {
		hint += " and replace " + executablePath
	}
	return hint
}

// Notice is the two-line advisory printed after a command finishes. The first
// line states the fact, the second states the action, so a reader who already
// knows about the release can stop after one line.
func Notice(currentVersion, latestVersion string, method Method, executablePath string) string {
	return fmt.Sprintf("grafo: %s is available (you have %s)\ngrafo: update with: %s\n",
		Display(latestVersion), Display(currentVersion), Hint(method, latestVersion, executablePath))
}
