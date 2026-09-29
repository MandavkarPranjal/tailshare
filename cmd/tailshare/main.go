// Command tailshare shares this machine's screen over HTTP on your tailnet.
//
// It captures frames of the local display (grim on Wayland, ImageMagick's
// import on X11) and serves them as an MJPEG stream to any browser that can
// reach the listener. In the default tailnet mode it runs as an embedded
// Tailscale node (tsnet), so the viewer URL is just your MagicDNS name.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"tailshare/internal/capture"
	"tailshare/internal/stream"
	"tailshare/internal/web"
)

const version = "0.1.0"

type config struct {
	mode     string
	listen   string
	hostname string
	authKey  string
	stateDir string
	capture  string
	fps      int
	quality  int
	scale    float64
}

// service is a mode-specific listener plus the identity function it implies.
type service struct {
	ln           net.Listener
	identity     web.IdentityFunc
	closeBackend func() error
	banner       []string
}

func main() {
	cfg := config{}
	showVersion := flag.Bool("version", false, "print version and exit")
	flag.StringVar(&cfg.mode, "mode", "tailnet", "service mode: tailnet or local")
	flag.StringVar(&cfg.listen, "listen", "", "listen address (default \":80\" tailnet, \"127.0.0.1:8080\" local)")
	flag.StringVar(&cfg.hostname, "hostname", "tailshare", "tailnet: MagicDNS hostname to advertise")
	flag.StringVar(&cfg.authKey, "ts-authkey", "", "tailnet: node auth key (default $TS_AUTHKEY; unused after first login)")
	flag.StringVar(&cfg.stateDir, "state-dir", "", "tailnet: tsnet state directory (default under the user config dir)")
	flag.StringVar(&cfg.capture, "capture", "auto", "screen capture backend: auto, grim, or x11")
	flag.IntVar(&cfg.fps, "fps", 5, "capture frame rate")
	flag.IntVar(&cfg.quality, "quality", 60, "JPEG quality (1-100)")
	flag.Float64Var(&cfg.scale, "scale", 0, "capture scale factor, 0 = native resolution (grim only)")
	flag.Parse()

	if *showVersion {
		fmt.Println("tailshare", version)
		return
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if err := run(ctx, cfg); err != nil {
		log.Fatalf("tailshare: %v", err)
	}
}

func run(ctx context.Context, cfg config) error {
	switch cfg.mode {
	case "tailnet", "local":
	default:
		return fmt.Errorf("unknown -mode %q (want tailnet or local)", cfg.mode)
	}
	if cfg.fps < 1 || cfg.fps > 60 {
		return fmt.Errorf("-fps must be between 1 and 60, got %d", cfg.fps)
	}

	grab, err := capture.New(capture.Options{
		Backend: cfg.capture,
		Quality: cfg.quality,
		Scale:   cfg.scale,
	})
	if err != nil {
		return err
	}
	// Probe once so a broken display/backend fails fast instead of log-spamming.
	if _, err := grab.Capture(ctx); err != nil {
		return fmt.Errorf("capture probe: %w", err)
	}

	var svc *service
	switch cfg.mode {
	case "local":
		svc, err = setupLocal(cfg)
	case "tailnet":
		svc, err = setupTailnet(ctx, cfg)
	}
	if err != nil {
		return err
	}
	if svc.closeBackend != nil {
		defer svc.closeBackend()
	}

	hub := stream.NewHub()
	go captureLoop(ctx, grab, hub, cfg.fps)

	pageBanner := fmt.Sprintf("%s capture · %d fps · quality %d · %s mode",
		grab.Name(), cfg.fps, cfg.quality, cfg.mode)
	h, err := web.New(web.Config{Hub: hub, Identity: svc.identity, Banner: pageBanner})
	if err != nil {
		return err
	}
	srv := &http.Server{Handler: h, ReadHeaderTimeout: 10 * time.Second}

	log.Printf("tailshare %s serving in %s mode on %s (%s, %d fps, quality %d)",
		version, cfg.mode, svc.ln.Addr(), grab.Name(), cfg.fps, cfg.quality)
	for _, line := range svc.banner {
		fmt.Fprintln(os.Stdout, line)
	}

	serveErr := make(chan error, 1)
	go func() { serveErr <- srv.Serve(svc.ln) }()

	select {
	case <-ctx.Done():
		log.Printf("shutting down")
	case err := <-serveErr:
		if !errors.Is(err, http.ErrServerClosed) {
			return err
		}
		return nil
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		return fmt.Errorf("shutdown: %w", err)
	}
	return nil
}

// captureLoop grabs frames at fps and publishes them until ctx is done.
// Capture failures are logged at most every few seconds (the compositor can
// refuse grabs transiently, e.g. while the session is locked).
func captureLoop(ctx context.Context, c capture.Capturer, hub *stream.Hub, fps int) {
	ticker := time.NewTicker(time.Second / time.Duration(fps))
	defer ticker.Stop()
	var lastLog time.Time
	for {
		frame, err := c.Capture(ctx)
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			if time.Since(lastLog) > 5*time.Second {
				log.Printf("capture: %v", err)
				lastLog = time.Now()
			}
		} else {
			hub.Publish(frame)
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// listenError explains the most common bind failure: ports below 1024 need
// extra privileges.
func listenError(addr string, err error) error {
	if !errors.Is(err, os.ErrPermission) {
		return fmt.Errorf("listen %s: %w", addr, err)
	}
	host, port, splitErr := net.SplitHostPort(addr)
	if splitErr != nil {
		return fmt.Errorf("listen %s: %w", addr, err)
	}
	p, convErr := strconv.Atoi(port)
	if convErr != nil || p >= 1024 {
		return fmt.Errorf("listen %s: %w", addr, err)
	}
	return fmt.Errorf("listen %s: %w (ports below 1024 need root or CAP_NET_BIND_SERVICE; "+
		"try `sudo setcap cap_net_bind_service=+ep tailshare` or -listen %s)",
		addr, err, net.JoinHostPort(host, "8080"))
}

// setupLocal binds a plain TCP listener (development / single-host mode).
func setupLocal(cfg config) (*service, error) {
	addr := cfg.listen
	if addr == "" {
		addr = "127.0.0.1:8080"
	}
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, listenError(addr, err)
	}
	_, port, err := net.SplitHostPort(ln.Addr().String())
	if err == nil && port != "" && port != "0" {
		addr = net.JoinHostPort("127.0.0.1", port)
	} else {
		addr = ln.Addr().String()
	}
	return &service{
		ln: ln,
		identity: func(*http.Request) web.Identity {
			return web.Identity{ID: "local"}
		},
		banner: []string{
			fmt.Sprintf("Open http://%s/ in a browser.", addr),
			"Local mode: plain TCP listener, no tailnet required.",
		},
	}, nil
}
