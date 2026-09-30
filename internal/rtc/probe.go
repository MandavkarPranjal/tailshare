package rtc

import (
	"sync"
	"time"

	"github.com/pion/interceptor"
	"github.com/pion/rtcp"
)

const (
	// ntpEpoch is the number of seconds between the NTP epoch, which RTCP
	// timestamps count from, and the Unix one.
	ntpEpoch = 2208988800

	// maxRTT rejects a round trip time that is really a clock problem rather
	// than a slow link.
	maxRTT = 5 * time.Second
)

// probe reads what a viewer says about its link out of the RTCP it sends back:
// how long the round trip takes, and how much of the stream goes missing.
//
// It is an interceptor because that is the only place RTCP passes through on
// its way from the network to the bandwidth estimator, and an interceptor sees
// every report without having to own the connection.
type probe struct {
	interceptor.NoOp

	mu   sync.Mutex
	rtt  time.Duration
	loss float64
}

// BindRTCPReader watches the reports on their way to the reader below.
func (p *probe) BindRTCPReader(reader interceptor.RTCPReader) interceptor.RTCPReader {
	return interceptor.RTCPReaderFunc(func(b []byte, a interceptor.Attributes) (int, interceptor.Attributes, error) {
		n, a, err := reader.Read(b, a)
		if err != nil {
			return n, a, err
		}
		p.record(b[:n], time.Now())
		return n, a, err
	})
}

// record takes in one batch of RTCP.
func (p *probe) record(b []byte, at time.Time) {
	packets, err := rtcp.Unmarshal(b)
	if err != nil {
		return
	}
	for _, packet := range packets {
		report, ok := packet.(*rtcp.ReceiverReport)
		// A connection here carries a single video stream, so there is only
		// ever one report block in there worth reading.
		if !ok || len(report.Reports) == 0 {
			continue
		}
		p.recordBlock(report.Reports[0], at)
	}
}

func (p *probe) recordBlock(block rtcp.ReceptionReport, at time.Time) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if rtt, ok := roundTrip(block, at); ok {
		p.rtt = rtt
	}
	// The fraction counts 256ths of the packets lost since the last report, so
	// a full byte is 255 of every 256 of them and not an unknown. A viewer that
	// is getting almost nothing is the one most worth knowing about, and the
	// controller can only cut the stream down for it if it is told.
	p.loss = float64(block.FractionLost) / 256
}

// probeFactory puts a probe into an interceptor registry, which builds an
// interceptor for every connection. A probe belongs to one viewer and its one
// connection, so it is the same one every time.
type probeFactory struct {
	probe *probe
}

func (f probeFactory) NewInterceptor(string) (interceptor.Interceptor, error) {
	return f.probe, nil
}

// measurement reports the round trip time and the loss seen since, 0 for
// either that has not been reported yet.
func (p *probe) measurement() (time.Duration, float64) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.rtt, p.loss
}

// roundTrip works out how long a round trip took from a receiver report, which
// says when the viewer got our last sender report and how long ago that was.
func roundTrip(block rtcp.ReceptionReport, at time.Time) (time.Duration, bool) {
	if block.LastSenderReport == 0 || block.Delay == 0 {
		return 0, false
	}
	// The delay is in units of 1/65536 of a second.
	since := time.Duration(uint64(block.Delay) * uint64(time.Second) / 65536)
	rtt := at.Sub(ntpMiddle(block.LastSenderReport, at)) - since
	if rtt <= 0 || rtt > maxRTT {
		return 0, false
	}
	return rtt, true
}

// ntpMiddle turns the middle 32 bits of an NTP timestamp, which is all a
// receiver report carries, back into a time.
//
// Those bits count whole seconds and repeat every 18 hours, so the value is
// placed in the turn of the wheel around now and stepped back by a whole turn
// when it would otherwise land in the future. A viewer can only report
// something that has already happened, so the most recent turn is the one
// meant; a report that names a moment in the future is a clock problem, and
// the round trip check throws it out rather than believing it.
func ntpMiddle(mid uint32, at time.Time) time.Time {
	now := uint64(at.Unix()+ntpEpoch)<<32 | uint64(at.Nanosecond())<<32/uint64(time.Second)
	ts := (now & 0xffff000000000000) | uint64(mid)<<16
	if ts > now {
		// Sixteen bits of the seconds field, which is where the repeat is.
		ts -= 1 << 48
	}
	return time.Unix(int64(ts>>32)-ntpEpoch, int64(uint64(ts&0xffffffff)*uint64(time.Second)>>32))
}
