// Package adapt picks the frame rate and the encoder bitrate that keep the
// screen stream watchable, on whichever of the two limits is binding.
//
// The controller makes two decisions, and which one to move follows from how the
// encoder spends a bitrate. Told a bitrate for the whole stream, it spends that
// bitrate whatever rate the frames turn up at, giving more of it to each of
// fewer frames. So a slower capture does not free any bandwidth: it spends the
// same bandwidth on fewer, larger frames. Measured on this machine, an 8 Mbit/s
// encoder fed at 60, 30, 20 and 10 frames a second puts 7.5 to 9.2 Mbit/s on the
// wire in every case.
//
// That settles the two knobs. The frame rate is the free one, because nothing
// about it reaches the encoder: the capture loop simply takes fewer grabs, and
// a faster machine or a roomier network costs nothing to use. It is the knob for
// the machine, and the only thing it is ever spent on is frames the machine
// failed to deliver. The bitrate is the knob for the network, since it is the
// only thing that changes what goes on the wire, and it is expensive: the
// encoder is told its rate once at startup, so a new bitrate means a new
// process, which every viewer sees as a frozen picture. So the bitrate moves
// rarely, in large steps, and always in the direction the measurements point.
//
// The frame rate is also the more delicate of the two, because a controller that
// blames the network for a machine that cannot keep up will walk its own rate
// down to the floor and leave it there. Feeding back the rate that actually came
// out is what tells the two apart.
package adapt

import (
	"sync"
	"time"
)

const (
	// downStep is the fraction of the current rate kept after congestion. A
	// quarter is the largest cut that still looks like a smooth ramp down.
	downStep = 0.75
	// upStep is the fraction of the current rate added once there is room.
	// Congestion costs a frame rate step, recovery costs two, so climbing
	// stays the careful half of the loop.
	upStep = 1.25
	// tightFactor is the share of the usable rate below which the bitrate
	// counts as too high to carry.
	tightFactor = 0.9
	// looseFactor is the share of the usable rate above which there is room
	// worth spending, and the margin the bitrate aims to keep. The margin
	// matters more than it looks: the send side is paced by a queue rather
	// than a drop, so overrunning the estimate does not lose frames, it adds
	// latency that never comes back.
	looseFactor = 1.3
	// behindFactor is the share of the requested rate the machine has to
	// deliver before it counts as keeping up. Short of it, the requested rate
	// is the problem and not the viewers.
	behindFactor = 0.9
	// rampSamples is how many comfortable observations are needed before the
	// frame rate climbs.
	rampSamples = 2
	// downSamples is how many congested observations are needed before the
	// bitrate is cut, and it is deliberately larger than rampSamples. A
	// bandwidth estimate is not a measurement so much as an inference, and
	// send-side estimation in particular overshoots on the way up and
	// overshoots hard on the way down: a link with nothing wrong on it can
	// report a quarter of its real rate for a second and then climb back.
	// Cutting the bitrate on that would restart the encoder and freeze
	// everyone's picture to chase a number that was never true. The frame
	// rate, which costs nothing, still reacts to a single bad sample, though
	// only where the complaint is a round trip time or a loss rate rather than
	// a shortage of bandwidth, since a bitrate cut is what answers that.
	downSamples = 4
	// downBitrateStep is the largest fraction of the current bitrate one cut
	// may remove. Together with downSamples this means a collapse in the
	// estimate walks the bitrate down over several observations rather than
	// arriving at the floor in one.
	downBitrateStep = 0.7
	// minBitrateFloor is the lowest bitrate the encoder will be asked for
	// unless told otherwise, in bits per second. Below roughly this a screen
	// share stops being readable, so a caller that sets no minimum still gets
	// a picture rather than a slideshow.
	minBitrateFloor = 1_500_000
	// minBitrateGap throttles a cut forced by congestion. The encoder restart
	// it costs is a frozen picture, so a second one has to be worth more than
	// the first.
	minBitrateGap = 3 * time.Second
	// maxLoss is the packet loss above which the stream counts as congested.
	maxLoss = 0.1
	// maxRTT is the round trip time the stream aims to stay below.
	maxRTT = 250 * time.Millisecond
)

