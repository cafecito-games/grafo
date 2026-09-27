package agentinstall

import (
	"fmt"
	"strings"
)

// pathSeparator returns the element separator for the target operating system.
// Platform rules are derived from the injected Reader rather than runtime.GOOS
// so that macOS, Linux and Windows layouts are all testable from one host.
func pathSeparator(goos string) string {
	if goos == "windows" {
		return `\`
	}
	return "/"
}

// joinPath joins path elements using the target platform's separator.
func joinPath(goos string, elements ...string) string {
	separator := pathSeparator(goos)
	parts := make([]string, 0, len(elements))
	for _, element := range elements {
		element = strings.Trim(element, "/\\")
		if element == "" {
			continue
		}
		parts = append(parts, element)
	}
	joined := strings.Join(parts, separator)
	if len(elements) > 0 {
		first := elements[0]
		if goos != "windows" && strings.HasPrefix(first, "/") {
			return "/" + joined
		}
	}
	return joined
}

// parentPath returns the directory containing path, or "" when there is none.
func parentPath(goos, path string) string {
	separator := pathSeparator(goos)
	index := strings.LastIndex(path, separator)
	if index <= 0 {
		return ""
	}
	return path[:index]
}

// homePath resolves a path relative to the user's home directory.
func homePath(reader Reader, elements ...string) (string, error) {
	home, err := reader.HomeDir()
	if err != nil {
		return "", fmt.Errorf("resolve home directory: %w", err)
	}
	if strings.TrimSpace(home) == "" {
		return "", fmt.Errorf("resolve home directory: empty path")
	}
	return joinPath(reader.GOOS(), append([]string{home}, elements...)...), nil
}

// appDataPath resolves a path inside the per-user application data directory:
// %APPDATA% on Windows, ~/Library/Application Support on macOS, and
// $XDG_CONFIG_HOME (falling back to ~/.config) elsewhere.
func appDataPath(reader Reader, elements ...string) (string, error) {
	goos := reader.GOOS()
	switch goos {
	case "windows":
		appData := strings.TrimSpace(reader.Getenv("APPDATA"))
		if appData == "" {
			home, err := reader.HomeDir()
			if err != nil {
				return "", fmt.Errorf("resolve APPDATA: %w", err)
			}
			appData = joinPath(goos, home, "AppData", "Roaming")
		}
		return joinPath(goos, append([]string{appData}, elements...)...), nil
	case "darwin":
		return homePath(reader, append([]string{"Library", "Application Support"}, elements...)...)
	default:
		if configHome := strings.TrimSpace(reader.Getenv("XDG_CONFIG_HOME")); configHome != "" {
			return joinPath(goos, append([]string{configHome}, elements...)...), nil
		}
		return homePath(reader, append([]string{".config"}, elements...)...)
	}
}

// vsCodeUserPath resolves a path inside VS Code's per-user profile directory.
// VS Code stores user data under Application Support on macOS, %APPDATA% on
// Windows, and ~/.config on Linux, always in a "Code/User" subtree.
func vsCodeUserPath(reader Reader, elements ...string) (string, error) {
	return appDataPath(reader, append([]string{"Code", "User"}, elements...)...)
}
