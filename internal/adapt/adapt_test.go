package adapt

import (
	"testing"
	"time"
)

type clock struct{ at time.Time }

func (c *clock) now() time.Time { return c.at }

func newTestController(t *testing.T, cfg Config) (*Controller, *clock) {
	t.Helper()
	c, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	clk := &clock{at: time.Unix(1700000000, 0)}
	c.now = clk.now
	return c, clk
}

func target(t *testing.T, c *Controller) (int, int) {
	t.Helper()
	fps, bitrate := c.Target()
	return fps, bitrate
}

// slow is a config for a machine that can manage anything, used by the tests
// that are about the network rather than about the ceiling.
var slow = Config{MaxFPS: 30, MinFPS: 5, Bitrate: 1_000_000, MinBitrate: 100_000, MaxBitrate: 4_000_000}

// roomy is a feedback sample for a viewer with bandwidth to spare and a machine
// that keeps up.
func roomy(rate int) Feedback { return Feedback{Peers: 1, Bitrate: 100_000_000, Rate: rate} }

func TestNewRejectsBadConfig(t *testing.T) {
	cases := map[string]Config{
		"min fps zero":      {MinFPS: -1, MaxFPS: 30},
		"max below min":     {MinFPS: 30, MaxFPS: 10},
		"max above sixty":   {MinFPS: 5, MaxFPS: 120},
		"bitrate inverted":  {Bitrate: 1000, MinBitrate: 2000, MaxBitrate: 4000},
		"bitrate too small": {Bitrate: 100, MinBitrate: 1000, MaxBitrate: 4000},
		"bitrate too large": {Bitrate: 9000, MinBitrate: 1000, MaxBitrate: 4000},
		"negative min rate": {MinFPS: 5, MaxFPS: -1},
		"negative bitrates": {Bitrate: 2000, MinBitrate: -1, MaxBitrate: 4000},
	}
	for name, cfg := range cases {
		if _, err := New(cfg); err == nil {
			t.Errorf("%s: New(%+v) succeeded, want error", name, cfg)
		}
	}
}

func TestNewFillsDefaults(t *testing.T) {
	c, err := New(Config{})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	want := Config{
		MinFPS: 5, MaxFPS: 30, Bitrate: 4_000_000,
		MinBitrate: 1_500_000, MaxBitrate: 16_000_000,
		Headroom: 1.15, RTTBudget: maxRTT,
		BitrateDebounce: 10 * time.Second, HoldTime: 2 * time.Second,
	}
	if c.cfg != want {
		t.Errorf("config = %+v, want %+v", c.cfg, want)
	}
}

func TestCaptureLimitCapsMaxFPS(t *testing.T) {
	c, err := New(Config{MaxFPS: 60, MinFPS: 10, CaptureLimit: 24})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if c.cfg.MaxFPS != 24 {
		t.Errorf("MaxFPS = %d, want 24", c.cfg.MaxFPS)
	}
}

func TestFirstObservationStartsAtConfiguredQuality(t *testing.T) {
	c, _ := newTestController(t, Config{Bitrate: 2_000_000, MinBitrate: 200_000, MaxBitrate: 6_000_000})
	c.Observe(roomy(0))
	fps, bitrate := target(t, c)
	if fps != 30 {
		t.Errorf("fps = %d, want 30", fps)
	}
	if bitrate != 2_000_000 {
		t.Errorf("bitrate = %d, want 2000000", bitrate)
	}
}

func TestUnknownBandwidthRampsToTop(t *testing.T) {
	c, clk := newTestController(t, slow)
	c.Observe(Feedback{Peers: 1, Bitrate: 5_000_000})
	if fps, _ := target(t, c); fps != 30 {
		t.Fatalf("fps after one observation = %d, want 30", fps)
	}
	// With neither a measurement of the network nor one of the machine, the
	// rate is allowed to climb, since there is nothing to contradict the
	// assumption that both are fine.
	for range 20 {
		clk.at = clk.at.Add(time.Second)
		c.Observe(Feedback{Peers: 1})
	}
	fps, bitrate := target(t, c)
	if fps != 30 {
		t.Errorf("fps = %d, want 30", fps)
	}
	if bitrate != 1_000_000 {
		t.Errorf("bitrate = %d, want 1000000 (no measurement to act on)", bitrate)
	}
}