// Config is the controller's operating range. Zero fields take a default.
type Config struct {
	// MinFPS and MaxFPS bound the capture and encode rate. The stream runs at
	// MinFPS when the machine cannot manage more and at MaxFPS when it can.
	MinFPS int
	MaxFPS int
	// Bitrate is the starting encoder bitrate, meaning the rate for the whole
	// stream however fast the frames arrive.
	Bitrate int
	// MinBitrate and MaxBitrate bound the encoder bitrate.
	MinBitrate int
	MaxBitrate int
	// Headroom is the share of the estimated usable rate the stream is
	// allowed to use. Staying under the estimate is what keeps the send side's
	// pacing queue short, and a short queue is what keeps the picture close to
	// live.
	Headroom float64
	// CaptureLimit is a hard ceiling on the frame rate, whatever the
	// controller would like. It lowers MaxFPS when it is the smaller of the
	// two.
	CaptureLimit int
	// RTTBudget is the round trip time the stream aims to stay below. Twice it
	// counts as congestion.
	RTTBudget time.Duration
	// BitrateDebounce is the shortest gap between two bitrate increases. Each
	// one restarts the encoder, so they are rationed.
	BitrateDebounce time.Duration
	// HoldTime is the shortest gap between two frame rate changes. The rate
	// costs nothing to change, but changing it every second on the strength of
	// a single sample makes for a stream that never settles.
	HoldTime time.Duration
}

// Feedback is one measurement of what the viewers and the machine are actually
// managing. The viewer fields are aggregated over every connected viewer, so a
// single struggling viewer pulls the whole stream down.
type Feedback struct {
	// Peers is the number of connected viewers. Zero means nobody is
	// watching, which resets the controller.
	Peers int
	// Bitrate is the usable send rate the viewers' congestion reports agree
	// on, in bits per second. Zero means it is not known yet.
	Bitrate int
	// RTT is the round trip time to the worst viewer.
	RTT time.Duration
	// Loss is the fraction of packets the worst viewer reports as lost.
	Loss float64
	// Rate is the frame rate that actually came out of the encoder, so the
	// controller can tell a machine that cannot keep up from a network that
	// cannot keep up. Zero means it is not known yet.
	Rate int
}

// Controller turns feedback into an encoder setting. It is safe for
// concurrent use.
type Controller struct {
	cfg Config
	now func() time.Time

	mu       sync.Mutex
	fps      int
	bitrate  int
	good     int       // consecutive observations with room to spare
	bad      int       // consecutive observations of congestion
	changed  time.Time // last bitrate change
	moved    time.Time // last frame rate change
	cuts     int       // bitrate cuts forced by congestion this session
	observed bool
}

// New returns a controller that starts at the top of its range, so the first
// viewer gets the best picture the hardware can manage.
func New(cfg Config) (*Controller, error) {
	if cfg.MaxFPS == 0 {
		cfg.MaxFPS = 30
	}
	if cfg.CaptureLimit > 0 && cfg.CaptureLimit < cfg.MaxFPS {
		cfg.MaxFPS = cfg.CaptureLimit
	}
	if cfg.MinFPS == 0 {
		cfg.MinFPS = 5
	}
	if cfg.Bitrate == 0 {
		cfg.Bitrate = 4_000_000
	}
	if cfg.MinBitrate == 0 {
		cfg.MinBitrate = max(cfg.Bitrate/4, minBitrateFloor)
	}
	if cfg.MaxBitrate == 0 {
		cfg.MaxBitrate = cfg.Bitrate * 4
	}
	if cfg.Headroom <= 0 {
		cfg.Headroom = 1.15
	}
	if cfg.RTTBudget <= 0 {
		cfg.RTTBudget = maxRTT
	}
	if cfg.BitrateDebounce <= 0 {
		cfg.BitrateDebounce = 10 * time.Second
	}
	if cfg.HoldTime <= 0 {
		cfg.HoldTime = 2 * time.Second
	}
	if cfg.MinFPS < 1 || cfg.MaxFPS < cfg.MinFPS || cfg.MaxFPS > 60 {
		return nil, errRange("frame rate must be 1..60 and non-decreasing")
	}
	if cfg.MinBitrate < 1 || cfg.MaxBitrate < cfg.MinBitrate {
		return nil, errRange("bitrate minimum and maximum out of order")
	}
	if cfg.Bitrate < cfg.MinBitrate || cfg.Bitrate > cfg.MaxBitrate {
		return nil, errRange("bitrate outside the configured minimum and maximum")
	}
	return &Controller{cfg: cfg, now: time.Now}, nil
}

