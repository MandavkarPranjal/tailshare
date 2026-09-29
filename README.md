# tailshare

A private, golink-style screen sharing service for your tailnet. Run one
binary on the machine whose screen you want to share, then open its MagicDNS
name in a browser and watch the screen live.

Frames are captured with [`grim`](https://codeberg.org/emersion/grim) on
Wayland (or ImageMagick's `import` on X11) and streamed as MJPEG. In the
default `tailnet` mode the process embeds a Tailscale node (tsnet), so the
stream is only reachable from your tailnet.

## Build

With [mise](https://mise.jdx.dev) (pinned Go + staticcheck, all tasks in
`mise.toml`):

```
mise run build     # CGO_ENABLED=0, -trimpath, -ldflags '-s -w' → ./tailshare
mise run check     # gofmt check + go vet + staticcheck + go test
mise run           # same as check (default task)
```

Without mise:

```
CGO_ENABLED=0 go build -trimpath -ldflags='-s -w' -o tailshare ./cmd/tailshare
```

No cgo, no system Go libraries. The capture backend needs `grim` (Wayland)
or `import` (X11) in `$PATH`.

## Run

### tailnet mode (default)

```
export TS_AUTHKEY=tskey-auth-...   # only needed on first login
./tailshare
```

On first run without a key, an auth URL is printed and login completes in
the browser. State persists under `~/.config/tailshare-tsnet/` (override
with `-state-dir`), so the auth key is only needed once. The node advertises
the MagicDNS name `tailshare` by default, so viewers just open
`http://tailshare/`.

Already-logged-in tsnet state from another app can be reused:

```
./tailshare -state-dir ~/.config/shortlink-tsnet
```

Every viewer connection is logged with the caller's tailnet login name
(via the tsnet WhoIs API). Viewing is network-trusted: anyone on the tailnet
can watch, nobody can control anything.

### local mode

Plain HTTP on localhost, for development:

```
./tailshare -mode local
# Open http://127.0.0.1:8080/
```

## Viewer page

- `GET /` — full-viewport live viewer; reconnects automatically if the
  stream drops.
- `GET /stream` — raw MJPEG (`multipart/x-mixed-replace`), usable directly
  as an `<img>` src.

## Flags

```
-mode string      tailnet or local (default "tailnet")
-listen string    listen address (default ":80" tailnet, "127.0.0.1:8080" local)
-hostname string  tailnet: MagicDNS hostname to advertise (default "tailshare")
-ts-authkey string tailnet: auth key (default $TS_AUTHKEY; unused after first login)
-state-dir string tailnet: tsnet state directory (default under user config dir)
-capture string   capture backend: auto, grim, or x11 (default "auto")
-fps int          capture frame rate (default 5)
-quality int      JPEG quality 1-100 (default 60)
-scale float      capture scale factor, 0 = native (grim only)
-version          print version and exit
```

## How it works

```
grim/import ──JPEG──▶ capture loop ──▶ Hub (latest frame + fan-out)
                                          │
                     GET /stream ◀────────┘  multipart/x-mixed-replace
                     GET /     ◀── viewer page (img src=/stream)
```

The hub keeps only the newest frame and gives each viewer a one-slot queue:
slow clients drop stale frames instead of ever blocking the capture loop.

At 5 fps / quality 60 a 1080p frame is ~150 KB (~750 KB/s per viewer), and
`grim` needs ~30 ms per frame, so there is plenty of headroom. Lower `-fps`
or `-quality`, or set `-scale 0.5`, to reduce bandwidth.

## Development

```
mise run lint      # gofmt check + go vet + staticcheck
mise run test      # go test ./...
mise run fmt       # fail if gofmt would change files
mise run run       # tailshare -mode local
```

The capture test exercises the real `grim` when a Wayland session is
present and skips otherwise.

## Notes

- Screen capture is read-only: the stream cannot inject input.
- On locked sessions some compositors refuse grabs; tailshare logs the
  error and keeps retrying.
- GNOME/KDE Wayland need a portal-based backend (`xdg-desktop-portal`);
  only wlroots-style compositors (Hyprland, Sway, …) work with `grim`.
