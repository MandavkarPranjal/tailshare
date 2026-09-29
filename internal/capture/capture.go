// Package capture grabs single frames of the host's screen as JPEG bytes.
package capture

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
)

// Capturer produces one JPEG frame of the screen per Capture call.
type Capturer interface {
	// Name identifies the backend, e.g. "grim (wayland)".
	Name() string
	// Capture returns a single JPEG frame.
	Capture(ctx context.Context) ([]byte, error)
}

// Options configure a Capturer.
type Options struct {
	// Backend is "auto", "grim", or "x11".
	Backend string
	// Quality is the JPEG quality, 1-100.
	Quality int
	// Scale is an output scale factor; 0 means native resolution.
	Scale float64
}

// New picks a capturer for the current session.
func New(opts Options) (Capturer, error) {
	if opts.Quality < 1 || opts.Quality > 100 {
		return nil, fmt.Errorf("quality must be 1-100, got %d", opts.Quality)
	}
	switch opts.Backend {
	case "auto":
		if os.Getenv("WAYLAND_DISPLAY") != "" && lookPath("grim") {
			return newGrim(opts), nil
		}
		if os.Getenv("DISPLAY") != "" && lookPath("import") {
			return newX11(opts), nil
		}
		return nil, fmt.Errorf("no capture backend found: need grim (wayland) or import (x11) in $PATH" +
			" and a running display (WAYLAND_DISPLAY/DISPLAY)")
	case "grim":
		if !lookPath("grim") {
			return nil, fmt.Errorf("grim not found in $PATH")
		}
		return newGrim(opts), nil
	case "x11":
		if !lookPath("import") {
			return nil, fmt.Errorf("import not found in $PATH")
		}
		return newX11(opts), nil
	default:
		return nil, fmt.Errorf("unknown -capture %q (want auto, grim, or x11)", opts.Backend)
	}
}

func lookPath(bin string) bool {
	_, err := exec.LookPath(bin)
	return err == nil
}

// grim captures via the Wayland compositor (wlroots screencopy), writing JPEG
// straight to stdout.
type grim struct {
	opts Options
}

func newGrim(opts Options) Capturer { return &grim{opts: opts} }

func (g *grim) Name() string { return "grim (wayland)" }

func (g *grim) Capture(ctx context.Context) ([]byte, error) {
	args := []string{"-t", "jpeg", "-q", strconv.Itoa(g.opts.Quality)}
	if g.opts.Scale > 0 {
		args = append(args, "-s", strconv.FormatFloat(g.opts.Scale, 'g', -1, 64))
	}
	args = append(args, "-")
	return run(ctx, "grim", args...)
}

// x11 captures the root window with ImageMagick's import.
type x11 struct {
	opts Options
}

func newX11(opts Options) Capturer { return &x11{opts: opts} }

func (x *x11) Name() string { return "import (x11)" }

func (x *x11) Capture(ctx context.Context) ([]byte, error) {
	args := []string{"-window", "root", "-quality", strconv.Itoa(x.opts.Quality), "jpeg:-"}
	return run(ctx, "import", args...)
}

func run(ctx context.Context, bin string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, bin, args...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		msg := truncate(stderr.String())
		if msg != "" {
			return nil, fmt.Errorf("%s: %w: %s", bin, err, msg)
		}
		return nil, fmt.Errorf("%s: %w", bin, err)
	}
	if stdout.Len() == 0 {
		return nil, fmt.Errorf("%s: empty output", bin)
	}
	return stdout.Bytes(), nil
}

func truncate(s string) string {
	const max = 200
	s = strings.TrimSpace(s)
	if len(s) > max {
		return s[:max] + "…"
	}
	return s
}