func TestRampUpNeedsTwoComfortableSamples(t *testing.T) {
	c, clk := newTestController(t, slow)
	c.Observe(roomy(0))
	c.mu.Lock()
	c.fps, c.good = 8, 0
	c.mu.Unlock()

	// One comfortable sample is not enough; the rate is meant to climb slowly.
	clk.at = clk.at.Add(time.Second)
	c.Observe(roomy(0))
	if fps, _ := target(t, c); fps != 8 {
		t.Errorf("fps = %d after one comfortable sample, want 8", fps)
	}
	clk.at = clk.at.Add(time.Second)
	c.Observe(roomy(0))
	if fps, _ := target(t, c); fps != 10 {
		t.Errorf("fps = %d after two comfortable samples, want 10", fps)
	}

	// A sample that is neither tight nor comfortable breaks the streak.
	c.mu.Lock()
	c.good = 1
	c.mu.Unlock()
	clk.at = clk.at.Add(time.Second)
	c.Observe(Feedback{Peers: 1, Bitrate: 310_000})
	clk.at = clk.at.Add(time.Second)
	c.Observe(roomy(0))
	if fps, _ := target(t, c); fps != 10 {
		t.Errorf("fps = %d, want 10: a sample without headroom must restart the climb", fps)
	}
}

// TestCongestionCutsTheBitrateNotTheRate is the rule the whole design turns on:
// a network that cannot carry the stream is answered with the bitrate, because a
// slower frame rate spends the same bitrate on fewer, larger frames and so buys
// nothing but choppiness.
func TestCongestionCutsTheBitrateNotTheRate(t *testing.T) {
	c, clk := newTestController(t, slow)
	c.Observe(roomy(0))

	starved := Feedback{Peers: 1, Bitrate: 200_000}
	for range downSamples - 1 {
		clk.at = clk.at.Add(time.Second)
		c.Observe(starved)
		if _, bitrate := target(t, c); bitrate != 1_000_000 {
			t.Fatalf("bitrate = %d after %d congested samples, want 1000000: the bitrate must not move on a sample or two",
				bitrate, c.bad)
		}
	}

	clk.at = clk.at.Add(time.Second)
	c.Observe(starved)
	fps, bitrate := target(t, c)
	if bitrate >= 1_000_000 {
		t.Fatalf("bitrate = %d, want below the starting 1000000", bitrate)
	}
	if fps != 30 {
		t.Fatalf("fps = %d, want 30: the rate must not move to answer the network", fps)
	}

	// The cut is one step, not a jump to wherever the estimate happens to
	// point. Anything larger is more of the picture given away on the strength
	// of a number that was already known to be unreliable.
	if want := int(float64(1_000_000) * downBitrateStep); bitrate != want {
		t.Errorf("bitrate = %d, want one step down to %d", bitrate, want)
	}

	// It keeps stepping down, but only while the stream still counts as too
	// heavy for the link, and then stops. Chasing the estimate all the way down
	// to the number that would leave the most margin is what makes a controller
	// hunt: the dead band between too heavy and comfortably light is there to
	// be settled in, not squeezed to the last byte of.
	headroom := 1.15
	usable := int(float64(200_000) * headroom)
	for range 40 {
		clk.at = clk.at.Add(time.Second)
		c.Observe(starved)
		gotFps, gotBitrate := target(t, c)
		if gotFps != 30 {
			t.Fatalf("fps = %d after %d congested samples, want 30", gotFps, downSamples+41)
		}
		if gotBitrate != bitrate {
			if gotBitrate > bitrate {
				t.Fatalf("bitrate rose to %d while congested", gotBitrate)
			}
			bitrate = gotBitrate
		}
		if float64(usable) >= float64(bitrate)*tightFactor {
			break
		}
	}
	if float64(usable) < float64(bitrate)*tightFactor {
		t.Errorf("bitrate = %d against a usable %d, want it settled where the stream fits", bitrate, usable)
	}
	if bitrate >= 1_000_000 {
		t.Errorf("bitrate = %d, want it well below the starting 1000000", bitrate)
	}

	// And it stays there rather than continuing to walk down for as long as the
	// link stays tight.
	for range 20 {
		clk.at = clk.at.Add(time.Second)
		c.Observe(starved)
		if _, settled := target(t, c); settled != bitrate {
			t.Fatalf("settled state moved to %d", settled)
		}
	}
}