type errRange string

func (e errRange) Error() string { return "adapt: " + string(e) + " out of range" }

func (c *Controller) Target() (fps, bitrate int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.fps, c.bitrate
}

// Observe folds one measurement into the controller. Call it on a steady
// cadence, about once a second.
func (c *Controller) Observe(f Feedback) {
	now := c.now()

	c.mu.Lock()
	defer c.mu.Unlock()

	if !c.observed {
		c.fps, c.bitrate = c.cfg.MaxFPS, c.cfg.Bitrate
		// The hold time is between two changes, and setting the rate to
		// something for the first time is not one of them: the first sample
		// that finds a machine or a network which cannot do it has to be able
		// to say so. The bitrate, on the other hand, starts its debounce here,
		// because an increase wants a settled estimate to be worth acting on.
		c.moved, c.changed = time.Time{}, now
		c.observed = true
	}
	if f.Peers <= 0 {
		c.resetLocked(now)
		return
	}

	usable := c.usable(f)
	// A shortfall against the rate we asked for is the machine, not the
	// viewers: the encoder gets whatever the capture loop manages and spends
	// its bitrate on it either way.
	behind := f.Rate > 0 && f.Rate < int(float64(c.fps)*behindFactor)
	tight := usable > 0 && float64(usable) < float64(c.bitrate)*tightFactor
	// Twice the budget rather than the budget itself: a round trip inside the
	// budget is latency the stream can be watched through, and one past twice it
	// is congestion even where the estimator reports plenty of room. The budget
	// is the caller's, so the threshold moves with it.
	late := f.RTT > 2*c.cfg.RTTBudget || f.Loss > maxLoss
	comfortable := usable == 0 || float64(usable) >= float64(c.bitrate)*looseFactor

	switch {
	case tight || late:
		// Congestion is the one thing worth reacting to straight away, but
		// what it is worth reacting to depends on which knob is turned. A link
		// short of bandwidth is answered with the bitrate, which is the only
		// one of the two that changes what goes on the wire, and it waits to be
		// sure as well: a cut is an encoder restart the viewers see as a frozen
		// picture, and a bandwidth estimate on its way down is the least
		// trustworthy number in the system.
		//
		// A round trip time or a loss rate is a different complaint, and the
		// estimate is not what is being made: the link has the room for what is
		// being sent, so there is no bitrate to cut to and a cut would freeze
		// everybody's picture to chase a shortage nobody reported. The frame
		// rate is what is left, it costs nothing, and every frame it does not
		// send is one the link does not have to lose or wait for. So it gives
		// way on the sample that says so, streak or not.
		c.good = 0
		c.bad++
		if late {
			c.shrinkLocked(0, now)
		}
		if c.bad >= downSamples || !c.running() {
			c.downLocked(usable, now)
		}
	case behind:
		// The machine is the limit and the network is not. Ask for what it can
		// actually deliver rather than climbing back into the same wall, which
		// is the loop that used to walk a rate down to the floor in
		// consecutive seconds and stop there.
		c.good, c.bad = 0, 0
		c.shrinkLocked(f.Rate, now)
	case comfortable:
		c.bad = 0
		c.upLocked(usable, f.Rate, now)
	default:
		// Close enough to the limit that neither direction is justified. Hold
		// position and make the next observation start over.
		c.good, c.bad = 0, 0
	}
}

// resetLocked forgets the last viewer's measurements so the next session
// starts from the configured quality at the top of the rate range.
func (c *Controller) resetLocked(now time.Time) {
	c.fps, c.bitrate = c.cfg.MaxFPS, c.cfg.Bitrate
	c.good, c.bad, c.cuts = 0, 0, 0
	c.changed, c.moved = now, time.Time{}
}

// usable is the send rate the stream may spend, or zero while it is unknown.
func (c *Controller) usable(f Feedback) int {
	if f.Bitrate <= 0 {
		return 0
	}
	return int(float64(f.Bitrate) * c.cfg.Headroom)
}

