// Package rtc delivers the encoded screen stream to viewers over WebRTC.
//
// Signalling is a single exchange: the viewer posts an offer and gets an
// answer back, with no candidates trickling in afterwards. Waiting for them
// costs a moment at the start and saves a round of requests per candidate,
// which for a page on the same tailnet is the better trade.
//
// Every viewer gets its own peer connection, its own measurement and its own
// place in the frame stream, so a viewer on a slow link is the only one held
// up by it. The server is told where the video comes from rather than knowing:
// it is handed the broadcast to subscribe viewers to, and asks for the codec
// when a viewer arrives, because that is not always known up front.
package rtc

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"maps"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/pion/interceptor"
	"github.com/pion/interceptor/pkg/cc"
	"github.com/pion/interceptor/pkg/gcc"
	"github.com/pion/rtcp"
	"github.com/pion/webrtc/v4"
	"github.com/pion/webrtc/v4/pkg/media"

	"tailshare/internal/encode"
	"tailshare/internal/stream"
)

const (
	// feedbackInterval is how often the viewers are measured.
	feedbackInterval = time.Second

	// disconnectGrace is how long a viewer that has stopped answering may stay
	// connected before it is given up on. A connection often recovers.
	disconnectGrace = 15 * time.Second

	// gatherTimeout caps the wait for ICE candidates. Going over it means
	// answering with what has been found so far, which is the best that can be
	// done without trickling.
	gatherTimeout = 5 * time.Second

	// maxOfferSize is generous for an SDP offer with every host address and
	// candidate in it, and small enough to refuse a body that is not one.
	maxOfferSize = 1 << 18

	// defaultInitialBitrate is the rate a viewer's congestion controller
	// starts from when the caller has no opinion. It only matters for the
	// first second or two, while the estimate is still climbing.
	defaultInitialBitrate = 1_000_000

	// keyframeWait is the shortest gap between two keyframe requests passed
	// on from the same viewer. It is also roughly how long a keyframe takes
	// to arrive, so asking more often than this cannot be answered any
	// sooner anyway.
	keyframeWait = 2 * time.Second
)

// Feedback is what the connected viewers can take, measured from this side.
type Feedback struct {
	// Quality is the level of the stream this is about, the name the viewer
	// asked for. It is empty when nobody is connected, which is a single report
	// about the whole room rather than one per level.
	Quality string

	// Peers is how many viewers are connected at this level.
	Peers int

	// Bitrate is the rate the tightest of them can take, in bits per second,
	// or 0 while none of them has said yet.
	Bitrate int

	// RTT is the slowest round trip of any of them.
	RTT time.Duration

	// Loss is the largest fraction of packets any of them is missing.
	Loss float64
}

// Stream is one rung of the quality ladder: the broadcast its viewers are fed
// from, and how to describe that broadcast.
//
// The levels are separate streams rather than one stream scaled for whoever is
// watching, because the resolution a viewer picks is a resolution the picture
// was encoded at, and because the viewers of one level are not entitled to hold
// down the picture being sent to the viewers of another.
type Stream struct {
	// Frames is the broadcast of coded frames this level's viewers are fed
	// from. Required.
	Frames *stream.Hub

	// Codec reports the codec the frames are in, for the offer that is
	// answered. Required. Its Ready flag decides whether a viewer can be
	// served at all: H.264 cannot be described until the encoder has produced
	// a keyframe to take its parameter sets from.
	Codec func() encode.CodecInfo

	// Prepare is called when a viewer arrives and the codec cannot be described
	// yet, which for H.264 means there is no keyframe to take the sequence and
	// picture parameter sets from. It is expected to make the codec describable
	// and to return once it is, because no viewer can be answered before then.
	// Optional; without it such a viewer is turned away.
	Prepare func(context.Context) error
}

