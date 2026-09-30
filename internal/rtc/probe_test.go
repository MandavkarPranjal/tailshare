package rtc

import (
	"testing"
	"time"

	"github.com/pion/rtcp"
)

// ntpMid is the middle 32 bits of the NTP timestamp for a time, which is what
// a receiver report carries.
func ntpMid(at time.Time) uint32 {
	full := uint64(at.Unix()+ntpEpoch)<<32 | uint64(at.Nanosecond())<<32/uint64(time.Second)
	return uint32(full >> 16)
}

func receiverReport(blocks ...rtcp.ReceptionReport) []byte {
	raw, err := (&rtcp.ReceiverReport{SSRC: 1, Reports: blocks}).Marshal()
	if err != nil {
		panic(err)
	}
	return raw
}

// ntpTurn is how long the middle 32 bits take to come round again.
const ntpTurn = 65536 * time.Second

func TestNTPMiddleReadsTheMostRecentTurn(t *testing.T) {
	// A report names a moment roughly a round trip into the past, and the
	// middle bits are only good to within a second of it.
	at := time.Unix(1700000000, 500_000_000)
	for _, offset := range []time.Duration{0, -time.Second, -8 * time.Hour, -17 * time.Hour} {
		want := at.Add(offset).Truncate(time.Second)
		got := ntpMiddle(ntpMid(want), at)
		if d := got.Sub(want); d > time.Second || d < -time.Second {
			t.Errorf("ntpMiddle for %v gave %v, off by %v", offset, got, d)
		}
	}
}

func TestNTPMiddleTurnsBack(t *testing.T) {
	// The middle 32 bits repeat every 18 hours, so a value from another turn
	// is read as the most recent one that is still in the past.
	at := time.Unix(1700000000, 0)
	tests := []struct {
		name   string
		offset time.Duration
		turns  int
	}{
		{"a second into the next turn", time.Second, 1},
		{"an hour into the next turn", time.Hour, 1},
		{"more than a turn ahead", 40 * time.Hour, 3},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			original := at.Add(tt.offset)
			want := original.Add(-time.Duration(tt.turns) * ntpTurn)
			got := ntpMiddle(ntpMid(original), at)
			if d := got.Sub(want); d > time.Second || d < -time.Second {
				t.Errorf("ntpMiddle gave %v, want %v", got, want)
			}
		})
	}
}

func TestNTPMiddleNeverReadsTheFuture(t *testing.T) {
	// Halfway round the wheel is where the ambiguity is worst, and a round
	// trip measured against a time in the future is refused as a clock
	// problem rather than a very quick link.
	at := time.Unix(1700000000, 0)
	future := at.Add(ntpTurn / 2)
	got := ntpMiddle(ntpMid(future), at)
	if !got.Before(at) {
		t.Errorf("ntpMiddle gave %v for a report at %v, want a time in the past", got, future)
	}
}

func TestRoundTrip(t *testing.T) {
	// The report says the last sender report left 20ms before it arrived, and
	// the round trip was 50ms, so the sender report itself was 70ms old.
	const delay = 20 * time.Millisecond
	const rtt = 50 * time.Millisecond
	at := time.Unix(1700000000, 0)
	block := rtcp.ReceptionReport{
		Delay:            uint32(delay * 65536 / time.Second),
		LastSenderReport: ntpMid(at.Add(-delay - rtt)),
	}
	got, ok := roundTrip(block, at)
	if !ok {
		t.Fatal("a complete report was thrown away")
	}
	if d := got - rtt; d > time.Millisecond || d < -time.Millisecond {
		t.Errorf("round trip = %v, want about %v", got, rtt)
	}
}

func TestRoundTripRejectsIncompleteReports(t *testing.T) {
	at := time.Unix(1700000000, 0)
	full := rtcp.ReceptionReport{
		Delay:            1000,
		LastSenderReport: ntpMid(at.Add(-100 * time.Millisecond)),
	}
	tests := []struct {
		name  string
		block rtcp.ReceptionReport
	}{
		{
			// A viewer that has not seen a sender report yet cannot say how
			// long anything took.
			name:  "no sender report",
			block: rtcp.ReceptionReport{Delay: full.Delay},
		},
		{
			// No delay is reported until the first report has been answered.
			name:  "no delay",
			block: rtcp.ReceptionReport{LastSenderReport: full.LastSenderReport},
		},
		{
			// A sender report dated after now means the clocks disagree, not
			// that the link is impossibly quick.
			name: "from the future",
			block: rtcp.ReceptionReport{
				Delay:            full.Delay,
				LastSenderReport: ntpMid(at.Add(time.Minute)),
			},
		},
		{
			// Far enough back to be a clock problem rather than a slow link.
			name: "unreasonably long",
			block: rtcp.ReceptionReport{
				Delay:            full.Delay,
				LastSenderReport: ntpMid(at.Add(-time.Hour)),
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got, ok := roundTrip(tt.block, at); ok {
				t.Errorf("round trip = %v, want the report refused", got)
			}
		})
	}
}

func TestProbeReadsReceiverReports(t *testing.T) {
	const delay = 20 * time.Millisecond
	const rtt = 50 * time.Millisecond
	at := time.Unix(1700000000, 0)
	p := &probe{}
	p.record(receiverReport(rtcp.ReceptionReport{
		FractionLost:     0x40, // a quarter of the packets
		Delay:            uint32(delay * 65536 / time.Second),
		LastSenderReport: ntpMid(at.Add(-delay - rtt)),
	}), at)

	gotRTT, gotLoss := p.measurement()
	if d := gotRTT - rtt; d > time.Millisecond || d < -time.Millisecond {
		t.Errorf("round trip = %v, want about %v", gotRTT, rtt)
	}
	if d := gotLoss - 0.25; d > 0.001 || d < -0.001 {
		t.Errorf("loss = %v, want 0.25", gotLoss)
	}
}

func TestProbeKeepsTheLastReport(t *testing.T) {
	at := time.Unix(1700000000, 0)
	p := &probe{}
	p.record(receiverReport(rtcp.ReceptionReport{FractionLost: 0xff}), at)
	p.record(receiverReport(rtcp.ReceptionReport{FractionLost: 0x10}), at)
	if _, loss := p.measurement(); loss != 0.0625 {
		t.Errorf("loss = %v, want 0.0625", loss)
	}
}

func TestProbeIgnoresWhatItCannotUse(t *testing.T) {
	p := &probe{}
	// Nonsense, a report with no blocks, and one that cannot say about loss.
	p.record([]byte{0x01, 0x02, 0x03}, time.Now())
	p.record(receiverReport(), time.Now())
	p.record(receiverReport(rtcp.ReceptionReport{FractionLost: 0xff}), time.Now())
	if rtt, loss := p.measurement(); rtt != 0 || loss != 0 {
		t.Errorf("probe reported %v and %v from nothing usable", rtt, loss)
	}
}