// TestOneBadSampleDoesNotCostThePicture is the regression for what a viewer
// actually saw: a bandwidth estimate that fell to a quarter of its real value
// for a single second restarted the encoder and cut the bitrate by more than
// half, on a link with nothing wrong with it. The frame rate still gives way at
// once, because it costs nothing.
func TestOneBadSampleDoesNotCostThePicture(t *testing.T) {
	c, clk := newTestController(t, slow)
	c.Observe(roomy(0))
	_, before := target(t, c)

	clk.at = clk.at.Add(time.Second)
	c.Observe(Feedback{Peers: 1, Bitrate: 120_000})
	if _, after := target(t, c); after != before {
		t.Errorf("bitrate = %d after one collapsed estimate, want it left at %d", after, before)
	}

	// One good sample in the middle puts it back to square one.
	clk.at = clk.at.Add(time.Second)
	c.Observe(roomy(0))
	clk.at = clk.at.Add(time.Second)
	c.Observe(Feedback{Peers: 1, Bitrate: 120_000})
	if _, after := target(t, c); after != before {
		t.Errorf("bitrate = %d, want it left at %d: a good sample starts the count again", after, before)
	}

	// And it does still act once the evidence is consistent.
	for range downSamples {
		clk.at = clk.at.Add(time.Second)
		c.Observe(Feedback{Peers: 1, Bitrate: 120_000})
	}
	if _, after := target(t, c); after >= before {
		t.Errorf("bitrate = %d, want below %d once %d samples agree", after, before, downSamples)
	}
}

func TestBitrateCutsAreThrottled(t *testing.T) {
	c, clk := newTestController(t, slow)
	c.Observe(roomy(0))
	starved := Feedback{Peers: 1, Bitrate: 200_000}
	for range downSamples {
		clk.at = clk.at.Add(time.Second)
		c.Observe(starved)
	}
	_, first := target(t, c)
	if first == 1_000_000 {
		t.Fatal("congestion that never went away did not cut the bitrate")
	}

	// Another cut milliseconds later must not restart the encoder again. The
	// count refills, but the gap has not expired.
	clk.at = clk.at.Add(100 * time.Millisecond)
	for range downSamples {
		c.Observe(Feedback{Peers: 1, Bitrate: 40_000})
	}
	_, second := target(t, c)
	if second != first {
		t.Errorf("bitrate = %d, want %d: cuts must be seconds apart", second, first)
	}
	for range downSamples {
		clk.at = clk.at.Add(time.Second)
		c.Observe(Feedback{Peers: 1, Bitrate: 40_000})
	}
	if _, third := target(t, c); third == second {
		t.Errorf("bitrate = %d, want another cut once the gap expired", third)
	}
}