// Config wires up the server.
type Config struct {
	// Streams gives the stream for a level, by the name a viewer asked for.
	// Required. A viewer that asks for a level that is not on offer is turned
	// away rather than quietly given a different picture than it asked for.
	Streams func(quality string) Stream

	// Identity names the viewer behind a request in the log. Optional.
	Identity func(*http.Request) string

	// InitialBitrate is the rate each viewer's congestion controller starts
	// from, in bits per second. 0 uses a default. Starting far below what the
	// encoder sends wastes the first seconds climbing back, so this is
	// normally the rate the stream is encoded at.
	InitialBitrate int

	// DisconnectGrace is how long a viewer that has stopped answering is kept
	// before it is dropped. 0 uses a default.
	DisconnectGrace time.Duration

	// OnViewers is called with the number of viewers at each level whenever any
	// of them changes, and once with none at startup. A level nobody is watching
	// is left out, so an absent key means an empty room rather than a stale
	// count. Optional.
	OnViewers func(map[string]int)

	// OnFeedback is called once a second with what the viewers of each level can
	// take, and once with no quality and no peers when the last one goes, which
	// is what a Close reports as it empties the room. Optional.
	OnFeedback func(Feedback)
}

// Server answers viewers and keeps their connections fed.
type Server struct {
	cfg            Config
	initialBitrate int
	grace          time.Duration

	// ctx bounds the life of every peer and of the monitor, so that shutting
	// the server down takes the connections with it. Close cancels it as well
	// as the caller does, so that both are one shutdown: a handshake still in
	// flight comes out of a context that is already done rather than one
	// nothing is left to watch.
	ctx    context.Context
	cancel context.CancelFunc

	mu sync.Mutex
	// closed says the server has been shut down and no longer answers viewers.
	// It is read where a peer is published, which is the last moment a peer
	// that would otherwise be missed by the shutdown can be turned away.
	closed bool
	peers  map[*peer]struct{}

	// lastReport is the feedback as each level was last written to the log, and
	// reported says which levels have had a line at all yet.
	lastReport map[string]Feedback
	reported   map[string]bool

	closeOnce sync.Once
}

// New starts a server. Viewers live until the connection to them fails, or
// until ctx is done.
func New(ctx context.Context, cfg Config) (*Server, error) {
	if cfg.Streams == nil {
		return nil, errors.New("rtc: streams are required")
	}
	s := &Server{
		cfg:            cfg,
		initialBitrate: cfg.InitialBitrate,
		grace:          cfg.DisconnectGrace,
		peers:          make(map[*peer]struct{}),
		lastReport:     make(map[string]Feedback),
		reported:       make(map[string]bool),
	}
	s.ctx, s.cancel = context.WithCancel(ctx)
	if s.initialBitrate <= 0 {
		s.initialBitrate = defaultInitialBitrate
	}
	if s.grace <= 0 {
		s.grace = disconnectGrace
	}
	if cfg.OnViewers != nil {
		cfg.OnViewers(map[string]int{})
	}
	go s.monitor(s.ctx)
	return s, nil
}

// signal is what the two sides exchange: one JSON object naming which way round
// it is, the level the viewer asked for, and carrying the session description.
type signal struct {
	Type    string `json:"type"`
	Quality string `json:"quality,omitempty"`
	SDP     string `json:"sdp"`
}

// httpError is a failure that says how it should be reported.
type httpError struct {
	status int
	err    error
}

func (e *httpError) Error() string { return e.err.Error() }

// errShuttingDown is what a viewer is told when it offers to a server that has
// been closed. It is a plain failure rather than a 404: the offer was perfectly
// good, and a viewer that retries once the server is back will be answered.
var errShuttingDown = errors.New("rtc: the server is shutting down")