// downLocked sheds load. The bitrate goes first and on its own: it is the only
// thing that changes what goes on the wire, and a bitrate the viewers cannot
// carry is what the measurements are complaining about. The frame rate is left
// where it is, because a slower rate spends the same bitrate on fewer, larger
// frames and buys nothing but choppiness. Only once the bitrate has bottomed out
// is there anything left to give up, and a rate already given away for a link
// that is losing packets rather than short of bandwidth stays given away.
//
// The first cut of a session is not rationed. The encoder was started at a
// guessed bitrate and the send side has been queueing the difference ever since,
// so a gap here would mean several seconds of a stream that is minutes behind.
// Every later cut has to earn its restart.
// running reports whether the rate has already been given up on, which is the
// case the congestion count must not be allowed to hold up: when the rate is
// already at its floor the bitrate is the only thing left to give, and there
// is nothing to be gained by waiting to find out whether the estimate meant it.
func (c *Controller) running() bool { return c.fps > c.cfg.MinFPS }

func (c *Controller) downLocked(usable int, now time.Time) {
	c.good, c.bad = 0, 0
	if usable <= 0 {
		return
	}
	next := max(c.cfg.MinBitrate, c.fitLocked(usable))
	// One step at a time. A cut is an encoder restart and a frozen picture,
	// so it is made no larger than it needs to be and no faster than the
	// evidence warrants. Reaching the floor now costs several restarts and
	// several seconds of a blocky picture; reaching it in steps costs the
	// same restarts but only briefly undoes what was working.
	floor := int(float64(c.bitrate) * downBitrateStep)
	if floor > next {
		next = floor
	}
	if next >= c.bitrate {
		// The bitrate is already as low as this network allows, so the frame
		// rate is the last thing left to give.
		if c.bitrate <= c.cfg.MinBitrate {
			c.shrinkLocked(0, now)
		}
		return
	}
	if c.cuts > 0 && now.Sub(c.changed) < minBitrateGap {
		return
	}
	c.bitrate, c.changed = next, now
	c.cuts++
}

// fitLocked is the bitrate that would spend the usable rate with the margin the
// pacing queue needs, leaving nothing to spare.
func (c *Controller) fitLocked(usable int) int {
	return int(float64(usable) / looseFactor)
}

// shrinkLocked drops the frame rate, either to a rate the machine was measured
// at or, with no measurement to go on, by the usual quarter step. A measured
// rate is taken as it is: the machine is not going to do better next time just
// because the stream asked more nicely.
func (c *Controller) shrinkLocked(measured int, now time.Time) {
	if now.Sub(c.moved) < c.cfg.HoldTime {
		return
	}
	next := 0
	if measured > 0 {
		next = min(measured, c.fps)
	} else {
		next = max(c.cfg.MinFPS, int(float64(c.fps)*downStep))
	}
	if next < c.cfg.MinFPS {
		next = c.cfg.MinFPS
	}
	if next < c.fps {
		c.fps = next
		c.moved = now
	}
}

// upLocked spends what is available. The frame rate climbs first, and it is the
// better of the two buys: a higher rate is smoother motion for exactly the same
// bitrate, and it never reaches the encoder. Only at the ceiling does raising the
// bitrate pay off, and then not before the debounce, since it costs a restart.
func (c *Controller) upLocked(usable, rate int, now time.Time) {
	c.good++
	if c.good < rampSamples || now.Sub(c.moved) < c.cfg.HoldTime {
		return
	}
	c.good = 0
	if c.fps < c.cfg.MaxFPS {
		next := min(c.cfg.MaxFPS, int(float64(c.fps)*upStep))
		// Never ask for more than the machine was seen to deliver. A rate it
		// cannot reach is not a rate we get, and climbing into it only spends
		// capture effort to arrive at the same number: with no measurement the
		// usual step stands.
		if rate > 0 && next > rate {
			next = rate
		}
		if next <= c.fps {
			return
		}
		c.fps, c.moved = next, now
		return
	}
	if usable <= 0 || now.Sub(c.changed) < c.cfg.BitrateDebounce {
		return
	}
	// Aim a little under the usable rate so the next sample is not already
	// tight, and only spend a restart on a worthwhile step up.
	next := c.fitLocked(usable)
	if next == c.bitrate || next < c.bitrate+int(float64(c.bitrate)*upStep) {
		return
	}
	// The ceiling can leave a worthwhile step nowhere to go, and a bitrate
	// already sitting on it is not raised. Nothing changed, so nothing is
	// recorded either: this timestamp is what holds off the next congestion cut,
	// and a gap it spends on a climb that never happened is a cut that waits on
	// nothing.
	if next = min(c.cfg.MaxBitrate, next); next <= c.bitrate {
		return
	}
	c.bitrate, c.changed = next, now
}
