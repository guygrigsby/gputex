package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestChildEnvInjectsAndPreservesOverrides(t *testing.T) {
	f := filepath.Join(t.TempDir(), "env")
	if err := os.WriteFile(f, []byte(
		"# managed gputex env\n"+
			"MLFLOW_TRACKING_URI=http://bee:5000\n"+
			"ALREADY_SET=from_file\n"+
			"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GPUTEX_ENV_FILE", f)
	t.Setenv("ALREADY_SET", "from_env") // existing value must win

	got := map[string]string{}
	for _, kv := range childEnv() {
		if k, v, ok := splitKV(kv); ok {
			got[k] = v
		}
	}
	if got["MLFLOW_TRACKING_URI"] != "http://bee:5000" {
		t.Errorf("missing injected MLFLOW_TRACKING_URI, got %q", got["MLFLOW_TRACKING_URI"])
	}
	if got["ALREADY_SET"] != "from_env" {
		t.Errorf("file overrode an existing env var: ALREADY_SET=%q, want from_env", got["ALREADY_SET"])
	}
}

func TestChildEnvNoFileIsPassthrough(t *testing.T) {
	t.Setenv("GPUTEX_ENV_FILE", filepath.Join(t.TempDir(), "nope"))
	if len(childEnv()) != len(os.Environ()) {
		t.Errorf("missing env file should be a pure passthrough")
	}
}

func splitKV(kv string) (string, string, bool) {
	for i := 0; i < len(kv); i++ {
		if kv[i] == '=' {
			return kv[:i], kv[i+1:], true
		}
	}
	return "", "", false
}