// ServeHTTP answers one viewer: an offer in, an answer out.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	// A server on its way down has nothing left to feed a viewer, so an offer
	// that arrives after the shutdown is turned away rather than negotiated
	// from start to finish and then dropped.
	if s.shuttingDown() {
		http.Error(w, errShuttingDown.Error(), http.StatusServiceUnavailable)
		return
	}
	answer, p, err := s.answer(r)
	if err != nil {
		var he *httpError
		if errors.As(err, &he) {
			http.Error(w, he.Error(), he.status)
			return
		}
		log.Printf("webrtc: %v", err)
		http.Error(w, "cannot start a connection", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	if err := json.NewEncoder(w).Encode(answer); err != nil {
		// Without the answer the viewer will never connect, and nothing else
		// is going to tell us that it gave up.
		p.close()
		log.Printf("webrtc: viewer %s: write answer: %v", p.id, err)
	}
}

// answer negotiates with one viewer. The peer it returns carries on without
// the request once the answer is out, so it is handed back to be closed if the
// answer cannot be written.
func (s *Server) answer(r *http.Request) (signal, *peer, error) {
	var offer signal
	if err := json.NewDecoder(io.LimitReader(r.Body, maxOfferSize)).Decode(&offer); err != nil {
		return signal{}, nil, &httpError{http.StatusBadRequest, fmt.Errorf("rtc: decode offer: %w", err)}
	}
	if offer.Type != "offer" || offer.SDP == "" {
		return signal{}, nil, &httpError{
			http.StatusBadRequest,
			errors.New(`rtc: expected a body of {"type":"offer","quality":"720p","sdp":"..."}`),
		}
	}
	// The name the viewer picked is only ever compared against the names the
	// streams are registered under, and those are written the way a resolution
	// is. Folding case and stray space away here means a request that was
	// spelled "1080P" lands on the same stream and is counted and reported the
	// same way as one that was spelled right, instead of quietly becoming a
	// viewer of a level nothing else can hear about.
	name := strings.ToLower(strings.TrimSpace(offer.Quality))
	// An unknown level comes back as the zero stream, so the nil fields below
	// are what "not on offer" looks like.
	level := s.cfg.Streams(name)
	if level.Frames == nil || level.Codec == nil {
		// Asking for a level that is not on offer is the one request worth
		// refusing outright. Serving the wrong resolution is a picture the
		// viewer did not choose and cannot see that it did not get, and it is
		// what a page left over from a differently configured server would ask
		// for.
		return signal{}, nil, &httpError{
			http.StatusBadRequest,
			fmt.Errorf("rtc: no stream for quality %q", name),
		}
	}
	info := level.Codec()
	if !info.Ready && level.Prepare != nil {
		// The viewer cannot be answered until the codec can be described, and
		// for H.264 that needs a keyframe, which needs an encoder that is not
		// running because there are no viewers at this level yet. A viewer
		// asking is reason enough to start one.
		if err := level.Prepare(r.Context()); err != nil {
			return signal{}, nil, &httpError{http.StatusServiceUnavailable, err}
		}
		info = level.Codec()
	}
	if !info.Ready {
		return signal{}, nil, &httpError{
			http.StatusServiceUnavailable,
			fmt.Errorf("rtc: %s cannot be described yet", info.Codec),
		}
	}
	p, err := s.newPeer(r, name, level, info)
	if err != nil {
		return signal{}, nil, err
	}
	answer, err := p.negotiate(r.Context(), offer.SDP)
	if err != nil {
		p.close()
		return signal{}, nil, err
	}
	if !s.add(p) {
		// The server was shut down while this handshake was in flight, so the
		// peer was never in the snapshot Close took and nothing else is going
		// to close it. An answer would be a connection nothing is left to feed.
		p.close()
		return signal{}, nil, &httpError{http.StatusServiceUnavailable, errShuttingDown}
	}
	return answer, p, nil
}

// ViewerCount reports how many viewers are connected.
func (s *Server) ViewerCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.peers)
}

// levels reports how many viewers are at each level, for the OnViewers
// callback. It is a copy, so the caller cannot reach into the server's own
// bookkeeping, and a level nobody is watching is left out entirely rather than
// being reported as a zero: the answer is the set of levels in use, and a
// caller that wants a count for a level it owns can read the missing one as
// zero.
func (s *Server) levels() map[string]int {
	s.mu.Lock()
	defer s.mu.Unlock()
	counts := make(map[string]int, len(s.peers))
	for p := range s.peers {
		counts[p.quality]++
	}
	return counts
}

// Close drops every viewer and turns away the ones on their way in. It is safe
// to call more than once.
func (s *Server) Close() {
	s.closeOnce.Do(func() {
		s.mu.Lock()
		s.closed = true
		s.mu.Unlock()
		// The context every peer's comes from, so that a handshake still in
		// flight is taken down with the rest. Its peer is not in the snapshot
		// below and never will be, and without this it would be left holding a
		// connection that nothing is left to feed. It also stops the monitor,
		// so shutting the server down is the same thing however it is asked
		// for.
		s.cancel()
		for _, p := range s.snapshot() {
			p.close()
		}
		// The room is empty now, and an empty room is the report everything is
		// stood down from. While the server is running the monitor is what says
		// so, once a second; it has just been stopped, so a shutdown says it
		// here instead, once, however the server was closed.
		if s.cfg.OnFeedback != nil {
			s.cfg.OnFeedback(Feedback{})
		}
	})
}

// shuttingDown reports whether the server has been shut down.
func (s *Server) shuttingDown() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.closed
}

