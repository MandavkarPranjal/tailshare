// Command tailshare shares this machine's screen over HTTP on your tailnet.
//
// It captures frames of the local display (grim on Wayland, ImageMagick's
// import on X11), encodes them with ffmpeg and serves them to any browser that
// can reach the listener. WebRTC is the default transport: the frames go out as
// video, and the frame rate follows what the viewers' connections can carry,
// climbing as soon as they can and giving way before the picture degrades. A
// browser that cannot do that gets an MJPEG stream instead, over the same
// endpoint, and that one is a small fixed rate copy of the captures rather than
// the captures themselves: it exists to hold the screen while WebRTC takes over,
// and at full size and rate it would cost more than the video it stands in for.
//
// Nothing is captured or encoded until somebody is watching, so a share with no
// viewers costs nothing.
//
// In the default tailnet mode it runs as an embedded Tailscale node (tsnet),
// so the viewer URL is just your MagicDNS name.
package main

import (
	"bytes"
	"context"
	"errors"
	"flag"
	"fmt"
	"image/jpeg"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"tailshare/internal/adapt"
	"tailshare/internal/capture"
	"tailshare/internal/encode"
	"tailshare/internal/preview"
	"tailshare/internal/quality"
	"tailshare/internal/rtc"
	"tailshare/internal/stream"
	"tailshare/internal/web"
)

const version = "0.3.0"

