//go:build !windows

package control

import (
	"context"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"time"
)

// Endpoint is the control socket for a Rein home: <home>/run/control.sock.
//
// Under the Rein home rather than the platform's runtime directory, for the
// same reason config.Dir puts everything else there: one directory holds
// everything Rein owns, and `$REIN_HOME` then isolates a test or a second
// runner completely — endpoint included.
func Endpoint(home string) string {
	return filepath.Join(home, "run", "control.sock")
}

// dialTimeout bounds the probe for a live runner on a stale socket path.
const dialTimeout = 300 * time.Millisecond

// sunPathMax is the size of sockaddr_un.sun_path on this platform — 108 on
// Linux, 104 on macOS and the BSDs. A path has to fit in it WITH its
// terminator, so the usable length is one less.
//
// Chosen at run time rather than by build tag because it is one number, and
// three files would be three places to get it wrong.
var sunPathMax = func() int {
	if runtime.GOOS == "linux" {
		return 108
	}
	return 104
}()

// checkPathLength rejects a socket path the kernel will not take.
//
// It is checked rather than discovered because the kernel's answer is `EINVAL`
// — "invalid argument", with no mention of a length — which is exactly the
// error a person cannot act on. It is not hypothetical: a `$REIN_HOME` under a
// sandbox's or a test's scratch directory can be ninety characters before Rein
// adds anything, and the macOS CI runner's own TMPDIR gets within two bytes of
// the limit.
//
// The bound is [sunPathMax] and not a round number under it: guessing low
// turns a path the kernel would have accepted into a refusal, which is the
// same unhelpful failure the other way round. The path must fit in sun_path
// WITH its terminator, so the test is >=.
func checkPathLength(path string) error {
	if len(path) < sunPathMax {
		return nil
	}
	return fmt.Errorf("%w: the socket path is %d bytes, past the ~%d a unix socket allows: %s\n"+
		"Set $REIN_HOME to something shorter — the endpoint lives under it, so a long home has nowhere to go",
		ErrEndpoint, len(path), sunPathMax, path)
}

// Listen opens the control endpoint, replacing a socket file left behind by a
// runner that did not exit cleanly.
//
// A socket file on disk proves nothing — a killed process leaves one — so the
// test for "is someone already serving this home" is a connection, not a stat.
// If something answers, this is [ErrInUse] and the caller must not steal it;
// if nothing does, the file is debris and is removed.
//
// The directory is 0700 and the socket 0600: on Linux the permission bits on a
// unix socket are what gate connecting to it, and on macOS they are advisory,
// so the directory carries the guarantee on both.
func Listen(home string) (net.Listener, error) {
	path := Endpoint(home)
	if err := checkPathLength(path); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, fmt.Errorf("control: create %s: %w", filepath.Dir(path), err)
	}
	if c, err := net.DialTimeout("unix", path, dialTimeout); err == nil {
		c.Close()
		return nil, fmt.Errorf("%w on %s", ErrInUse, path)
	}
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return nil, fmt.Errorf("control: clear %s: %w", path, err)
	}
	ln, err := net.Listen("unix", path)
	if err != nil {
		return nil, fmt.Errorf("control: listen on %s: %w", path, err)
	}
	if err := os.Chmod(path, 0o600); err != nil {
		ln.Close()
		return nil, fmt.Errorf("control: secure %s: %w", path, err)
	}
	return ln, nil
}

func dial(ctx context.Context, home string) (net.Conn, error) {
	path := Endpoint(home)
	if err := checkPathLength(path); err != nil {
		return nil, err
	}
	var d net.Dialer
	return d.DialContext(ctx, "unix", path)
}
