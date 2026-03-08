package engine

import (
	"bufio"
	"fmt"
	"os"
	"strings"
)

// ValidateConfigFiles parses the snapraid configuration file and checks that
// all content and parity files exist and are non-zero in size.
func (e *Engine) ValidateConfigFiles() error {
	paths, err := parseConfigPaths(e.configPath)
	if err != nil {
		return fmt.Errorf("parsing snapraid config: %w", err)
	}

	for _, p := range paths {
		info, err := os.Stat(p.path)
		if err != nil {
			return fmt.Errorf("%s file %q: %w", p.kind, p.path, err)
		}
		if info.Size() == 0 {
			return fmt.Errorf("%s file %q exists but is empty (0 bytes)", p.kind, p.path)
		}
	}

	return nil
}

type configPath struct {
	kind string // "content" or "parity"
	path string
}

// parseConfigPaths reads a snapraid.conf and extracts content and parity file paths.
func parseConfigPaths(configFile string) ([]configPath, error) {
	f, err := os.Open(configFile)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	var paths []configPath
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}

		// Lines look like: "content /path/to/file" or "parity /path/to/file"
		// Also: "2-parity", "3-parity", etc.
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}

		keyword := fields[0]
		switch {
		case keyword == "content":
			paths = append(paths, configPath{kind: "content", path: fields[1]})
		case keyword == "parity" || strings.HasSuffix(keyword, "-parity"):
			paths = append(paths, configPath{kind: keyword, path: fields[1]})
		}
	}

	return paths, scanner.Err()
}
