package main

import (
	"context"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"tailscale.com/client/local"
	"tailscale.com/tsnet"

	"tailshare/internal/web"
)

// setupTailnet embeds a Tailscale node (tsnet) and serves on the tailnet only.
func setupTailnet(ctx context.Context, cfg config) (*service, error) {
	dir := cfg.stateDir
	if dir == "" {
		base, err := os.UserConfigDir()
		if err != nil {
			return nil, fmt.Errorf("user config dir: %w", err)
		}
		dir = filepath.Join(base, "tailshare-tsnet")
	}

	authKey := cfg.authKey
	if authKey == "" {
		authKey = os.Getenv("TS_AUTHKEY")
	}

	s := &tsnet.Server{
		Dir:      dir,
		Hostname: cfg.hostname,
		AuthKey:  authKey,
		UserLogf: log.Printf,
	}

	// Up logs in (or prints an auth URL) and waits until the node is running.
	st, err := s.Up(ctx)
	if err != nil {
		s.Close()
		return nil, fmt.Errorf("tailscale login: %w", err)
	}

	addr := cfg.listen
	if addr == "" {
		addr = ":80"
	}
	ln, err := s.Listen("tcp", addr)
	if err != nil {
		s.Close()
		return nil, fmt.Errorf("tailscale listen: %w", err)
	}
	lc, err := s.LocalClient()
	if err != nil {
		s.Close()
		return nil, fmt.Errorf("tailscale local client: %w", err)
	}

	banner := []string{fmt.Sprintf(
		"Tailshare is on your tailnet at http://%s/ (state in %s)", cfg.hostname, dir)}
	if st != nil {
		if st.Self != nil && st.Self.DNSName != "" {
			banner = append(banner, "  Also reachable as http://"+strings.TrimSuffix(st.Self.DNSName, ".")+"/")
		}
		if len(st.TailscaleIPs) > 0 {
			banner = append(banner, "  Tailnet IPs: "+fmt.Sprint(st.TailscaleIPs))
		}
	}

	return &service{
		ln:           ln,
		identity:     whoIsIdentity(lc),
		closeBackend: func() error { return s.Close() },
		banner:       banner,
	}, nil
}

// whoIsIdentity resolves the caller via tailscaled's WhoIs API, using the
// user's login name as the viewer identity. Viewing is network-trusted: the
// identity is recorded for the log, not checked against an ACL.
func whoIsIdentity(lc *local.Client) web.IdentityFunc {
	return func(r *http.Request) web.Identity {
		resp, err := lc.WhoIs(r.Context(), r.RemoteAddr)
		if err != nil || resp == nil || resp.UserProfile == nil {
			return web.Identity{ID: hostOf(r.RemoteAddr)}
		}
		id := resp.UserProfile.LoginName
		if id == "" {
			id = resp.UserProfile.DisplayName
		}
		return web.Identity{ID: id}
	}
}

// hostOf strips the port from a remote address.
func hostOf(addr string) string {
	if host, _, err := net.SplitHostPort(addr); err == nil {
		return host
	}
	return addr
}
