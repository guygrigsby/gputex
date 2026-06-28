package main

import (
	"bufio"
	"os"
	"strings"
)

// envFilePath is gputex's managed environment file: KEY=VALUE lines (blank lines
// and # comments ignored). gputex injects these into every wrapped job, so a GPU
// job cannot run without them. This is how the metrics contract is guaranteed:
// you cannot acquire the card without also getting MLFLOW_TRACKING_URI, because
// both come through gputex. Root-owned (/etc/gputex/env), so a gated user cannot
// edit it. Overridable for tests via GPUTEX_ENV_FILE.
func envFilePath() string {
	if p := os.Getenv("GPUTEX_ENV_FILE"); p != "" {
		return p
	}
	return "/etc/gputex/env"
}

// childEnv returns the environment for a wrapped job: the current environment
// plus any key from the managed env file not already set. Existing values win,
// so an explicit override on the command still takes effect; the file only fills
// gaps (the common case: the agent's shell never set MLFLOW_TRACKING_URI).
func childEnv() []string {
	managed := readEnvFile(envFilePath())
	env := os.Environ()
	if len(managed) == 0 {
		return env
	}
	have := make(map[string]bool, len(env))
	for _, kv := range env {
		if i := strings.IndexByte(kv, '='); i > 0 {
			have[kv[:i]] = true
		}
	}
	for k, v := range managed {
		if !have[k] {
			env = append(env, k+"="+v)
		}
	}
	return env
}

func readEnvFile(path string) map[string]string {
	f, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer f.Close()
	out := map[string]string{}
	s := bufio.NewScanner(f)
	for s.Scan() {
		line := strings.TrimSpace(s.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		out[strings.TrimSpace(k)] = strings.TrimSpace(v)
	}
	return out
}