// TestRateOnlyGivesWayWhenTheBitrateIsAtItsFloor covers a network too slow for
// even the lowest bitrate, where the frame rate is all that is left to give.
func TestRateOnlyGivesWayWhenTheBitrateIsAtItsFloor(t *testing.T) {
	c, clk := newTestController(t, slow)
	c.Observe(roomy(0))
	starved := Feedback{Peers: 1, Bitrate: 50_000}
	// Long enough for the bitrate to walk all the way down, one cautious step
	// and a few confirming samples at a time.
	for range 80 {
		clk.at = clk.at.Add(time.Second)
		c.Observe(starved)
	}
	fps, bitrate := target(t, c)
	if bitrate != 100_000 {
		t.Errorf("bitrate = %d, want the 100000 floor", bitrate)
	}
	if fps >= 30 {
		t.Errorf("fps = %d, want below 30 once the bitrate has bottomed out", fps)
	}
}

// TestAMachineBehindTheRateIsNotTheNetworksFault covers the loop that used to
// walk a 60 frames a second request down to the floor in consecutive seconds and
// leave it there: the network said it had plenty of room, the machine could not
// deliver, and the controller kept reading the second as the first.
func TestAMachineBehindTheRateIsNotTheNetworksFault(t *testing.T) {
	c, clk := newTestController(t, Config{
		MaxFPS: 60, MinFPS: 5, Bitrate: 8_000_000,
		MinBitrate: 500_000, MaxBitrate: 20_000_000,
	})
	c.Observe(Feedback{Peers: 1, Bitrate: 20_000_000, Rate: 45})
	fps, bitrate := target(t, c)
	if fps != 45 {
		t.Fatalf("fps = %d, want the 45 the machine delivered", fps)
	}
	if bitrate != 8_000_000 {
		t.Fatalf("bitrate = %d, want the configured 8000000", bitrate)
	}

	// Twenty comfortable samples from a machine that never gets past 45 must
	// leave the rate exactly where the machine put it, and must not touch the
	// bitrate: this is the whole point of feeding the measured rate back.
	for range 20 {
		clk.at = clk.at.Add(time.Second)
		c.Observe(Feedback{Peers: 1, Bitrate: 20_000_000, Rate: 45})
		if gotFps, _ := target(t, c); gotFps != fps {
			t.Fatalf("fps = %d, want %d: the rate must not hunt around the machine's ceiling", gotFps, fps)
		}
	}
	if _, bitrate := target(t, c); bitrate != 8_000_000 {
		t.Errorf("bitrate = %d, want 8000000: a machine limit is not a network limit", bitrate)
	}
}

// TestTheRateFollowsAMachineThatFreesUp is the other half of the same feedback:
// with the load gone the machine delivers more, and the rate has to be allowed
// to follow it all the way back up to the configured ceiling.
func TestTheRateFollowsAMachineThatFreesUp(t *testing.T) {
	c, clk := newTestController(t, Config{
		MaxFPS: 60, MinFPS: 5, Bitrate: 8_000_000,
		MinBitrate: 500_000, MaxBitrate: 20_000_000,
	})
	c.Observe(Feedback{Peers: 1, Bitrate: 20_000_000, Rate: 10})
	if fps, _ := target(t, c); fps != 10 {
		t.Fatalf("fps = %d, want 10", fps)
	}
	// The machine now keeps up with everything asked of it, so the usual step
	// stands and the rate climbs until it reaches the ceiling.
	for range 20 {
		clk.at = clk.at.Add(2 * time.Second)
		c.Observe(Feedback{Peers: 1, Bitrate: 20_000_000, Rate: 60})
	}
	if fps, _ := target(t, c); fps != 60 {
		t.Errorf("fps = %d, want 60", fps)
	}
}

