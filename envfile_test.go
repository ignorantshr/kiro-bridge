package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadDotEnvFileParsesSupportedSyntax(t *testing.T) {
	path := writeTempEnvFile(t, `
# comment
KIRO_BRIDGE_PORT=19000
export KIRO_BRIDGE_AGENT="agent name"
KIRO_BRIDGE_API_KEY='secret token'
KIRO_BRIDGE_SHOW_TOOLS=1 # inline comment
UNQUOTED_VALUE=kept#hash
`)

	unsetEnvForTest(t, "KIRO_BRIDGE_PORT")
	unsetEnvForTest(t, "KIRO_BRIDGE_AGENT")
	unsetEnvForTest(t, "KIRO_BRIDGE_API_KEY")
	unsetEnvForTest(t, "KIRO_BRIDGE_SHOW_TOOLS")
	unsetEnvForTest(t, "UNQUOTED_VALUE")

	if err := loadDotEnvFile(path, true); err != nil {
		t.Fatalf("loadDotEnvFile: %v", err)
	}

	if got := os.Getenv("KIRO_BRIDGE_PORT"); got != "19000" {
		t.Fatalf("KIRO_BRIDGE_PORT = %q, want %q", got, "19000")
	}
	if got := os.Getenv("KIRO_BRIDGE_AGENT"); got != "agent name" {
		t.Fatalf("KIRO_BRIDGE_AGENT = %q, want %q", got, "agent name")
	}
	if got := os.Getenv("KIRO_BRIDGE_API_KEY"); got != "secret token" {
		t.Fatalf("KIRO_BRIDGE_API_KEY = %q, want %q", got, "secret token")
	}
	if got := os.Getenv("KIRO_BRIDGE_SHOW_TOOLS"); got != "1" {
		t.Fatalf("KIRO_BRIDGE_SHOW_TOOLS = %q, want %q", got, "1")
	}
	if got := os.Getenv("UNQUOTED_VALUE"); got != "kept#hash" {
		t.Fatalf("UNQUOTED_VALUE = %q, want %q", got, "kept#hash")
	}
}

func TestLoadDotEnvFileKeepsExistingEnvironment(t *testing.T) {
	path := writeTempEnvFile(t, "KIRO_BRIDGE_API_KEY=from-file\n")

	t.Setenv("KIRO_BRIDGE_API_KEY", "from-env")
	if err := loadDotEnvFile(path, true); err != nil {
		t.Fatalf("loadDotEnvFile: %v", err)
	}
	if got := os.Getenv("KIRO_BRIDGE_API_KEY"); got != "from-env" {
		t.Fatalf("KIRO_BRIDGE_API_KEY = %q, want %q", got, "from-env")
	}
}

func TestLoadDotEnvFileOptionalMissingFile(t *testing.T) {
	if err := loadDotEnvFile(filepath.Join(t.TempDir(), "missing.env"), false); err != nil {
		t.Fatalf("loadDotEnvFile: %v", err)
	}
}

func TestLoadDotEnvFileErrorsOnMalformedLine(t *testing.T) {
	path := writeTempEnvFile(t, "BROKEN_LINE\n")

	unsetEnvForTest(t, "BROKEN_LINE")
	err := loadDotEnvFile(path, true)
	if err == nil {
		t.Fatal("expected error")
	}
}

func TestLoadRuntimeEnvironmentRefreshesGlobals(t *testing.T) {
	path := writeTempEnvFile(t, `
KIRO_BRIDGE_VERBOSE=1
KIRO_BRIDGE_SHOW_TOOLS=1
KIRO_BRIDGE_ALL_IP=1
KIRO_BRIDGE_MAX_BODY=64
KIRO_BRIDGE_CONTEXT_WINDOW=4096
KIRO_BRIDGE_API_KEY=dotenv-secret
`)

	t.Setenv("KIRO_BRIDGE_ENV_FILE", path)
	unsetEnvForTest(t, "KIRO_BRIDGE_VERBOSE")
	unsetEnvForTest(t, "KIRO_BRIDGE_SHOW_TOOLS")
	unsetEnvForTest(t, "KIRO_BRIDGE_ALL_IP")
	unsetEnvForTest(t, "KIRO_BRIDGE_MAX_BODY")
	unsetEnvForTest(t, "KIRO_BRIDGE_CONTEXT_WINDOW")
	unsetEnvForTest(t, "KIRO_BRIDGE_API_KEY")

	oldVerbose := verboseLog
	oldShowTools := showToolAnnotations
	oldAllIP := allIP
	oldMaxBody := maxBodyBytes
	oldContextWindow := contextWindow
	t.Cleanup(func() {
		verboseLog = oldVerbose
		showToolAnnotations = oldShowTools
		allIP = oldAllIP
		maxBodyBytes = oldMaxBody
		contextWindow = oldContextWindow
	})

	if err := loadRuntimeEnvironment(); err != nil {
		t.Fatalf("loadRuntimeEnvironment: %v", err)
	}

	if !verboseLog {
		t.Fatal("verboseLog = false, want true")
	}
	if !showToolAnnotations {
		t.Fatal("showToolAnnotations = false, want true")
	}
	if !allIP {
		t.Fatal("allIP = false, want true")
	}
	if maxBodyBytes != 64 {
		t.Fatalf("maxBodyBytes = %d, want 64", maxBodyBytes)
	}
	if contextWindow != 4096 {
		t.Fatalf("contextWindow = %d, want 4096", contextWindow)
	}
	if got := os.Getenv("KIRO_BRIDGE_API_KEY"); got != "dotenv-secret" {
		t.Fatalf("KIRO_BRIDGE_API_KEY = %q, want %q", got, "dotenv-secret")
	}
}

func writeTempEnvFile(t *testing.T, contents string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), ".env")
	if err := os.WriteFile(path, []byte(contents), 0o644); err != nil {
		t.Fatalf("write env file: %v", err)
	}
	return path
}

func unsetEnvForTest(t *testing.T, key string) {
	t.Helper()
	oldValue, hadValue := os.LookupEnv(key)
	if err := os.Unsetenv(key); err != nil {
		t.Fatalf("unset %s: %v", key, err)
	}
	t.Cleanup(func() {
		var err error
		if hadValue {
			err = os.Setenv(key, oldValue)
		} else {
			err = os.Unsetenv(key)
		}
		if err != nil {
			t.Fatalf("restore %s: %v", key, err)
		}
	})
}