// add publishes a peer and starts it. It reports false if the server was shut
// down before the peer got here, leaving the peer to its caller to close: it was
// checked and put on the list under one lock, so a shutdown either had not
// started, and closes it with everything else, or had, and this peer never
// reaches the list that shutdown took.
func (s *Server) add(p *peer) bool {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return false
	}
	s.peers[p] = struct{}{}
	s.mu.Unlock()
	counts := s.levels()
	log.Printf("webrtc: viewer connected: %s (%s from %s, %s, %d connected)",
		p.id, p.identity, p.remote, p.quality, counts[p.quality])
	if s.cfg.OnViewers != nil {
		s.cfg.OnViewers(counts)
	}
	go p.run()
	return true
}

func (s *Server) remove(p *peer) {
	s.mu.Lock()
	_, was := s.peers[p]
	delete(s.peers, p)
	s.mu.Unlock()
	// A viewer that never made it out of the handshake was never counted.
	if !was {
		return
	}
	counts := s.levels()
	log.Printf("webrtc: viewer disconnected: %s (%s from %s, %s, %d connected)",
		p.id, p.identity, p.remote, p.quality, counts[p.quality])
	if s.cfg.OnViewers != nil {
		s.cfg.OnViewers(counts)
	}
}

// keyframe reports that a viewer has lost its way and asked for a new
// keyframe. There is nothing to do about it: the encoder puts one out every
// keyframe interval whether anybody asks or not, and a new process takes longer
// to start than the wait for the next one. So this only says it out loud, at
// most once per keyframeWait, because a browser that cannot decode anything is
// worth knowing about even when there is no way to help it.
func (s *Server) keyframe(id string) {
	log.Printf("webrtc: viewer %s: asked for a keyframe", id)
}

func (s *Server) snapshot() []*peer {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Collect(maps.Keys(s.peers))
}

// monitor measures the viewers and reports the tightest at each level, because
// one viewer on a slow link is what decides how fast that level's stream can
// go, and a viewer watching at one level says nothing about the link to the
// viewers watching at another.
//
// It runs until the server's context is done, which is either the caller's or
// Close's, and stops there itself rather than leaving the measurement going.
func (s *Server) monitor(ctx context.Context) {
	ticker := time.NewTicker(feedbackInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			s.Close()
			return
		case <-ticker.C:
			for _, f := range s.sample() {
				s.report(f)
				if s.cfg.OnFeedback != nil {
					s.cfg.OnFeedback(f)
				}
			}
		}
	}
}

// report logs what the viewers report back, but only when something in it has
// actually moved. The bitrate estimate is the number the rest of the service
// steers by, and it is invisible otherwise: a stream can sit at a tenth of
// what the link can carry for a long time without a single line in the log
// saying so. Each level is tracked on its own, so a level that has not moved
// says nothing when another one does.
func (s *Server) report(f Feedback) {
	if f.Peers == 0 {
		return
	}
	s.mu.Lock()
	prev, seen := s.lastReport[f.Quality], s.reported[f.Quality]
	s.lastReport[f.Quality], s.reported[f.Quality] = f, true
	s.mu.Unlock()
	if seen && unchanged(prev, f) {
		return
	}
	switch {
	case f.Bitrate <= 0:
		log.Printf("webrtc: %s: %d connected, no bandwidth estimate yet", f.Quality, f.Peers)
	default:
		log.Printf("webrtc: %s: %d connected, %d kbit/s estimated, %v away, %.1f%% lost",
			f.Quality, f.Peers, f.Bitrate/1000, f.RTT.Round(time.Millisecond), f.Loss*100)
	}
}

// unchanged reports whether a new sample says anything the old one did not.
// A rounding difference in the estimate is not news; a tenth of it is.
func unchanged(prev, next Feedback) bool {
	if prev.Peers != next.Peers || prev.RTT != next.RTT || prev.Loss != next.Loss {
		return false
	}
	if prev.Bitrate == 0 || next.Bitrate == 0 {
		return prev.Bitrate == next.Bitrate
	}
	diff := prev.Bitrate - next.Bitrate
	if diff < 0 {
		diff = -diff
	}
	return diff*10 <= max(prev.Bitrate, next.Bitrate)
}

