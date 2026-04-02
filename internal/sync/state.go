package sync

import (
	"bufio"
	"os"
	"strings"

	"gopkg.in/yaml.v3"
)

// prevState holds the fields we need from an existing issue file to detect state transitions.
type prevState struct {
	State  string `yaml:"state"`
	Type   string `yaml:"type"`
	Merge  struct {
		Merged bool `yaml:"merged"`
	} `yaml:"merge"`
}

// readPrevState reads the YAML frontmatter from an existing issue markdown file.
// Returns nil if the file does not exist.
func readPrevState(path string) *prevState {
	f, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)

	// Skip opening ---
	if !scanner.Scan() || strings.TrimSpace(scanner.Text()) != "---" {
		return nil
	}

	var lines []string
	for scanner.Scan() {
		line := scanner.Text()
		if strings.TrimSpace(line) == "---" {
			break
		}
		lines = append(lines, line)
	}

	var ps prevState
	if err := yaml.Unmarshal([]byte(strings.Join(lines, "\n")), &ps); err != nil {
		return nil
	}
	return &ps
}
