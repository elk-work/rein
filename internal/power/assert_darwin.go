package power

import (
	"os"
	"os/exec"
	"strconv"
	"time"
)

// caffeinate is macOS's own command for holding a power assertion. Rein is
// built without cgo, so IOPMAssertionCreateWithName is out of reach, and this
// is the same assertion taken by a process Apple ships.
const caffeinate = "/usr/bin/caffeinate"

var platformStart StartFunc = startCaffeinate

// startCaffeinate holds PreventUserIdleSystemSleep (`-i`) until stopped. `-w`
// ties it to this process as well, so a Rein that crashes or is killed does
// not leave the Mac unable to idle to sleep: caffeinate lets go the moment
// Rein's pid exits. `-s` is left out on purpose: it applies only on AC power,
// where `-i` already keeps an open Mac from idle sleep.
func startCaffeinate() (func(), error) {
	cmd := exec.Command(caffeinate, "-i", "-w", strconv.Itoa(os.Getpid()))
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	exited := make(chan struct{})
	go func() {
		_ = cmd.Wait()
		close(exited)
	}()
	return func() {
		_ = cmd.Process.Signal(os.Interrupt)
		select {
		case <-exited:
		case <-time.After(2 * time.Second):
			_ = cmd.Process.Kill()
			<-exited
		}
	}, nil
}
