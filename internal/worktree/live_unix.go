//go:build !windows

package worktree

import (
	"bufio"
	"bytes"
	"context"
	"os/exec"
	"strings"
	"time"
)

// processesIn lists the processes whose working directory is inside dir, as
// "<pid> <command>", using lsof — present on every macOS and on the Linux
// hosts Rein runs on. lsof exits 1 when some processes could not be read,
// which is normal for an unprivileged user, so its output is used either way.
func processesIn(dir string) ([]string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "lsof", "-w", "-a", "-d", "cwd", "-Fpcn").Output()
	if len(out) == 0 && err != nil {
		return nil, err
	}
	var procs []string
	var pid, cmd string
	sc := bufio.NewScanner(bytes.NewReader(out))
	for sc.Scan() {
		line := sc.Text()
		if line == "" {
			continue
		}
		switch line[0] {
		case 'p':
			pid, cmd = line[1:], ""
		case 'c':
			cmd = line[1:]
		case 'n':
			if inside(line[1:], dir) {
				procs = append(procs, strings.TrimSpace(pid+" "+cmd))
			}
		}
	}
	return procs, nil
}