// sample measures the viewers, one report per level, tightest viewer at each.
//
// An empty room reports a single Feedback with no quality and no peers, rather
// than one empty report per level: the point of it is that the last viewer has
// gone and everything can be stood down, and a caller cannot tell an empty room
// from a level nobody ever asked for.
func (s *Server) sample() []Feedback {
	peers := s.snapshot()
	if len(peers) == 0 {
		return []Feedback{{}}
	}
	order := make([]string, 0, len(peers))
	byQuality := make(map[string]Feedback, len(peers))
	for _, p := range peers {
		f, seen := byQuality[p.quality]
		if !seen {
			f.Quality = p.quality
			order = append(order, p.quality)
		}
		f.Peers++
		m := p.measure()
		if m.bitrate > 0 && (f.Bitrate == 0 || m.bitrate < f.Bitrate) {
			f.Bitrate = m.bitrate
		}
		f.RTT = max(f.RTT, m.rtt)
		f.Loss = max(f.Loss, m.loss)
		byQuality[p.quality] = f
	}
	// In the order the levels were first asked for, so the log and the callback
	// read the same way round every time rather than in map order.
	reports := make([]Feedback, 0, len(order))
	for _, name := range order {
		reports = append(reports, byQuality[name])
	}
	return reports
}

// peer is one viewer: a peer connection, the track the stream goes out on and
// what that connection says about its link.
type peer struct {
	server   *Server
	id       string
	identity string
	remote   string

	// quality is the level this viewer asked for, and stream is where its
	// frames come from. A peer is pinned to one level for life: the picture it
	// is being sent was encoded at that resolution, and swapping the stream out
	// from under a connection that is already decoding it would put frames from
	// two different encoders on one track.
	quality string
	stream  Stream

	ctx    context.Context
	cancel context.CancelFunc
	pc     *webrtc.PeerConnection
	track  *webrtc.TrackLocalStaticSample
	bwe    cc.BandwidthEstimator
	probe  *probe
	gate   gate
	once   sync.Once

	// mu guards the fields the connection's own callbacks touch. back is the
	// channel the disconnect timer is waiting on; closing it is how that timer
	// is told the viewer came back.
	mu      sync.Mutex
	codec   webrtc.RTPCodecParameters
	askedAt time.Time
	back    chan struct{}
}

func (s *Server) newPeer(r *http.Request, quality string, level Stream, info encode.CodecInfo) (*peer, error) {
	identity := "unknown"
	if s.cfg.Identity != nil {
		if id := s.cfg.Identity(r); id != "" {
			identity = id
		}
	}
	ctx, cancel := context.WithCancel(s.ctx)
	p := &peer{
		server:   s,
		id:       peerID(),
		identity: identity,
		remote:   r.RemoteAddr,
		quality:  quality,
		stream:   level,
		ctx:      ctx,
		cancel:   cancel,
		probe:    &probe{},
	}
	if err := p.dial(info); err != nil {
		cancel()
		return nil, err
	}
	return p, nil
}

// videoFeedback is the feedback every video codec here is offered. pion keeps
// the answer's feedback to what both ends asked for, so a codec registered
// without any of it ends up with an answer that negotiates no retransmission
// and no keyframe requests at all, and the viewer has no way to ask for either.
//
// Transport-wide congestion feedback is the one the estimator runs on. The
// receiver reports the sequence numbers it actually got, which is a measurement
// of the path rather than an opinion about the pictures, so the estimate it
// produces is what this link can carry rather than what a sender would like to
// send. Without it the answer negotiates none and the estimate stays at the
// bitrate it started with, however well or badly the link is doing.
var videoFeedback = []webrtc.RTCPFeedback{
	{Type: "nack"},
	{Type: "nack", Parameter: "pli"},
	{Type: "ccm", Parameter: "fir"},
	{Type: "goog-remb"},
	{Type: "transport-cc"},
}

