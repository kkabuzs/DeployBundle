package compose

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
)

// Config asks the installed Docker Compose CLI to resolve a Compose file to JSON.
// Using the Compose CLI keeps YAML parsing and Compose interpolation semantics in
// the same implementation that will later start the package.
func Config(ctx context.Context, dockerBin, path, project, envFile string, profiles []string, noPathResolution bool) (map[string]any, error) {
	args := []string{"compose"}
	if envFile != "" {
		args = append(args, "--env-file", envFile)
	}
	if project != "" {
		args = append(args, "--project-name", project)
	}
	for _, profile := range profiles {
		args = append(args, "--profile", profile)
	}
	// Avoid persisting build-time defaults such as `name: nginx` and
	// `networks.default.name: nginx_default` into the package. The runner
	// supplies the project name, so Compose can derive those resources on the
	// deployment host. Explicit user-authored resources remain in the model.
	args = append(args, "-f", path, "config", "--format", "json", "--no-normalize")
	if noPathResolution {
		args = append(args, "--no-path-resolution")
	}
	cmd := exec.CommandContext(ctx, dockerBin, args...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("docker compose config failed: %w", err)
	}
	var config map[string]any
	if err := json.Unmarshal(stdout.Bytes(), &config); err != nil {
		return nil, fmt.Errorf("parse docker compose config JSON: %w", err)
	}
	if services, ok := config["services"].(map[string]any); !ok || len(services) == 0 {
		return nil, fmt.Errorf("compose config has no services object")
	}
	return config, nil
}
