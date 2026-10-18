package browser

import (
	"archive/zip"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
)

// Install downloads Chrome for Testing [Version] for this platform into
// <reinHome>/browsers/<version>/ and prunes every other version. It is
// idempotent: a complete copy already on disk is left alone and only the
// pruning runs. progress, when set, gets one line per step.
//
// Go's HTTP client sets no quarantine attribute, so macOS runs the copy
// without a Gatekeeper prompt; Chrome for Testing is Google-signed either way.
func Install(ctx context.Context, reinHome string, progress io.Writer) (string, error) {
	return install(ctx, http.DefaultClient, reinHome, Version, Platform(), URL, progress)
}

func install(ctx context.Context, client *http.Client, reinHome, version, plat string,
	urlFor func(version, plat string) string, progress io.Writer) (string, error) {
	say := func(format string, args ...any) {
		if progress != nil {
			fmt.Fprintf(progress, format+"\n", args...)
		}
	}
	if plat == "" {
		return "", errors.New("browser: Chrome for Testing has no build for this platform")
	}
	root := Dir(reinHome)
	dest := filepath.Join(root, version)
	exe := filepath.Join(dest, executableIn(plat))
	if _, err := os.Stat(filepath.Join(dest, completeMarker)); err == nil && isFile(exe) {
		say("Chrome for Testing %s is already installed: %s", version, exe)
		prune(root, version, say)
		return exe, nil
	}
	if err := os.MkdirAll(root, 0o755); err != nil {
		return "", err
	}

	url := urlFor(version, plat)
	say("downloading Chrome for Testing %s for %s from %s", version, plat, url)
	tmp, err := os.CreateTemp(root, ".download-*.zip")
	if err != nil {
		return "", err
	}
	defer os.Remove(tmp.Name())
	defer tmp.Close()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", err
	}
	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("browser: download: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("browser: download %s: %s", url, resp.Status)
	}
	n, err := io.Copy(tmp, resp.Body)
	if err != nil {
		return "", fmt.Errorf("browser: download: %w", err)
	}
	say("downloaded %d MB; unpacking", n>>20)

	// Unpack beside the destination and rename into place, so a version
	// directory either is complete or does not exist.
	staging, err := os.MkdirTemp(root, ".unpack-")
	if err != nil {
		return "", err
	}
	defer os.RemoveAll(staging)
	if err := unzip(tmp.Name(), staging); err != nil {
		return "", fmt.Errorf("browser: unpack: %w", err)
	}
	if !isFile(filepath.Join(staging, executableIn(plat))) {
		return "", fmt.Errorf("browser: the archive has no %s", executableIn(plat))
	}
	if err := os.WriteFile(filepath.Join(staging, completeMarker), []byte(version+"\n"), 0o644); err != nil {
		return "", err
	}
	if err := os.RemoveAll(dest); err != nil {
		return "", err
	}
	if err := os.Rename(staging, dest); err != nil {
		return "", err
	}
	say("installed Chrome for Testing %s: %s", version, exe)
	prune(root, version, say)
	return exe, nil
}

// prune removes every version directory but keep.
func prune(root, keep string, say func(string, ...any)) {
	entries, err := os.ReadDir(root)
	if err != nil {
		return
	}
	for _, e := range entries {
		if !e.IsDir() || e.Name() == keep || strings.HasPrefix(e.Name(), ".") {
			continue
		}
		if err := os.RemoveAll(filepath.Join(root, e.Name())); err == nil {
			say("removed Chrome for Testing %s", e.Name())
		}
	}
}

// unzip extracts archive into dir, keeping file modes and symlinks — a macOS
// app bundle's frameworks are symlinks, and a bundle unpacked without them does
// not launch. An entry or link that would land outside dir is refused.
func unzip(archive, dir string) error {
	zr, err := zip.OpenReader(archive)
	if err != nil {
		return err
	}
	defer zr.Close()
	inside := func(p string) bool {
		rel, err := filepath.Rel(dir, p)
		return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
	}
	for _, f := range zr.File {
		target := filepath.Join(dir, filepath.FromSlash(f.Name))
		if !inside(target) {
			return fmt.Errorf("entry %q escapes the archive", f.Name)
		}
		mode := f.Mode()
		switch {
		case mode.IsDir():
			if err := os.MkdirAll(target, 0o755); err != nil {
				return err
			}
		case mode&os.ModeSymlink != 0:
			link, err := readAll(f)
			if err != nil {
				return err
			}
			if filepath.IsAbs(link) || !inside(filepath.Join(filepath.Dir(target), filepath.FromSlash(link))) {
				return fmt.Errorf("link %q escapes the archive", f.Name)
			}
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				return err
			}
			if err := os.Symlink(link, target); err != nil {
				return err
			}
		default:
			if err := writeFile(f, target, mode.Perm()|0o600); err != nil {
				return err
			}
		}
	}
	return nil
}

func readAll(f *zip.File) (string, error) {
	rc, err := f.Open()
	if err != nil {
		return "", err
	}
	defer rc.Close()
	b, err := io.ReadAll(io.LimitReader(rc, 4096))
	return string(b), err
}

func writeFile(f *zip.File, target string, perm os.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		return err
	}
	rc, err := f.Open()
	if err != nil {
		return err
	}
	defer rc.Close()
	out, err := os.OpenFile(target, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, perm)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, rc); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}