// dial builds the peer connection, the track that carries the screen and the
// interceptors around them.
func (p *peer) dial(info encode.CodecInfo) error {
	capability := webrtc.RTPCodecCapability{
		MimeType:     info.MimeType,
		ClockRate:    uint32(info.ClockRate),
		SDPFmtpLine:  info.SDPFmtp,
		RTCPFeedback: videoFeedback,
	}
	// Kept for preferCodec, so the codec the answer asks for is the very same
	// one that was registered here rather than a second spelling of it.
	p.codec = webrtc.RTPCodecParameters{RTPCodecCapability: capability}
	// The media engine is built per viewer rather than shared because the
	// codec it advertises can change: a restarted encoder may hand out new
	// H.264 parameter sets, and a viewer joining later has to be told about
	// those rather than the ones from before.
	mediaEngine := &webrtc.MediaEngine{}
	if err := mediaEngine.RegisterCodec(p.codec, webrtc.RTPCodecTypeVideo); err != nil {
		return fmt.Errorf("rtc: viewer %s: register %s: %w", p.id, info.Codec, err)
	}
	registry := &interceptor.Registry{}
	// Google congestion control, which is the one number the stream rate is
	// set from: it watches the feedback viewers send back and settles on what
	// this link can carry.
	ccInterceptor, err := cc.NewInterceptor(func() (cc.BandwidthEstimator, error) {
		// The pacer is replaced with one that does nothing, and that is the
		// whole trick. Pion's leaky bucket queues whatever it is given and
		// sends it later at the rate it has been told, without ever dropping
		// anything, which is right for a file being read at a fixed rate and
		// badly wrong here: the encoder is ours, and when it is set above what
		// the estimate allows the difference does not vanish, it piles up in
		// the queue. The stream then lags further and further behind the wall
		// clock, the delay the estimator can see grows with it, and the
		// estimate collapses in response — a loop that ends with the picture
		// frozen and the bitrate cut to nothing. Writing straight through
		// turns the same mistake into packet loss instead, which the viewer
		// reports back and the controller can act on.
		est, err := gcc.NewSendSideBWE(
			gcc.SendSideBWEInitialBitrate(p.server.initialBitrate),
			gcc.SendSideBWEPacer(gcc.NewNoOpPacer()),
		)
		if err != nil {
			return nil, err
		}
		// The estimator is built as part of the connection it belongs to. It
		// is only read once the connection has been published, which the
		// server's lock orders after this runs.
		p.bwe = est
		return est, nil
	})
	if err != nil {
		return fmt.Errorf("rtc: viewer %s: bandwidth estimator: %w", p.id, err)
	}
	registry.Add(ccInterceptor)
	registry.Add(probeFactory{p.probe})
	// Transport feedback has to be registered after the estimator: an
	// interceptor added later wraps the ones before it, so this is the order
	// that puts the transport sequence numbers on the packet before the
	// estimator sees it and pairs it with the acknowledgements coming back.
	if err := webrtc.ConfigureTWCCHeaderExtensionSender(mediaEngine, registry); err != nil {
		return fmt.Errorf("rtc: viewer %s: transport feedback: %w", p.id, err)
	}
	if err := webrtc.RegisterDefaultInterceptors(mediaEngine, registry); err != nil {
		return fmt.Errorf("rtc: viewer %s: interceptors: %w", p.id, err)
	}

	api := webrtc.NewAPI(
		webrtc.WithMediaEngine(mediaEngine),
		webrtc.WithInterceptorRegistry(registry),
	)
	pc, err := api.NewPeerConnection(webrtc.Configuration{})
	if err != nil {
		return fmt.Errorf("rtc: viewer %s: peer connection: %w", p.id, err)
	}
	p.pc = pc
	track, err := webrtc.NewTrackLocalStaticSample(capability, "screen", "tailshare")
	if err != nil {
		_ = pc.Close()
		return fmt.Errorf("rtc: viewer %s: track: %w", p.id, err)
	}
	p.track = track
	sender, err := pc.AddTrack(track)
	if err != nil {
		_ = pc.Close()
		return fmt.Errorf("rtc: viewer %s: add track: %w", p.id, err)
	}
	pc.OnConnectionStateChange(p.stateChanged)
	// Reports come back on this reader and go nowhere else, and a connection
	// whose reports nobody reads cannot make progress.
	go p.drainReports(sender)
	return nil
}

// drainReports keeps the connection's RTCP moving.
//
// Reading the reports is not optional: a connection whose reports nobody reads
// cannot make progress. It is also the only place a viewer can ask for a
// keyframe, and a viewer that has lost one has no way back other than being
// given a new keyframe, so a picture loss indication is passed on rather than
// dropped.
func (p *peer) drainReports(sender *webrtc.RTPSender) {
	buf := make([]byte, 1500)
	for {
		n, _, err := sender.Read(buf)
		if err != nil {
			return
		}
		p.wantsKeyframe(buf[:n])
	}
}

