package worktree

import (
	"strconv"
	"strings"
	"time"
)

// Build daemons a finished run leaves behind.
//
// A build tool's daemon is started by the agent's build, takes the worktree as
// its working directory and then outlives the session by hours, waiting for a
// next build that never comes. On 2026-11-20 one Gradle daemon kept a finished
// Android run's 835 MB worktree on fij-claude: every reap, and every retry of
// it, found "1 process(es) still working" there and left it (ark:rein#80).
//
// So before a reap decides whether a worktree is in use, the build daemons
// rooted in it are stopped. Only those: a process is stopped when its working
// directory is inside the worktree, it belongs to the user Rein runs as, and
// its command line is one of [buildDaemons]. Anything else found there — a
// background shell, a test still running, a helper agent — is work, and still
// stops the reap exactly as before.
//
// `./gradlew --stop` is deliberately not how: it stops every daemon of that
// Gradle version the user has, including the one a run in another worktree is
// building with right now. Signalling by process id reaches nothing outside
// this worktree.

// buildDaemons maps a marker in a process's command line to what to call it.
// Each is a main class no ordinary program carries, so a match is the daemon
// and nothing else.
var buildDaemons = []struct{ marker, name string }{
	{"org.gradle.launcher.daemon.bootstrap.GradleDaemon", "Gradle daemon"},
	{"org.gradle.process.internal.worker.GradleWorkerMain", "Gradle worker"},
	{"org.jetbrains.kotlin.daemon.KotlinCompileDaemon", "Kotlin compile daemon"},
}

// buildDaemonName names the build daemon a command line belongs to, or "".
func buildDaemonName(commandLine string) string {
	for _, d := range buildDaemons {
		if strings.Contains(commandLine, d.marker) {
			return d.name
		}
	}
	return ""
}

// daemonStopGrace is how long a daemon has to exit after being asked before
// it is killed. A Gradle daemon asked to stop exits in about a second.
const daemonStopGrace = 10 * time.Second

// stopBuildDaemons stops the build daemons whose working directory is inside
// dir and returns what it stopped, as "<name> (pid N)". It never fails a reap:
// whatever it could not stop is still there for [Manager.checkLive] to find.
func (m *Manager) stopBuildDaemons(dir string) []string {
	probe := m.LiveProcesses
	if probe == nil {
		probe = processesIn
	}
	procs, err := probe(dir)
	if err != nil || len(procs) == 0 {
		return nil
	}
	var stopped []string
	for _, p := range procs {
		pid, err := strconv.Atoi(strings.SplitN(p, " ", 2)[0])
		if err != nil || pid <= 1 || pid == selfPID {
			continue
		}
		mine, commandLine, err := processOwnerAndCommand(pid)
		if err != nil || !mine {
			continue
		}
		name := buildDaemonName(commandLine)
		if name == "" {
			continue
		}
		if err := stopProcess(pid, daemonStopGrace); err != nil {
			m.log("worktree: could not stop the %s (pid %d) left in %s: %v", name, pid, dir, err)
			continue
		}
		m.log("worktree: stopped the %s (pid %d) a finished run left in %s", name, pid, dir)
		stopped = append(stopped, name+" (pid "+strconv.Itoa(pid)+")")
	}
	return stopped
}
