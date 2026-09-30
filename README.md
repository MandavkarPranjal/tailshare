# tailshare

A private, golink-style screen sharing service for your tailnet. Run one
binary on the machine whose screen you want to share, then open its MagicDNS
name in a browser and watch the screen live.

Frames are captured with [`grim`](https://codeberg.org/emersion/grim) on
Wayland (or ImageMagick's `import` on X11), encoded with `ffmpeg` and sent to
viewers as **WebRTC video**. The frame rate follows what the viewers'
connections can carry: it climbs when they have room and gives way before the
picture degrades. A browser that cannot do WebRTC gets the same frames as an
MJPEG stream instead. Viewers pick their own resolution from a ladder (360p to
1080p by default), and each rung is encoded on its own, so a viewer on a slow
link costs only themselves. Nothing is captured or encoded while nobody is
watching. In the default `tailnet` mode the process embeds a Tailscale node
(tsnet), so the stream is only reachable from your tailnet.

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
or `import` (X11) in `$PATH`, and the video path needs `ffmpeg`.

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

- `GET /` — full-viewport live viewer. MJPEG goes up underneath it first, so
  there is a picture within a frame or two and never a black flash, and WebRTC
  takes over the moment it presents a real frame. If WebRTC is unavailable,
  refused, or stops presenting frames, the page settles on MJPEG for good
  rather than flickering between the two. The header shows which path is
  running and, for WebRTC, the frame rate, the bitrate and how far behind the
  picture is.
- Quality picker — a `<select>` in the header listing the ladder, so a viewer on
  a phone can drop to 360p without the picture everybody else is watching
  suffering for it. Changing it tears the connection down and asks again, since
  the two are different streams on the server rather than one stream scaled. The
  choice is put in the URL (`?quality=720p`) so it survives a reload and can be
  shared. It is hidden when there is only one rung or no WebRTC, and disabled
  once the page has settled on MJPEG, where there is nothing to switch between.
- `POST /webrtc` — the signalling exchange: a WebRTC offer in, an answer out.
  There is no trickling; the answer carries every candidate. The offer says
  which rung the viewer wants.
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
-fps int          capture rate, and the ceiling for everything served from it
                  (default 30)
-quality int      JPEG quality of the captures 1-100 (default 60)
-scale float      capture scale factor, 0 = native (grim only)
-codec string     WebRTC video codec: auto, h264, vp8 or vp9 (default "auto" = h264)
-qualities string resolutions the viewer may pick between, smallest first
                  (default "360p, 480p, 720p, 1080p")
-max-fps int      highest WebRTC frame rate, never above -fps (default 60)
-min-fps int      lowest WebRTC frame rate (default 5)
-bitrate int      WebRTC bitrate budget in kbit/s at -max-fps, for the top rung
                  (default 4000)
-keyint int       seconds between keyframes (default 2)
-width int        widest picture to offer in pixels, 0 = as captured
-webrtc bool      serve WebRTC video; off leaves MJPEG as the only transport
-preview-width int  widest MJPEG fallback picture, 0 = as captured (default 640)
-preview-fps int    MJPEG fallback frame rate, captures are dropped to hold it
                  (default 10)
-preview-quality int  JPEG quality of the fallback, well below -quality (default 45)
-version          print version and exit
```

`-qualities` is one `ffmpeg` per rung, and a rung is only running while somebody
is watching it, so the cost of the ladder is what the viewers on it cost. Each
rung gets a share of `-bitrate` in proportion to how many pixels it has to
describe, and then keeps that share to itself: a viewer on a slow link who has
asked for 360p cannot pull down the picture somebody else is watching at 1080p.
The controller and the measurements are per rung for the same reason.

A rung taller than the capture is an upscale, which costs the machine as much to
encode as a downscale and puts a blurrier picture on the wire, so it is not
offered, and neither is one that would come out wider than `-width`. A screen too
small for the whole ladder still gets the smallest rung on offer rather than a
page with nothing to pick.

The defaults are not trying to squeeze the machine: 30 captures a second, 4
Mbit/s at the top of the ladder, and stopping entirely when nobody is watching.
Above that the controller takes over and gives ground on its own — a machine
that cannot capture or encode that fast simply runs below the target, because
the rate is a ceiling and not a promise. `-fps 60 -bitrate 32000` are there for
a machine that turns out to have the headroom, and nothing above 8 Mbit/s is
worth having on a screen that is mostly text.

`-max-fps` is not a way to get above `-fps`. Every rung is fed the same
captures, so a WebRTC stream cannot be handed frames that were never taken:
the encoder is told the rate it will really see, and the controller's ceiling
is the lower of the two. Asking for more than the captures deliver is quiet
and does nothing.

H.264 is the default because every browser takes it and it is decoded in
hardware more often than not. `vp8` and `vp9` are available; note that VP8
rendered black in the Chrome this was developed against, so treat it as
untested rather than as an alternative.

The MJPEG fallback is served a small, slow copy of the captures and not the
captures themselves. A 1080p JPEG at quality 60 is around 280 kB, which at 15
frames a second is 30-odd megabits a second — more than the WebRTC video it
stands in for, spent on a picture that is about to be thrown away. The copy is
made once and shared by every fallback viewer: at the defaults it is 640 px
wide, quality 45, about 17 kB a frame, which is 0.9-1.4 Mbit/s per viewer
depending on what the screen is doing. It costs a decode, a rescale and a
re-encode per published frame, which is why it is only ever paid while somebody
is watching that endpoint, and why the rate actually reached is a little under
`-preview-fps` on a busy screen: the reduction is not free, it is just much
cheaper than the bandwidth it saves. `-preview-fps 3` is a third of the work and
still a moving picture. The ladder does not apply to any of this — there is
nothing to pick on a fallback, and the picks would be about a picture that is
about to be replaced.

## How it works

```
grim/import ──JPEG──▶ capture loop ──▶ JPEG hub ──┬──▶ preview ──▶ /stream (MJPEG)
               │                                   │
               │            ┌── ffmpeg 360p ──▶ hub ──▶ peers on 360p
               └────────────┼── ffmpeg 720p ──▶ hub ──▶ peers on 720p
                            └── ffmpeg 1080p ─▶ hub ──▶ peers on 1080p
```

Every rung is fed the same captures and scales them to its own height, so the
capture rate is whichever is asked for most, and only the rungs with viewers
have an `ffmpeg` at all. The MJPEG endpoint is fed by the preview rather than
by the captures, so the fallback can be small and slow without holding the
capture rate up for the rungs above it.

Every hub keeps only the newest frame and gives each subscriber a one-slot
queue: slow clients drop stale frames instead of ever blocking the capture
loop.

`ffmpeg` is told its frame rate when it starts, and keeps it. That makes the
two decisions that follow the viewers' bandwidth very lopsided, because
everything the encoder does with a frame's share of the bitrate is worked out
from that number rather than from how fast frames actually arrive:

- **Frame rate** only reaches the captures. Capturing more slowly costs
  nothing at all, so this is the knob for the machine: when it cannot keep up
  with the rate asked for, the rate comes down and nobody's bandwidth is
  touched. The controller is told what the encoder actually managed to
  produce, so it can tell a slow machine from a slow network.
- **Bitrate** is the only knob for the network, and the expensive one, because
  changing it means restarting ffmpeg. A slower rate spends exactly as much
  total bandwidth as a faster one, just spread over fewer, larger frames, so
  the stream has to be made smaller the only way it can be. Cuts go through
  first, before the rate is touched, and increases are held back for a good
  while; each one costs everyone a moment of frozen picture. A restart is that
  rung's own: whoever is watching another resolution does not see it.

Each viewer connection carries its own congestion estimate (Google
congestion control) and its own RTT and loss, read from the reports it sends
back. The tightest of them decides, within a rung: one viewer on a slow link
holds the other viewers on their rung down rather than being the only one who
suffers. Rungs do not hold each other down, which is the point of having them.

`-bitrate` is therefore the budget for the whole share rather than for one
stream, and what a rung gets out of it is a share of that budget, scaled to the
pixels it has to describe — 1080p gets all of it, 360p about a ninth. Within a
rung it is then the rate the whole stream runs at, whatever frame rate it turns
out to run at: not per second of wall clock and not per frame. At 4000 kbit/s
every frame gets 4000000/30 bits to spend, so a stream that settles at 16 fps
sends 16 × 134 kbit rather than 16 × 4 Mbit. Raising `-bitrate` sharpens the
picture and raising `-max-fps` smooths it, at the cost of more bandwidth for the
same result. A rung will climb to twice its share and no further: a link with
headroom to spare is not worth spending on a screen that is mostly text.

Nobody chases a keyframe. A viewer that has just joined, or one that has lost
its way and asked for a new keyframe, gets back on at the next one, which is at
most `-keyint` seconds away. Restarting ffmpeg to hurry it along was tried and
removed: a new process takes longer to come up than the wait it was meant to
skip, and the restart shows up as a frozen picture for everyone watching. What
it did produce, every `-keyint` seconds, was a burst of traffic that made the
bandwidth estimate collapse and the bitrate follow it down.

That last effect is worth spelling out, because it is the reason this does not
use pion's leaky-bucket packet pacer, which is what its own bandwidth
estimation example reaches for. The pacer queues whatever it is handed and
sends it later at the rate it has been told, and it never drops anything. That
suits a file being read at a fixed rate. Here the encoder is ours, so when it
is set above what the estimate allows the difference does not go anywhere — it
piles up in the queue, the stream falls further and further behind the wall
clock, the delay the estimator can see grows with it, and the estimate
collapses in response. It ends with a frozen picture and the bitrate cut to
nothing. Writing straight through turns the same mistake into packet loss
instead, which the viewer reports back and the controller can act on.

## Development

```
mise run lint      # gofmt check + go vet + staticcheck
mise run test      # go test ./...
mise run fmt       # fail if gofmt would change files
mise run run       # tailshare -mode local
```

The capture test exercises the real `grim` when a Wayland session is
present and skips otherwise. The encoder and signalling tests need `ffmpeg`
and skip without it.

## Notes

- Screen capture is read-only: the stream cannot inject input.
- On locked sessions some compositors refuse grabs; tailshare logs the
  error and keeps retrying.
- GNOME/KDE Wayland need a portal-based backend (`xdg-desktop-portal`);
  only wlroots-style compositors (Hyprland, Sway, …) work with `grim`.
- H.264 cannot be described in an SDP offer until a keyframe has been
  encoded, since that is where the sequence and picture parameter sets come
  from. A viewer that arrives first gets the encoder started for it rather
  than a refusal.
- A browser's own offer never carries those parameter sets, so the answer
  has to put them there. pion builds its answer from the codecs it matched
  against the offer, which would otherwise echo the browser's line back with
  no `sprop-parameter-sets` in it — a browser then accepts the connection,
  receives every packet, and decodes nothing at all. The answer also has to
  offer the retransmission and keyframe feedback the browser asked for, for
  the same reason.
- Known gap: Chrome in the environment this was developed against accepts the
  connection, keeps asking for keyframes, and still presents no frames from the
  H.264 stream, so the viewer page stays on its MJPEG underlay. The
  packets, the SDP and the encoded access units have each been checked
  against ffmpeg and come out right, and a pion viewer decodes the same
  stream, so what is left is somewhere in the browser's handling rather than
  in the stream. `-codec vp8` is a way to try a different path; the page falls
  back to MJPEG for good if it does not help. What is *not* free is the rung
  the viewer asked for: tailshare still counts it as connected and keeps
  encoding it at its share, so a browser that never presents a frame is paid
  for at up to twice `-bitrate`. Picking a smaller rung is the workaround, and
  making the page tell the server when it has given up on a stream would be
  the real fix.