// wantsKeyframe reports whether the viewer has asked for a keyframe, at most
// once per keyframeWait however hard it asks. A browser that has lost its way
// sends a request several times a second, and every one of them that came true
// is a line in the log.
//
// There is deliberately nothing here that answers the request. The encoder
// already puts out a keyframe every keyframe interval whether anybody asks or
// not, and a new process takes longer to come up than the wait for the next
// one, so restarting to hurry it along would freeze the picture and arrive
// later. A viewer that has lost its way gets back on at the next keyframe.
func (p *peer) wantsKeyframe(report []byte) bool {
	p.mu.Lock()
	fresh := time.Since(p.askedAt) >= keyframeWait
	p.mu.Unlock()
	if !fresh {
		return false
	}
	reports, err := rtcp.Unmarshal(report)
	if err != nil {
		return false
	}
	for _, r := range reports {
		switch r.(type) {
		case *rtcp.PictureLossIndication, *rtcp.FullIntraRequest:
		default:
			continue
		}
		p.mu.Lock()
		p.askedAt = time.Now()
		p.mu.Unlock()
		p.server.keyframe(p.id)
		return true
	}
	return false
}

// negotiate answers a remote offer, waiting for the local candidates so the
// whole answer can be sent in one piece.
func (p *peer) negotiate(ctx context.Context, offer string) (signal, error) {
	if err := p.pc.SetRemoteDescription(webrtc.SessionDescription{
		Type: webrtc.SDPTypeOffer,
		SDP:  offer,
	}); err != nil {
		return signal{}, fmt.Errorf("rtc: viewer %s: remote description: %w", p.id, err)
	}
	if err := p.preferCodec(); err != nil {
		return signal{}, fmt.Errorf("rtc: viewer %s: %w", p.id, err)
	}
	answer, err := p.pc.CreateAnswer(nil)
	if err != nil {
		return signal{}, fmt.Errorf("rtc: viewer %s: answer: %w", p.id, err)
	}
	gathered := webrtc.GatheringCompletePromise(p.pc)
	if err := p.pc.SetLocalDescription(answer); err != nil {
		return signal{}, fmt.Errorf("rtc: viewer %s: local description: %w", p.id, err)
	}
	select {
	case <-gathered:
	case <-time.After(gatherTimeout):
		log.Printf("webrtc: viewer %s: still gathering candidates, answering with those found", p.id)
	case <-ctx.Done():
		return signal{}, ctx.Err()
	}
	local := p.pc.LocalDescription()
	if local == nil {
		return signal{}, fmt.Errorf("rtc: viewer %s: no local description after gathering", p.id)
	}
	return signal{Type: "answer", SDP: local.SDP}, nil
}

// preferCodec makes the answer describe the codec the way the encoder does.
//
// pion builds the answer out of the codecs it matched against the offer, which
// for H.264 means the fmtp line the browser sent. A browser's offer has no
// sprop-parameter-sets in it, so neither does the answer, and a browser cannot
// configure an H.264 decoder without them: it accepts the connection, the
// packets arrive, and not one frame is ever decoded. Preferring our own
// capability puts the parameter sets into the answer. The payload type is left
// at zero because that is how pion is told to keep the number the offer chose:
// the answer has to use a payload type the browser offered, or it will not
// recognise the codec at all.
//
// The feedback travels with it for the same reason. pion narrows a preference
// to the feedback both ends asked for, so a preference carrying none of its
// own is narrowed to nothing and the answer ends up without a single
// a=rtcp-fb line: no retransmission, and no way for the viewer to ask for a
// keyframe when it loses one.
func (p *peer) preferCodec() error {
	preference := p.codec
	if p.codec.SDPFmtpLine == "" {
		// Nothing to add: VP8 and VP9 are described well enough by their mime
		// type, and the browser's own fmtp is the better one to echo. The
		// feedback still does, so this goes ahead for them too.
		preference = webrtc.RTPCodecParameters{RTPCodecCapability: webrtc.RTPCodecCapability{
			MimeType:     p.codec.MimeType,
			ClockRate:    p.codec.ClockRate,
			RTCPFeedback: p.codec.RTCPFeedback,
		}}
	}
	for _, transceiver := range p.pc.GetTransceivers() {
		if transceiver.Kind() != webrtc.RTPCodecTypeVideo {
			continue
		}
		if err := transceiver.SetCodecPreferences([]webrtc.RTPCodecParameters{preference}); err != nil {
			// The browser did not offer this codec at all, which is not
			// something to be loud about: the viewer page falls back to MJPEG.
			return fmt.Errorf("prefer %s: %w", p.codec.MimeType, err)
		}
	}
	return nil
}

