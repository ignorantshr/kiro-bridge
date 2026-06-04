package main

import (
	"bufio"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
)

const (
	defaultDotEnvPath    = ".env"
	defaultMaxBodyBytes  = 1 << 20
	defaultContextWindow = 218000
)

// loadRuntimeEnvironment populates process env from an optional dotenv file and
// then refreshes all runtime-tunable globals that used to be read at init time.
func loadRuntimeEnvironment() error {
	path := strings.TrimSpace(os.Getenv("KIRO_BRIDGE_ENV_FILE"))
	required := path != ""
	if path == "" {
		path = defaultDotEnvPath
	}
	if err := loadDotEnvFile(path, required); err != nil {
		return err
	}
	loadRuntimeSettings()
	return nil
}

// loadRuntimeSettings snapshots environment-backed knobs after dotenv loading so
// the whole process observes a consistent configuration.
func loadRuntimeSettings() {
	verboseLog = os.Getenv("KIRO_BRIDGE_VERBOSE") != ""
	showToolAnnotations = os.Getenv("KIRO_BRIDGE_SHOW_TOOLS") != ""
	allIP = os.Getenv("KIRO_BRIDGE_ALL_IP") != ""
	maxBodyBytes = parseMaxBodyBytes()
	contextWindow = parseContextWindow()
}

func loadDotEnvFile(path string, required bool) error {
	f, err := os.Open(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) && !required {
			return nil
		}
		return fmt.Errorf("load dotenv %s: %w", path, err)
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 4096), 1024*1024)
	for lineNo := 1; scanner.Scan(); lineNo++ {
		key, value, ok, err := parseDotEnvLine(scanner.Text())
		if err != nil {
			return fmt.Errorf("load dotenv %s:%d: %w", path, lineNo, err)
		}
		if !ok {
			continue
		}
		if _, exists := os.LookupEnv(key); exists {
			continue
		}
		if err := os.Setenv(key, value); err != nil {
			return fmt.Errorf("set %s from dotenv: %w", key, err)
		}
	}
	if err := scanner.Err(); err != nil {
		return fmt.Errorf("read dotenv %s: %w", path, err)
	}
	return nil
}

func parseDotEnvLine(line string) (key, value string, ok bool, err error) {
	line = strings.TrimSpace(strings.TrimPrefix(line, "\uFEFF"))
	if line == "" || strings.HasPrefix(line, "#") {
		return "", "", false, nil
	}
	if strings.HasPrefix(line, "export ") {
		line = strings.TrimSpace(strings.TrimPrefix(line, "export "))
	}

	idx := strings.IndexRune(line, '=')
	if idx < 0 {
		return "", "", false, fmt.Errorf("missing '='")
	}

	key = strings.TrimSpace(line[:idx])
	if key == "" {
		return "", "", false, fmt.Errorf("missing variable name")
	}

	value, err = parseDotEnvValue(strings.TrimSpace(line[idx+1:]))
	if err != nil {
		return "", "", false, err
	}
	return key, value, true, nil
}

func parseDotEnvValue(raw string) (string, error) {
	if raw == "" {
		return "", nil
	}
	if raw[0] == '"' || raw[0] == '\'' {
		return parseQuotedDotEnvValue(raw)
	}
	if idx := indexUnquotedComment(raw); idx >= 0 {
		raw = raw[:idx]
	}
	return strings.TrimSpace(raw), nil
}

func parseQuotedDotEnvValue(raw string) (string, error) {
	quote := raw[0]
	end := -1
	escaped := false
	for i := 1; i < len(raw); i++ {
		ch := raw[i]
		if quote == '"' && ch == '\\' && !escaped {
			escaped = true
			continue
		}
		if ch == quote && (!escaped || quote == '\'') {
			end = i
			break
		}
		escaped = false
	}
	if end < 0 {
		return "", fmt.Errorf("unterminated quoted value")
	}

	rest := strings.TrimSpace(raw[end+1:])
	if rest != "" && !strings.HasPrefix(rest, "#") {
		return "", fmt.Errorf("unexpected trailing content after quoted value")
	}

	if quote == '"' {
		value, err := strconv.Unquote(raw[:end+1])
		if err != nil {
			return "", fmt.Errorf("invalid quoted value: %w", err)
		}
		return value, nil
	}
	return raw[1:end], nil
}

func indexUnquotedComment(raw string) int {
	for i := 0; i < len(raw); i++ {
		if raw[i] == '#' && (i == 0 || raw[i-1] == ' ' || raw[i-1] == '\t') {
			return i
		}
	}
	return -1
}

func parseMaxBodyBytes() int64 {
	if v := os.Getenv("KIRO_BRIDGE_MAX_BODY"); v != "" {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil && n > 0 {
			return n
		}
	}
	return defaultMaxBodyBytes
}
