package compose

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
)

// Config asks the installed Docker Compose CLI to resolve a Compose file to JSON.
// Using the Compose CLI keeps YAML parsing and Compose interpolation semantics in
// the same implementation that will later start the package.
func Config(ctx context.Context, dockerBin, path, project, envFile string) (map[string]any, error) {
	args := []string{"compose"}
	if envFile != "" {
		args = append(args, "--env-file", envFile)
	}
	if project != "" {
		args = append(args, "--project-name", project)
	}
	// Avoid persisting build-time defaults such as `name: nginx` and
	// `networks.default.name: nginx_default` into the package. The runner
	// supplies the project name, so Compose can derive those resources on the
	// deployment host. Explicit user-authored resources remain in the model.
	args = append(args, "-f", path, "config", "--format", "json", "--no-normalize")
	cmd := exec.CommandContext(ctx, dockerBin, args...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		// JSON files are useful in tests and for callers that already ran Compose
		// normalization. Keep this narrow fallback; ordinary YAML still requires
		// the Docker Compose CLI.
		if raw, readErr := os.ReadFile(path); readErr == nil {
			var config map[string]any
			if json.Unmarshal(raw, &config) == nil && config["services"] != nil {
				return config, nil
			}
		}
		return nil, fmt.Errorf("docker compose config failed: %w: %s", err, stderr.String())
	}
	var config map[string]any
	if err := json.Unmarshal(stdout.Bytes(), &config); err != nil {
		return nil, fmt.Errorf("parse docker compose config JSON: %w", err)
	}
	if _, ok := config["services"].(map[string]any); !ok {
		return nil, fmt.Errorf("compose config has no services object")
	}
	return config, nil
}