// run feeds the viewer from the level it asked for until it goes away.
func (p *peer) run() {
	frames, unsubscribe := p.stream.Frames.Subscribe(p.ctx)
	defer unsubscribe()
	var sent bool
	for {
		select {
		case <-p.ctx.Done():
			return
		case f, ok := <-frames:
			if !ok {
				return
			}
			if !p.gate.allow(f) {
				continue
			}
			if !sent {
				// A viewer that is connected but sees nothing is the one thing
				// worth being able to account for afterwards.
				sent = true
				log.Printf("webrtc: viewer %s: first keyframe sent", p.id)
			}
			// The timestamp matters as much as the data. Left at the zero time it
			// reaches the viewer as an arbitrary point in the codec's clock, and the
			// jitter buffer built from that never plays anything out.
			if err := p.track.WriteSample(media.Sample{Data: f.Data, Duration: f.Duration, Timestamp: f.Time}); err != nil {
				log.Printf("webrtc: viewer %s: %v", p.id, err)
			}
		}
	}
}

func (p *peer) stateChanged(state webrtc.PeerConnectionState) {
	switch state {
	case webrtc.PeerConnectionStateConnected:
		log.Printf("webrtc: viewer %s: connected", p.id)
		// Any timer still running from an earlier disconnect belongs to a
		// disconnection this connection is no longer in.
		p.woke()
	case webrtc.PeerConnectionStateDisconnected:
		// A viewer that has gone quiet usually leaves the connection looking
		// disconnected for a while before it is declared failed, so give it a
		// chance to come back before taking the stream away from it. The timer
		// is waiting on this channel rather than on the grace period alone,
		// because a viewer that does come back before it runs out must keep its
		// stream: a grace period measured from the first disconnection closes
		// a connection that has been up and working since.
		go func(back <-chan struct{}) {
			select {
			case <-p.ctx.Done():
			case <-back:
			case <-time.After(p.server.grace):
				p.close()
			}
		}(p.disconnected())
	case webrtc.PeerConnectionStateFailed, webrtc.PeerConnectionStateClosed:
		// Closing from inside the callback would be closing a connection while
		// the code announcing that closure is still running.
		go p.close()
	}
}

// disconnected hands back the channel a disconnect timer waits on, and makes a
// fresh one: a viewer that drops again after coming back owes a new wait.
func (p *peer) disconnected() <-chan struct{} {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.back = make(chan struct{})
	return p.back
}

// woke says the connection is connected again, which retires any timer still
// counting down towards closing it.
func (p *peer) woke() {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.back != nil {
		close(p.back)
		p.back = nil
	}
}

// close drops the viewer. It is safe to call more than once.
func (p *peer) close() {
	p.once.Do(func() {
		p.cancel()
		if err := p.pc.Close(); err != nil && !errors.Is(err, webrtc.ErrConnectionClosed) {
			log.Printf("webrtc: viewer %s: close: %v", p.id, err)
		}
		p.server.remove(p)
	})
}

// measure reads what this viewer has said about its link.
func (p *peer) measure() measurement {
	m := measurement{}
	if p.bwe != nil {
		m.bitrate = p.bwe.GetTargetBitrate()
	}
	m.rtt, m.loss = p.probe.measurement()
	return m
}

// measurement is what one viewer can take right now.
type measurement struct {
	bitrate int
	rtt     time.Duration
	loss    float64
}

// gate holds frames back until a keyframe goes by.
//
// A viewer cannot decode anything before the first keyframe, and skipping the
// frames in between is cheaper than sending them. After that the frames go
// through as they are: the broadcast drops frames for a viewer that cannot
// keep up, and the picture may freeze until the next keyframe, which is what
// any video call does on a link that cannot carry the frames being sent.
type gate struct {
	started bool
}

// allow reports whether f should be sent.
func (g *gate) allow(f stream.Frame) bool {
	if !g.started && f.Keyframe {
		g.started = true
	}
	return g.started
}

func peerID() string {
	var b [4]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "?"
	}
	return hex.EncodeToString(b[:])
}
