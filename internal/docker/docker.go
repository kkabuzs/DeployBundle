package docker

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
)

type ImageInspect struct {
	ID           string   `json:"Id"`
	RepoDigests  []string `json:"RepoDigests"`
	Architecture string   `json:"Architecture"`
	OS           string   `json:"Os"`
	RootFS       struct {
		Layers []string `json:"Layers"`
	} `json:"RootFS"`
}

func Inspect(ctx context.Context, dockerBin, ref string) (ImageInspect, error) {
	cmd := exec.CommandContext(ctx, dockerBin, "image", "inspect", ref)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return ImageInspect{}, fmt.Errorf("docker image inspect %q failed: %w: %s", ref, err, stderr.String())
	}
	var list []ImageInspect
	if err := json.Unmarshal(stdout.Bytes(), &list); err != nil || len(list) != 1 {
		return ImageInspect{}, fmt.Errorf("unexpected docker image inspect output for %q", ref)
	}
	if list[0].ID == "" {
		return ImageInspect{}, fmt.Errorf("docker image inspect %q returned no image ID", ref)
	}
	return list[0], nil
}
