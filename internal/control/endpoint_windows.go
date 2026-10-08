//go:build windows

package control

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"path/filepath"
	"strings"

	"github.com/Microsoft/go-winio"
	"golang.org/x/sys/windows"
)

// Endpoint is the control endpoint for a Rein home — a named pipe, because
// Windows has no unix sockets and the loopback alternative is worse: a TCP
// port plus a shared-secret file, where the file would be readable by every
// other user on the box (Go's 0600 sets the read-only attribute on Windows,
// not an ACL) and the port would be reachable by every process on it.
//
// A pipe name cannot contain a path separator, so the home is hashed into it.
// The hash also isolates two Rein homes on one machine from each other, which
// is what `$REIN_HOME` promises everywhere else. It is lowercased and cleaned
// first because Windows paths are case-insensitive and the same home reached
// two ways must be the same endpoint.
func Endpoint(home string) string {
	sum := sha256.Sum256([]byte(strings.ToLower(filepath.Clean(home))))
	return `\\.\pipe\rein-control-` + hex.EncodeToString(sum[:8])
}

// Listen creates the pipe.
//
// The security descriptor is left empty on purpose: winio then creates the
// pipe with the process token's default DACL, which grants the user who
// started `rein run` and LocalSystem, and nobody else. That is exactly the
// audience — the service runs as the person whose keychain, vendor logins and
// repositories the runs use, and `rein attach` is that same person at a
// terminal. Writing an SDDL by hand here would be a second way to express the
// same thing and a first way to get it wrong.
//
// Unlike a unix socket there is no stale file to clear: a pipe exists only
// while its server process does, so a runner that was killed leaves nothing
// behind. A second runner on the same home fails to create it, which is
// [ErrInUse].
func Listen(home string) (net.Listener, error) {
	name := Endpoint(home)
	ln, err := winio.ListenPipe(name, &winio.PipeConfig{})
	if err != nil {
		if errors.Is(err, windows.ERROR_ACCESS_DENIED) || errors.Is(err, windows.ERROR_PIPE_BUSY) {
			return nil, fmt.Errorf("%w on %s", ErrInUse, name)
		}
		return nil, fmt.Errorf("control: listen on %s: %w", name, err)
	}
	return ln, nil
}

func dial(ctx context.Context, home string) (net.Conn, error) {
	return winio.DialPipeContext(ctx, Endpoint(home))
}
