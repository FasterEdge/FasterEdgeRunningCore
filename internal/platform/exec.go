package platform

import (
	"context"
	"os/exec"
	"strings"
)

func lookPath(name string) (string, error) {
	return exec.LookPath(name)
}

func runCommand(ctx context.Context, name string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	out, err := cmd.Output()
	return strings.TrimSpace(string(out)), err
}