const (
	// grabWorkers is how many captures may be in flight at once. A single grim
	// process takes around 20ms, which is longer than the interval at 60fps, so
	// one capture at a time would put a ceiling on the rate well below what the
	// encoder can keep up with. Three is where the captures stop getting any
	// faster: the compositor is the limit, not the number of them.
	grabWorkers = 3

	// idleGrace is how long capture and encoding carry on after the last
	// viewer goes. Restarting ffmpeg for the next viewer would cost the wait
	// for a fresh keyframe, which is more than carrying on costs.
	idleGrace = 10 * time.Second

	// idlePoll is how long the capture loop sits still when nobody is
	// watching before looking again.
	idlePoll = 200 * time.Millisecond

	// controlInterval is how often the loops that watch the viewers look.
	controlInterval = time.Second

	// prepareTimeout is how long a viewer waits for a codec to become
	// describable before being turned away. One keyframe is all it takes.
	prepareTimeout = 3 * time.Second

	// minShareBitrate is the smallest share of the bitrate budget a rung can be
	// left with, in bits per second. It is well below the floor the rate
	// controller would pick for itself, because that floor is meant for a whole
	// stream and the smallest rung on the ladder is not one.
	minShareBitrate = 400_000
)

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

	codec     string
	qualities string
	maxFPS    int
	minFPS    int
	bitrate   int
	keyint    int
	width     int
	webrtc    bool

	previewWidth   int
	previewFPS     int
	previewQuality int
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
	flag.IntVar(&cfg.fps, "fps", 30, "capture frame rate, and the fixed rate MJPEG viewers are served at; the ceiling for WebRTC too")
	flag.IntVar(&cfg.quality, "quality", 60, "JPEG quality (1-100)")
	flag.Float64Var(&cfg.scale, "scale", 0, "capture scale factor, 0 = native resolution (grim only)")
	flag.StringVar(&cfg.codec, "codec", "auto", "WebRTC video codec: auto, h264, vp8 or vp9 (auto picks h264)")
	flag.StringVar(&cfg.qualities, "qualities", quality.Default.Names(), "comma separated resolutions the viewer may pick between, smallest first")
	flag.IntVar(&cfg.maxFPS, "max-fps", 60, "highest WebRTC frame rate (1-60), never above -fps; the rate goes below this on its own when a viewer cannot keep up")
	flag.IntVar(&cfg.minFPS, "min-fps", 5, "lowest WebRTC frame rate (1-60)")
	flag.IntVar(&cfg.bitrate, "bitrate", 4000, "WebRTC bitrate budget in kbit/s at -max-fps for the top of -qualities (default 4000); each rung gets a share of it and the rate sent scales with the frame rate")
	flag.IntVar(&cfg.keyint, "keyint", 2, "seconds between keyframes (0-10); a viewer waits this long for the first one")
	flag.IntVar(&cfg.width, "width", 0, "widest picture to offer in pixels, 0 = as captured; a rung that would come out wider is dropped")
	flag.BoolVar(&cfg.webrtc, "webrtc", true, "serve WebRTC video; off leaves the MJPEG stream as the only transport")
	flag.IntVar(&cfg.previewWidth, "preview-width", 640, "widest MJPEG fallback picture in pixels, 0 = as captured; never an upscale")
	flag.IntVar(&cfg.previewFPS, "preview-fps", 10, "MJPEG fallback frame rate (1-60); captures arriving faster are dropped rather than queued")
	flag.IntVar(&cfg.previewQuality, "preview-quality", 45, "JPEG quality of the MJPEG fallback (1-100), well below -quality because the size of the frame is what it costs")
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
	if cfg.width < 0 || cfg.width > 0 && cfg.width < 320 {
		return fmt.Errorf("-width must be 0 or 320-7680, got %d", cfg.width)
	}
	if cfg.previewWidth < 0 {
		return fmt.Errorf("-preview-width must not be negative, got %d", cfg.previewWidth)
	}
	if cfg.previewFPS < 1 || cfg.previewFPS > 60 {
		return fmt.Errorf("-preview-fps must be between 1 and 60, got %d", cfg.previewFPS)
	}
	if cfg.previewQuality < 1 || cfg.previewQuality > 100 {
		return fmt.Errorf("-preview-quality must be between 1 and 100, got %d", cfg.previewQuality)
	}
	// The ladder is parsed before anything is started rather than where the
	// encoders are built, so a typo in it does not cost a capture and a tailnet
	// login to find out about.
	ladder, err := quality.Parse(cfg.qualities)
	if err != nil {
		return err
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
	// The frame is kept for its size: how big the screen is decides which
	// resolutions can be served from it at all, and a rung taller than the
	// capture would be an upscale dressed up as a choice.
	probe, err := grab.Capture(ctx)
	if err != nil {
		return fmt.Errorf("capture probe: %w", err)
	}
	size, err := jpeg.DecodeConfig(bytes.NewReader(probe))
	if err != nil {
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

	// Two broadcasts off one capture: JPEG for the MJPEG endpoint and coded
	// video for WebRTC, at the rate each of them wants. The endpoint does not get
	// the captures themselves: a full size JPEG is most of the traffic this
	// serves, and it is the one viewer that is watching a picture that is about
	// to be replaced.
	jpeg := stream.NewHub()
	prev, err := preview.New(preview.Config{
		Source:  jpeg,
		Width:   cfg.previewWidth,
		Quality: cfg.previewQuality,
		FPS:     cfg.previewFPS,
	})
	if err != nil {
		return err
	}
	pipe := &pipeline{
		grab:     grab,
		jpeg:     jpeg,
		preview:  prev,
		mjpegFPS: cfg.previewFPS,
		capFPS:   cfg.fps,
		viewers:  make(chan map[string]int, 1),
		feedback: make(chan rtc.Feedback, 1),
		capErr:   &errLogger{what: "capture"},
	}
	go captureLoop(ctx, pipe)
	go prev.Run(ctx)

	pageBanner := fmt.Sprintf("%s capture · %s · %s mode",
		grab.Name(), prev, cfg.mode)
	transport := "MJPEG only"
	var (
		signalling http.Handler
		served     quality.Ladder
	)
	if cfg.webrtc {
		served = servedLadder(ladder, quality.Capture{Width: size.Width, Height: size.Height}, cfg.width)
		pageBanner, transport, err = pipe.startVideo(ctx, cfg, served)
		if err != nil {
			return err
		}
		defer pipe.stop()
		signalling, err = pipe.startSignalling(ctx, svc)
		if err != nil {
			return err
		}
	}
	// Started here rather than with the video, because the endpoint needs it too:
	// it is what decides whether anything is captured at all, and with
	// -webrtc=false nothing else ever would.
	go pipe.controlLoop(ctx)

	h, err := web.New(web.Config{
		Hub:       prev.Hub(),
		Identity:  svc.identity,
		Banner:    pageBanner,
		WebRTC:    signalling,
		Qualities: served,
	})
	if err != nil {
		return err
	}
	srv := &http.Server{Handler: h, ReadHeaderTimeout: 10 * time.Second}

	log.Printf("tailshare %s serving in %s mode on %s (%s, %s)",
		version, cfg.mode, svc.ln.Addr(), grab.Name(), transport)
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

// servedLadder is the ladder as this screen can actually serve it: the
// resolutions that fit inside the capture, and inside maxWidth, smallest first.
//
// A rung taller than the capture would be an upscale, and an upscale costs the
// machine as much to encode as a downscale while putting a blurrier picture on
// the wire, so it is not offered. -width rules the same way for the other
// dimension, which is all it has left to say now that the width of the stream is
// the viewer's choice rather than one number for everybody.
func servedLadder(ladder quality.Ladder, c quality.Capture, maxWidth int) quality.Ladder {
	fit := ladder.Fit(c, maxWidth)
	if len(fit) < len(ladder) {
		bound := fmt.Sprintf("a %dx%d capture", c.Width, c.Height)
		if maxWidth > 0 {
			bound += fmt.Sprintf(" and -width %d", maxWidth)
		}
		log.Printf("offering %s of %s: the rest do not fit %s", fit.Names(), ladder.Names(), bound)
	}
	return fit
}

// pipeline is the capture and encode path, along with the two decisions that
// keep it honest: how hard to work, and whether to work at all.
//
// Capture failures are logged at most every few seconds, since a compositor can
// refuse grabs transiently and would otherwise drown the log. The same applies
// to a viewer that has joined while the encoder is having trouble.
type pipeline struct {
	grab capture.Capturer
	jpeg *stream.Hub

	// preview is the cheap copy of the captures that the MJPEG endpoint is
	// served from. It costs nothing until somebody is watching it.
	preview *preview.Preview

	// mjpegFPS is the rate that endpoint is served at, and capFPS the ceiling
	// for the shared capture. They differ because the endpoint is a
	// placeholder: nothing about the picture it puts on screen is worth a
	// capture rate, and the rungs below it want the frames that are left over.
	mjpegFPS int
	capFPS   int

	// levels are the rungs of the ladder, smallest first, each encoded on its
	// own and steered by the viewers on it alone.
	levels []*qualityStream

	// fps is what the capture loop is to grab at right now, zero when there is
	// nothing to capture for. It is the fastest rate any one of the streams
	// wants, and every stream is fed from the same captures, so the highest
	// rung can be as sharp as the screen allows whichever rung is being
	// watched.
	fps atomic.Int64

	// The control loop is woken by these, and always re-reads the latest
	// value, so a value that arrives while another is still waiting is only
	// ever dropped, never left to be read twice as if it were new.
	viewers  chan map[string]int
	feedback chan rtc.Feedback

	capErr *errLogger
}

// qualityStream is one rung of the ladder: a resolution encoded on its own, from
// the same captures as every other rung, and answered only to the viewers that
// asked for it.
//
// The separation is the whole point. A viewer on a slow link who has asked for
// 360p has no business holding down the picture somebody else is watching at
// 1080p, and the cost of a rung is only paid while somebody is watching it. What
// it costs is real: every rung that is in use is a second ffmpeg being fed the
// full-size capture, which is why a rung with no viewers is not merely idle but
// stopped.
type qualityStream struct {
	level quality.Level
	video *stream.Hub
	enc   *encode.Encoder
	rate  *adapt.Controller
	err   *errLogger

	// ctx is the lifetime of the process. The encoder is started with it rather
	// than with whatever request happened to ask, because ffmpeg is killed off
	// with whatever context started it.
	ctx context.Context

	// measured is the rate frames have actually been coming out of this
	// encoder at, counted over the last second. The rate asked for is a
	// ceiling: the machine may not be able to reach it, and the controller has
	// to be told the difference, or it will keep asking for a frame rate that
	// never turns up and blame the network for the missing frames.
	measured atomic.Int64

	// The encoder lifecycle is guarded because the control loop and a viewer
	// asking for a codec both have a say in it. idle is when the last viewer of
	// this rung went.
	mu   sync.Mutex
	idle time.Time
}

// startVideo builds one encoder and one rate controller per rung of the ladder
// and starts the loops that run them. It returns the page banner and a
// description for the log.
func (p *pipeline) startVideo(ctx context.Context, cfg config, ladder quality.Ladder) (string, string, error) {
	codec, err := encode.ParseCodec(cfg.codec)
	if err != nil {
		return "", "", err
	}
	// The configured bitrate is the budget for the whole share, handed out
	// between the rungs in proportion to how much picture each has to describe,
	// and each rung then keeps its own share of what is left once the viewers on
	// it have had their say.
	budget := cfg.bitrate * 1000
	// A rung is fed the same captures as the MJPEG endpoint, so it can never be
	// given more frames than they are taken at however high -max-fps is set.
	// The encoder is told the rate it will really see rather than the one that
	// was asked for, which is what its own frame budget is worked out from.
	maxFPS := min(cfg.maxFPS, cfg.fps)
	for _, level := range ladder {
		share := ladder.Budget(level, budget)
		rate, err := adapt.New(adapt.Config{
			MinFPS:  cfg.minFPS,
			MaxFPS:  maxFPS,
			Bitrate: share,
			// The floor a controller picks for itself is meant for a whole
			// stream, and holding a 360p rung to it would leave the share it
			// was given unusable. The share is the top of its range, so a
			// viewer on a bad link has somewhere to go.
			MinBitrate: max(share/4, minShareBitrate),
			// The climb is half as far as the share is wide, not four times it.
			// A controller that may raise a rung to four times its share will
			// find that ceiling on a link with headroom to spare, and a screen
			// that barely moves is not worth 32 Mbit/s to look at.
			MaxBitrate: share * 2,
			// The captures are shared with the MJPEG endpoint and the other
			// rungs, so there is no point capturing faster than -fps.
			CaptureLimit: cfg.fps,
		})
		if err != nil {
			return "", "", err
		}
		enc, err := encode.New(encode.Options{
			Codec:            codec,
			Bitrate:          share,
			MaxFPS:           maxFPS,
			KeyframeInterval: cfg.keyint,
			Height:           level.Height,
		})
		if err != nil {
			return "", "", err
		}
		p.levels = append(p.levels, &qualityStream{
			level: level,
			video: stream.NewHub(),
			enc:   enc,
			rate:  rate,
			ctx:   ctx,
			// Named after the rung, because the failure that matters is "the
			// 360p stream died" and not "a stream died".
			err: &errLogger{what: "encode " + level.Name},
		})
		// The controller has nothing to work from until it has been told about
		// a viewer, and an encoder asked for before that would have no rate.
		rate.Observe(adapt.Feedback{})
	}

	for _, l := range p.levels {
		go l.feedLoop(p.jpeg)
		go l.publishLoop()
	}

	return fmt.Sprintf("%s capture · %s %d-%d fps · %s at %d kbit/s down to %d · %s",
			p.grab.Name(), codec, cfg.minFPS, maxFPS, ladder.Names(), budget/1000,
			ladder.Budget(ladder[0], budget)/1000, p.preview),
		fmt.Sprintf("%s via %s, %d-%d fps, %s at %d kbit/s down to %d",
			p.levels[len(p.levels)-1].enc.Name(), codec, cfg.minFPS, maxFPS,
			ladder.Names(), budget/1000, ladder.Budget(ladder[0], budget)/1000), nil
}

// startSignalling answers viewers and feeds their measurements back into the
// control loop.
func (p *pipeline) startSignalling(ctx context.Context, svc *service) (http.Handler, error) {
	return rtc.New(ctx, rtc.Config{
		Streams:        p.stream,
		InitialBitrate: p.levels[len(p.levels)-1].enc.Bitrate(),
		Identity:       func(r *http.Request) string { return svc.identity(r).ID },
		OnViewers:      p.reportViewers,
		OnFeedback:     p.reportFeedback,
	})
}

// stream is the rung a viewer asked for, as the signalling server needs it.
func (p *pipeline) stream(name string) rtc.Stream {
	// A name that is not on offer means a viewer that has not chosen, or a link
	// that was written when a different ladder was running. Both get the best
	// picture on offer, which is what either would have got without a picker,
	// and is better than a refused offer and a fallback to MJPEG.
	l := p.rung(name)
	if l == nil {
		l = p.levels[len(p.levels)-1]
	}
	return rtc.Stream{Frames: l.video, Codec: l.enc.Codec, Prepare: l.prepare}
}

// rung is the named level's stream, or nil when the name is not on offer.
func (p *pipeline) rung(name string) *qualityStream {
	for _, l := range p.levels {
		if l.level.Name == name {
			return l
		}
	}
	return nil
}

// reportViewers and reportFeedback hand the latest numbers to the control loop.
func (p *pipeline) reportViewers(counts map[string]int) {
	select {
	case p.viewers <- counts:
	default:
	}
}

func (p *pipeline) reportFeedback(f rtc.Feedback) {
	select {
	case p.feedback <- f:
	default:
	}
}

// stop tears every encoder down. Capture stops with the context.
func (p *pipeline) stop() {
	for _, l := range p.levels {
		l.enc.Stop()
	}
	p.fps.Store(0)
}

// controlLoop turns viewer counts and measurements into one thing: the rate
// capture and every encoder run at.
func (p *pipeline) controlLoop(ctx context.Context) {
	ticker := time.NewTicker(controlInterval)
	defer ticker.Stop()
	var (
		counts map[string]int
		latest = map[string]rtc.Feedback{}
	)
	for {
		select {
		case <-ctx.Done():
			p.stop()
			return
		case m := <-p.viewers:
			counts = m
			for _, l := range p.levels {
				name, n := l.level.Name, counts[l.level.Name]
				switch {
				case n < 1:
					// The rung is empty. Its controller forgets the last
					// viewer's measurements, so the next one starts from the
					// whole share of the bitrate rather than from a rate that
					// was measured for a link that has gone.
					latest[name] = rtc.Feedback{}
					l.rate.Observe(adapt.Feedback{})
				case latest[name].Peers < 1:
					// A viewer that has just connected has not said anything
					// yet, and ffmpeg has to be told a bitrate before there is
					// anything to send. Assuming the link is idle sends at the
					// share this rung was given, and the first measurement a
					// second later corrects it.
					l.rate.Observe(adapt.Feedback{Peers: n, Rate: int(l.measured.Load())})
				}
			}
		case f := <-p.feedback:
			latest[f.Quality] = f
			if l := p.rung(f.Quality); l != nil {
				l.rate.Observe(adapt.Feedback{
					Peers:   f.Peers,
					Bitrate: f.Bitrate,
					RTT:     f.RTT,
					Loss:    f.Loss,
					Rate:    int(l.measured.Load()),
				})
			}
		case <-ticker.C:
		}
		p.apply(counts)
	}
}

// apply sets the capture rate up for the viewers there are and lets every rung
// follow: the MJPEG viewers want frames at a fixed rate, the WebRTC ones at
// whatever their own connections can carry.
func (p *pipeline) apply(counts map[string]int) {
	want := 0
	// Counted on the preview rather than on the captures, because that is who is
	// watching. The captures also carry every rung and the preview itself, and
	// neither is a viewer: an encoder with nobody to send to would ask for
	// captures to feed nothing.
	if p.preview.Hub().ViewerCount() > 0 {
		want = p.mjpegFPS
	}
	for _, l := range p.levels {
		if counts[l.level.Name] < 1 {
			l.update(false)
			continue
		}
		l.update(true)
		if fps, _ := l.rate.Target(); fps > want {
			want = fps
		}
	}
	// The captures are shared, so they run at the fastest rate any one end
	// wants and no faster than -fps, which is the ceiling for everything.
	p.fps.Store(int64(min(want, p.capFPS)))
}

// update makes one rung's encoder match what is being asked of it: running while
// there is a reason to, and stopped a moment after the reason goes.
//
// The frame rate only reaches the captures, and only through the pipeline, which
// shares them between every rung. ffmpeg was told the ceiling once, when it
// started, and works out every frame's share of the bitrate from that, so
// capturing more slowly costs nothing at all: it is the bitrate that costs a
// restart, and it is the only thing that says anything about the network.
//
// Nothing else is worth a restart. A viewer that wants a keyframe is not an
// exception: the encoder already emits one every -keyint seconds, and a new
// process takes long enough to start that restarting for one arrives later than
// waiting, and freezes the picture for everyone in between. The first frame out
// of a new encoder is a keyframe, so the one moment a restart is genuinely
// useful is the one already made here: starting the encoder at all.
func (l *qualityStream) update(want bool) {
	l.mu.Lock()
	defer l.mu.Unlock()

	if !want {
		if !l.enc.Running() {
			l.idle = time.Time{}
			return
		}
		if l.idle.IsZero() {
			l.idle = time.Now()
			return
		}
		if time.Since(l.idle) < idleGrace {
			return
		}
		l.enc.Stop()
		l.idle = time.Time{}
		l.measured.Store(0)
		log.Printf("%s: no viewers, encoder stopped", l.level.Name)
		return
	}

	l.idle = time.Time{}
	fps, bitrate := l.rate.Target()
	if fps < 1 {
		// Nothing has been measured yet, so there is no rate to run at.
		return
	}
	switch {
	case !l.enc.Running():
		// An encoder that is not running has either not been asked for yet or
		// has died, and the second is worth saying out loud.
		if err := l.enc.Err(); err != nil {
			l.err.Print(err)
		}
		if err := l.enc.SetBitrate(l.ctx, bitrate); err != nil {
			l.err.Print(err)
			return
		}
		log.Printf("%s: encoding up to %d fps at %d kbit/s", l.level.Name, fps, bitrate/1000)
	case l.enc.Bitrate() != bitrate:
		was := l.enc.Bitrate()
		if err := l.enc.SetBitrate(l.ctx, bitrate); err != nil {
			l.err.Print(err)
			return
		}
		log.Printf("%s: encoding up to %d fps at %d kbit/s (was %d kbit/s)",
			l.level.Name, fps, bitrate/1000, was/1000)
	}
}

// prepare makes the codec describable, so that a viewer which arrives before
// anything has been encoded can still be answered.
func (l *qualityStream) prepare(ctx context.Context) error {
	if l.enc.Codec().Ready {
		return nil
	}
	l.update(true)
	deadline := time.Now().Add(prepareTimeout)
	for {
		info := l.enc.Codec()
		if info.Ready {
			return nil
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("no %s keyframe within %v, cannot describe the codec", info.Codec, prepareTimeout)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// feedLoop hands captured frames to this rung's encoder, which does the work in
// its own time: a frame it cannot take yet is dropped rather than queued,
// because a queue of old frames is a stream of a screen that has moved on.
//
// Every rung is fed every capture, at whatever rate the shared capture runs at.
// A rung that is not running simply skips them, so an unused one costs a channel
// receive and nothing more, and a slow one holds up nothing but itself.
func (l *qualityStream) feedLoop(jpeg *stream.Hub) {
	// Subscribed to the captures before anything can be published, so the
	// first frame of a newly started encoder cannot be missed.
	frames, unsubscribe := jpeg.Subscribe(l.ctx)
	defer unsubscribe()
	for {
		select {
		case <-l.ctx.Done():
			return
		case f := <-frames:
			if !l.enc.Running() {
				continue
			}
			if err := l.enc.Feed(l.ctx, f.Data); err != nil {
				if l.ctx.Err() != nil {
					return
				}
				if !errors.Is(err, encode.ErrStopped) {
					l.err.Print(err)
				}
			}
		}
	}
}

// publishLoop puts the coded frames where this rung's WebRTC viewers are fed
// from. The MJPEG viewers need nothing from here: they are served the captured
// frames themselves, so they see the screen whether or not anything is being
// encoded.
//
// It also counts what comes out, which is how the controller learns what the
// machine can actually do rather than what was asked of it.
func (l *qualityStream) publishLoop() {
	// A gap this long means ffmpeg has only just started. The first frames
	// after a start say more about the start than about the rate, so the
	// window begins again rather than reporting a rate of three.
	const startGap = 500 * time.Millisecond
	const window = time.Second
	// Fewer frames than this in a whole second is a start, not a rate.
	const minFrames = 4

	var (
		from  time.Time
		last  time.Time
		count int
	)
	// The channel stays open across restarts, so this outlives every ffmpeg
	// process the control loop starts.
	for {
		select {
		case <-l.ctx.Done():
			return
		case f := <-l.enc.Frames():
			now := time.Now()
			if from.IsZero() || now.Sub(last) >= startGap {
				from, last, count = now, now, 0
			}
			last, count = now, count+1
			if elapsed := now.Sub(from); elapsed >= window {
				if count >= minFrames {
					l.measured.Store(int64(float64(count) / elapsed.Seconds()))
				}
				from, count = now, 0
			}
			// A viewer's clock is built from when the frames actually turned
			// up and how far apart they were, neither of which ffmpeg can say:
			// it stamps them at the rate it was told to expect.
			l.video.PublishFrame(stream.Frame{
				Data:     f.Data,
				Keyframe: f.Keyframe,
				Time:     f.Time,
				Duration: f.Duration,
			})
		}
	}
}

// captureLoop grabs frames at the rate the pipeline is asking for, and does
// nothing at all while it is asking for none.
func captureLoop(ctx context.Context, p *pipeline) {
	for ctx.Err() == nil {
		fps := int(p.fps.Load())
		if fps < 1 {
			if !sleep(ctx, idlePoll) {
				return
			}
			continue
		}
		if !p.captureAt(ctx, fps) {
			return
		}
	}
}

// captureAt grabs and publishes at fps until the pipeline wants a different rate
// or there is nothing left to capture for, and reports whether capturing should
// carry on. The grabs still in flight are waited for first, so two rates never
// compete for the same interval.
func (p *pipeline) captureAt(ctx context.Context, fps int) bool {
	interval := time.Second / time.Duration(fps)
	next := time.Now()
	// More captures in flight than this would not make the rate any higher,
	// because they queue behind each other inside the compositor.
	slots := make(chan struct{}, grabWorkers)
	var wg sync.WaitGroup
	defer wg.Wait()
	for {
		select {
		case slots <- struct{}{}:
		case <-ctx.Done():
			return false
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer func() { <-slots }()
			frame, err := p.grab.Capture(ctx)
			if err != nil {
				if ctx.Err() == nil {
					p.capErr.Print(err)
				}
				return
			}
			p.jpeg.Publish(frame)
		}()

		next = next.Add(interval)
		wait := time.Until(next)
		if wait <= 0 {
			// Behind schedule, which happens when a grab overruns its
			// interval. Start again from now rather than trying to catch up
			// with a burst.
			next = time.Now()
			continue
		}
		if !sleep(ctx, wait) {
			return false
		}
		if int(p.fps.Load()) != fps {
			return true
		}
	}
}

// sleep waits for d, reporting whether the wait finished rather than the
// context ending.
func sleep(ctx context.Context, d time.Duration) bool {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-timer.C:
		return true
	case <-ctx.Done():
		return false
	}
}

// errLogger keeps a failure that repeats from filling the log.
type errLogger struct {
	mu   sync.Mutex
	last time.Time
	what string
}

// quietFor is how long the same kind of failure has to go unrepeated before
// another one is written down.
const quietFor = 5 * time.Second

func (e *errLogger) Print(err error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if time.Since(e.last) < quietFor {
		return
	}
	e.last = time.Now()
	log.Printf("%s: %v", e.what, err)
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
