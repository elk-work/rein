package service

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"
)

// A systemctl binary can exist in a container without a user manager or bus.
// Query the manager itself before asking about an individual unit.
func userManagerAvailable() (bool, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "systemctl", "--user", "show-environment")
	cmd.Env = append(os.Environ(), "LC_ALL=C")
	out, err := cmd.CombinedOutput()
	if err == nil {
		return true, nil
	}
	if errors.Is(err, exec.ErrNotFound) {
		return false, nil
	}
	message := string(out)
	if strings.Contains(message, "Failed to connect to user scope bus") && (strings.Contains(message, ": No medium found") || strings.Contains(message, ": No such file or directory") || strings.Contains(message, ": Connection refused")) {
		return false, nil
	}
	for _, reason := range []string{"Failed to connect to bus: No such file or directory", "Failed to connect to bus: No medium found", "Failed to connect to bus: Host is down", "System has not been booted with systemd", "Failed to connect to bus: Connection refused"} {
		if strings.Contains(message, reason) {
			return false, nil
		}
	}
	// Do not publish show-environment output: it can contain credentials.
	return false, fmt.Errorf("service: query user service manager: %w", err)
}