func TestHighBitrateNeedsDebounce(t *testing.T) {
	c, clk := newTestController(t, Config{MaxFPS: 30, MinFPS: 5, Bitrate: 1_000_000, MinBitrate: 100_000, MaxBitrate: 8_000_000})
	c.Observe(roomy(0))
	_, bitrate := target(t, c)

	// Comfortable every time, but ten seconds is the shortest gap between two
	// increases.
	clk.at = clk.at.Add(time.Second)
	c.Observe(roomy(0))
	if _, got := target(t, c); got != bitrate {
		t.Fatalf("bitrate = %d, want %d before the debounce expires", got, bitrate)
	}
	clk.at = clk.at.Add(10 * time.Second)
	c.Observe(roomy(0))
	// The climb needs a streak of its own before the debounce is consulted.
	c.Observe(roomy(0))
	_, got := target(t, c)
	if got <= bitrate {
		t.Fatalf("bitrate = %d, want above %d once the debounce expires", got, bitrate)
	}
	if got > 8_000_000 {
		t.Errorf("bitrate = %d, want at most 8000000", got)
	}
}

func TestHighRateAndLossTriggerDown(t *testing.T) {
	for name, f := range map[string]Feedback{
		"round trip": {Peers: 1, Bitrate: 100_000_000, RTT: 900 * time.Millisecond},
		"loss":       {Peers: 1, Bitrate: 100_000_000, Loss: 0.4},
	} {
		c, _ := newTestController(t, slow)
		c.Observe(roomy(0))
		c.Observe(roomy(0))
		// Plenty of measured bandwidth, so only the latency and loss can be the
		// reason to back off. Congestion is acted on at once, without waiting
		// for the ramp-up streak to break.
		c.Observe(f)
		if _, bitrate := target(t, c); bitrate != 1_000_000 {
			t.Errorf("%s: bitrate = %d, want 1000000: the bitrate was already far under the estimate", name, bitrate)
		}
	}
}

func TestNoViewersResets(t *testing.T) {
	c, clk := newTestController(t, slow)
	for range 8 {
		clk.at = clk.at.Add(2 * time.Second)
		c.Observe(Feedback{Peers: 1, Bitrate: 50_000})
	}
	if _, bitrate := target(t, c); bitrate == 1_000_000 {
		t.Fatalf("setup: bitrate = 1000000, want it to have dropped")
	}

	clk.at = clk.at.Add(2 * time.Second)
	c.Observe(Feedback{Peers: 0})
	if fps, bitrate := target(t, c); fps != 30 || bitrate != 1_000_000 {
		t.Errorf("after idle: %d fps at %d bps, want 30 fps at 1000000 bps", fps, bitrate)
	}
}

// TestSettlesUnderAFixedNetwork drives the controller against a constant
// capacity for long enough to be sure it finds a stable bitrate instead of
// hunting, and that the bitrate it settles on both fits and comes close to what
// the viewers could actually take.
func TestSettlesUnderAFixedNetwork(t *testing.T) {
	for _, usable := range []int{120_000, 400_000, 900_000, 2_000_000, 6_000_000, 20_000_000} {
		c, clk := newTestController(t, Config{
			MaxFPS: 30, MinFPS: 5,
			Bitrate: 4_000_000, MinBitrate: 100_000, MaxBitrate: 12_000_000,
		})
		room := Feedback{Peers: 1, Bitrate: int(float64(usable) / 1.15)}

		var fps, bitrate int
		for range 120 {
			clk.at = clk.at.Add(500 * time.Millisecond)
			c.Observe(room)
		}
		fps, bitrate = target(t, c)
		for range 20 {
			clk.at = clk.at.Add(500 * time.Millisecond)
			c.Observe(room)
			if gotFps, gotBitrate := target(t, c); gotFps != fps || gotBitrate != bitrate {
				t.Fatalf("usable %d: settled state moved from %d fps at %d bps to %d fps at %d bps", usable, fps, bitrate, gotFps, gotBitrate)
			}
		}
		// The dead band between the two thresholds is a fifth of the bitrate,
		// so a settled stream can sit a little above the reported rate without
		// having been caught out by the estimator.
		if float64(bitrate) > float64(usable)/tightFactor {
			t.Errorf("usable %d: bitrate %d exceeds the network", usable, bitrate)
		}
		if bitrate < usable/2 {
			t.Errorf("usable %d: bitrate %d leaves half the network unused", usable, bitrate)
		}
	}
}
