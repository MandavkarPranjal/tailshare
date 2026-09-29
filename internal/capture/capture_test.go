package capture

import (
	"context"
	"os"
	"os/exec"
	"testing"
	"time"
)

func TestNewRejectsBadOptions(t *testing.T) {
	if _, err := New(Options{Backend: "wayland", Quality: 60}); err == nil {
		t.Fatal("want error for unknown backend")
	}
	if _, err := New(Options{Backend: "auto", Quality: 0}); err == nil {
		t.Fatal("want error for quality 0")
	}
}

func TestNewUnknownBackend(t *testing.T) {
	_, err := New(Options{Backend: "portal", Quality: 60})
	if err == nil {
		t.Fatal("want error for unknown backend")
	}
}

// TestGrimCapture captures a real frame when grim and a Wayland session are
// available; it otherwise skips.
func TestGrimCapture(t *testing.T) {
	if os.Getenv("WAYLAND_DISPLAY") == "" {
		t.Skip("no WAYLAND_DISPLAY")
	}
	if _, err := exec.LookPath("grim"); err != nil {
		t.Skip("grim not installed")
	}
	c, err := New(Options{Backend: "grim", Quality: 50})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	frame, err := c.Capture(ctx)
	if err != nil {
		t.Fatalf("capture: %v", err)
	}
	if len(frame) < 2 || frame[0] != 0xff || frame[1] != 0xd8 {
		t.Fatalf("output is not a JPEG (first bytes %x)", frame[:min(len(frame), 4)])
	}
	t.Logf("%s produced %d bytes", c.Name(), len(frame))
}
