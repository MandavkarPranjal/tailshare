// Package encode turns captured JPEG frames into a compressed video stream
// that WebRTC viewers can play, using ffmpeg as the encoder.
//
// One Encoder owns one long-lived ffmpeg process. Frames are written to its
// standard input as JPEG images; coded frames are read back from its standard
// output.
//
// The process is told once, at startup, how fast the capture loop may go. From
// then on the encoder keeps that rate however many frames actually turn up, so
// every frame gets the same share of the bitrate and the rate the viewers see
// follows the rate the machine can keep up with. Changing how fast we capture is
// therefore free, and the only thing worth restarting the process for is a
// bitrate nobody can carry.
package encode

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Codec is a video codec tailshare can encode to.
type Codec string

// Supported codecs. CodecAuto picks H.264: it is the one every browser will
// take, it is decoded in hardware more often than not, and it survives a bad
// link better than the alternatives, which matters more here than the bitrate
// it saves.
const (
	CodecAuto Codec = "auto"
	CodecVP8  Codec = "vp8"
	CodecVP9  Codec = "vp9"
	CodecH264 Codec = "h264"
)

// ParseCodec turns a command line value into a Codec.
func ParseCodec(s string) (Codec, error) {
	c := Codec(strings.ToLower(strings.TrimSpace(s)))
	switch c {
	case CodecAuto, CodecVP8, CodecVP9, CodecH264:
		return c, nil
	}
	return "", fmt.Errorf("encode: unknown codec %q (want auto, vp8, vp9 or h264)", s)
}

// encoderNames lists the ffmpeg encoders to try for a codec, best first.
var encoderNames = map[Codec][]string{
	CodecVP8:  {"libvpx"},
	CodecVP9:  {"libvpx-vp9"},
	CodecH264: {"libx264", "libopenh264"},
}

// MimeTypes lists the WebRTC mime type per codec.
var mimeTypes = map[Codec]string{
	CodecVP8:  "video/VP8",
	CodecVP9:  "video/VP9",
	CodecH264: "video/H264",
}

// Options configures an Encoder.
type Options struct {
	// Codec selects the output codec. The zero value and CodecAuto both
	// select H.264.
	Codec Codec

	// Bitrate is the target rate in bits per second for the whole stream. Each
	// frame is given the same share of it, so a slower stream spends no more
	// in total and spends more per frame: a frame rate that gives way under
	// pressure costs smoothness and not sharpness. That is what makes the
	// frame rate the knob to turn and the bitrate the one to leave alone.
	Bitrate int

	// MaxFPS is the rate the capture loop may go as fast as, which is the rate
	// ffmpeg is told to expect and therefore the rate every frame's share of
	// the bitrate is worked out from. It is fixed for the life of the process.
	// 1 to 60, default 30.
	MaxFPS int

	// KeyframeInterval is the longest time in seconds between keyframes, timed
	// off the wall clock so it holds whatever rate the capture loop settles
	// at. A viewer that joins mid-stream has to wait for the next one before the
	// picture appears, so this trades bandwidth against start-up time.
	// 0 uses the default of 2 seconds.
	KeyframeInterval int

	// Width scales the picture to this width, keeping the aspect ratio.
	// 0 keeps the captured size, or whatever Height asks for.
	Width int

	// Height scales the picture to this many lines, keeping the aspect ratio,
	// and must be even: the picture is encoded as 4:2:0, which has no room for
	// an odd line. Set one of Width or Height, not both. A stream is scaled by
	// whichever of the two the viewer chose between, and a level that the
	// capture is too small for is taken off the ladder rather than being
	// enlarged to reach it.
	Height int

	// Binary is the ffmpeg executable to run. Empty means "ffmpeg".
	Binary string
}

// CodecInfo describes the video format the encoder produces in the terms
// WebRTC needs for SDP.
type CodecInfo struct {
	// Codec is the codec being encoded.
	Codec Codec

	// MimeType and ClockRate are the WebRTC codec capability fields.
	MimeType  string
	ClockRate int

	// SDPFmtp is the fmtp attribute for the codec. It is empty for VP8,
	// "profile-id=0" for VP9 and the parameter sets for H.264.
	SDPFmtp string

	// Ready reports whether SDPFmtp may be put in an SDP offer. Browsers want
	// the H.264 sequence and picture parameter sets, which only exist once the
	// encoder has emitted a keyframe, so H.264 becomes ready late.
	Ready bool
}

// Frame is one coded video frame, ready to be handed to a viewer.
type Frame struct {
	// Data is the coded frame payload, without container framing.
	Data []byte

	// Keyframe marks a frame a viewer can start decoding from.
	Keyframe bool

	// Time is when the frame is meant to be shown, on a clock that runs at a
	// steady rate: ffmpeg counts the frames it was told to expect rather than
	// the ones that actually arrived, and taking the gaps between arrivals
	// instead would hand every viewer a clock that stutters.
	Time time.Time

	// Duration is how much of that clock the frame takes up, which is the rate
	// the encoder is actually running at rather than the one it was told.
	Duration time.Duration
}

const (
	defaultKeyframeInterval = 2
	frameQueue              = 4
	// flushTimeout is how long ffmpeg gets to flush the last frames out
	// cleanly before it is killed.
	flushTimeout = 500 * time.Millisecond
	// minFrame and maxFrame bound the duration reported for a frame, so a
	// stalled or bursty capture cannot confuse a viewer's clock.
	minFrame = time.Millisecond
	maxFrame = 2 * time.Second
	// rateWindow is how many recent gaps the frame rate is worked out from. A
	// median over a few frames rides out a capture hiccup without dragging the
	// clock with it.
	rateWindow = 5
)

// ErrStopped is returned by Feed when the encoder is not running.
var ErrStopped = errors.New("encode: encoder is not running")

// Encoder feeds JPEG frames to ffmpeg and yields coded video frames.
//
// An Encoder is safe for concurrent use. It is normally started and stopped as
// viewers come and go rather than running for the lifetime of the process.
type Encoder struct {
	opts  Options
	codec Codec
	enc   string // ffmpeg encoder name
	bin   string

	frames chan Frame

	// mu guards the fields below: it says what the process is, not how the
	// encoder came to be that way.
	mu sync.Mutex
	// restart guards the handover from one ffmpeg process to the next, which is
	// a stop, a change and a start and has to happen to one caller at a time.
	// Two callers each doing their half find the encoder running when they come
	// to start it, and the loser reports an encoder that is already running
	// while leaving the bitrate it was asked for recorded on one still going at
	// the old rate, where the next change to that bitrate takes it for applied
	// and does nothing at all. It is taken before mu and never the other way
	// round, and holding mu across a stop would not serve instead, since
	// stopping a process means waiting for it to flush.
	restart  sync.Mutex
	ctx      context.Context
	bitrate  int
	fmtp     string
	sps, pps []byte
	ready    bool
	cmd      *exec.Cmd
	stdin    io.WriteCloser
	stderr   *ringBuffer
	stopping bool
	err      error
	done     chan struct{}
	lastKey  time.Time
	at       time.Time
}

// New validates opts and looks for a usable ffmpeg, but does not start it.
func New(opts Options) (*Encoder, error) {
	codec := opts.Codec
	if codec == "" || codec == CodecAuto {
		codec = CodecH264
	}
	if _, ok := encoderNames[codec]; !ok {
		return nil, fmt.Errorf("encode: unknown codec %q", codec)
	}
	if opts.MaxFPS == 0 {
		opts.MaxFPS = 30
	}
	if opts.MaxFPS < 1 || opts.MaxFPS > 60 {
		return nil, fmt.Errorf("encode: max fps %d out of range 1-60", opts.MaxFPS)
	}
	if opts.Bitrate == 0 {
		opts.Bitrate = 4_000_000
	}
	if opts.Bitrate < 50_000 || opts.Bitrate > 50_000_000 {
		return nil, fmt.Errorf("encode: bitrate %d out of range 50000-50000000", opts.Bitrate)
	}
	if opts.KeyframeInterval == 0 {
		opts.KeyframeInterval = defaultKeyframeInterval
	}
	if opts.KeyframeInterval < 0 || opts.KeyframeInterval > 10 {
		return nil, fmt.Errorf("encode: keyframe interval %d out of range 0-10", opts.KeyframeInterval)
	}
	if opts.Width < 0 || opts.Width > 0 && opts.Width < 320 {
		return nil, fmt.Errorf("encode: width %d out of range 0 or 320-7680", opts.Width)
	}
	// The -2 in the scale filter is the other dimension rounded to an even
	// number, so it covers the size of the source but not the one asked for
	// here: an odd width goes into the command line as it stands and the
	// process then fails to start on a picture 4:2:0 cannot have. It is refused
	// here, where the number is, rather than there, where the error is about a
	// scale and says nothing about which option was wrong.
	if opts.Width%2 != 0 {
		return nil, fmt.Errorf("encode: width %d must be even, 4:2:0 pictures have no odd column", opts.Width)
	}
	if opts.Height < 0 {
		return nil, fmt.Errorf("encode: height %d out of range", opts.Height)
	}
	if opts.Height%2 != 0 {
		return nil, fmt.Errorf("encode: height %d must be even, 4:2:0 pictures have no odd line", opts.Height)
	}
	if opts.Width > 0 && opts.Height > 0 {
		return nil, errors.New("encode: width and height are exclusive, scale by one or by the other")
	}
	bin := opts.Binary
	if bin == "" {
		bin = "ffmpeg"
	}
	path, err := exec.LookPath(bin)
	if err != nil {
		return nil, fmt.Errorf("encode: %w", err)
	}
	enc, err := pickEncoder(path, codec)
	if err != nil {
		return nil, err
	}
	e := &Encoder{
		opts:    opts,
		codec:   codec,
		enc:     enc,
		bin:     path,
		bitrate: opts.Bitrate,
		frames:  make(chan Frame, frameQueue),
		ready:   codec != CodecH264,
	}
	if codec == CodecVP9 {
		e.fmtp = "profile-id=0"
	}
	return e, nil
}

// pickEncoder returns the first encoder for codec that this ffmpeg has.
func pickEncoder(ffmpeg string, codec Codec) (string, error) {
	out, err := exec.Command(ffmpeg, "-hide_banner", "-encoders").Output()
	if err != nil {
		return "", fmt.Errorf("encode: %w", err)
	}
	for _, name := range encoderNames[codec] {
		for line := range strings.Lines(string(out)) {
			fields := strings.Fields(line)
			if len(fields) >= 2 && fields[1] == name {
				return name, nil
			}
		}
	}
	return "", fmt.Errorf("encode: ffmpeg has no encoder for %s (tried %s)", codec, strings.Join(encoderNames[codec], ", "))
}

// Name reports the ffmpeg encoder the Encoder runs, such as "libvpx".
func (e *Encoder) Name() string { return e.enc }

// Codec reports how the encoded stream can be described in SDP.
func (e *Encoder) Codec() CodecInfo {
	e.mu.Lock()
	defer e.mu.Unlock()
	return CodecInfo{
		Codec:     e.codec,
		MimeType:  mimeTypes[e.codec],
		ClockRate: 90000,
		SDPFmtp:   e.fmtp,
		Ready:     e.ready,
	}
}

// Frames returns the channel coded frames arrive on. It stays open across
// restarts so a caller can read from it for the life of the Encoder. Frames are
// dropped rather than queued up when nobody reads them.
func (e *Encoder) Frames() <-chan Frame { return e.frames }

// Rate reports the frame rate ffmpeg was told to expect, which is fixed for the
// life of the process. Every frame gets Bitrate/Rate bits, so this is also the
// rate the bitrate was chosen for: the stream runs at Rate only if the capture
// loop can keep up with it, and slower than that otherwise.
func (e *Encoder) Rate() int { return e.opts.MaxFPS }

// Bitrate reports the target rate in bits per second. While the encoder is
// stopped, this is the rate it will start at.
func (e *Encoder) Bitrate() int {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.bitrate
}

// SinceKeyframe reports how long ago the last keyframe came out of the encoder,
// or a long time if none has.
func (e *Encoder) SinceKeyframe() time.Duration {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.lastKey.IsZero() {
		return maxFrame
	}
	return time.Since(e.lastKey)
}

// Err reports why the ffmpeg process stopped, or nil if it is still running or
// was stopped on purpose.
func (e *Encoder) Err() error {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.err
}

// Running reports whether an ffmpeg process is attached.
func (e *Encoder) Running() bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.cmd != nil
}

// Start launches ffmpeg. It fails if the encoder is already running.
func (e *Encoder) Start(ctx context.Context) error {
	e.restart.Lock()
	defer e.restart.Unlock()
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.startLocked(ctx)
}

// Stop flushes ffmpeg, ending the stream. The Frames channel stays open.
//
// The input is taken away before the process is reaped, so a Feed that is on its
// way in finds nothing to write to and reports the encoder as stopped rather
// than writing into a closing pipe.
func (e *Encoder) Stop() {
	e.restart.Lock()
	defer e.restart.Unlock()
	e.stop()
}

// stop is Stop with the handover already held, for a caller that is part of a
// handover itself and must not have another one land in the middle of it.
func (e *Encoder) stop() {
	e.mu.Lock()
	cmd, stdin, done := e.cmd, e.stdin, e.done
	e.stdin = nil
	e.stopping = true
	e.mu.Unlock()
	if cmd == nil {
		return
	}
	// Closing the input is how ffmpeg is told there is nothing more to
	// encode, which makes it flush and exit on its own.
	if stdin != nil {
		_ = stdin.Close()
	}
	select {
	case <-done:
	case <-time.After(flushTimeout):
		_ = cmd.Process.Kill()
		<-done
	}
}

// SetBitrate restarts ffmpeg at a new bitrate. Changing how fast we capture
// costs nothing, so this is the only thing the process is ever restarted for, and
// only the bitrate a network cannot carry is worth it for. A stopped encoder is
// started, at that bitrate or the one it was left at; one already running at it
// is left alone. The first frame out of a new process is a keyframe, so viewers
// carry straight on.
//
// The stop, the new bitrate and the start are one handover, so two changes
// arriving together take their turn rather than overlapping: a change of bitrate
// part way through somebody else's is not a bitrate anybody asked for.
func (e *Encoder) SetBitrate(ctx context.Context, bps int) error {
	if bps < 50_000 || bps > 50_000_000 {
		return fmt.Errorf("encode: bitrate %d out of range 50000-50000000", bps)
	}
	e.restart.Lock()
	defer e.restart.Unlock()
	e.mu.Lock()
	unchanged := e.cmd != nil && e.bitrate == bps
	e.mu.Unlock()
	if unchanged {
		return nil
	}
	e.stop()
	e.mu.Lock()
	e.bitrate = bps
	err := e.startLocked(ctx)
	e.mu.Unlock()
	return err
}

// Feed hands one JPEG image to the encoder. It blocks while ffmpeg catches up,
// which is what paces the pipeline, and fails once the process is gone.
func (e *Encoder) Feed(ctx context.Context, jpeg []byte) error {
	if len(jpeg) == 0 {
		return nil
	}
	e.mu.Lock()
	stdin := e.stdin
	e.mu.Unlock()
	// A stopped encoder has no pipe to write to. It is taken away before the
	// process is reaped, so a process being on its way out looks the same as one
	// that has gone.
	if stdin == nil {
		return ErrStopped
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	// The frames are written one after another and ffmpeg's MJPEG demuxer finds
	// each one from the markers at its ends, so a frame of any size goes in as
	// it is and nothing has to be padded or restarted for.
	if _, err := stdin.Write(jpeg); err != nil {
		// Writing to a pipe that is being closed on purpose fails, and that is
		// the encoder being reconfigured or stopped rather than anything wrong
		// with the frames.
		e.mu.Lock()
		stopping := e.stopping
		e.mu.Unlock()
		if stopping {
			return ErrStopped
		}
		return fmt.Errorf("encode: write to ffmpeg: %w", err)
	}
	return nil
}

func (e *Encoder) startLocked(ctx context.Context) error {
	if e.cmd != nil {
		return errors.New("encode: already running")
	}
	cmd := exec.CommandContext(ctx, e.bin, e.argsLocked()...)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return fmt.Errorf("encode: %w", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return fmt.Errorf("encode: %w", err)
	}
	stderr := newRingBuffer()
	cmd.Stderr = stderr
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("encode: start ffmpeg: %w", err)
	}
	e.ctx, e.cmd, e.stdin, e.stderr, e.stopping, e.err = ctx, cmd, stdin, stderr, false, nil
	e.done = make(chan struct{})
	go e.readLoop(cmd, stdout, stderr, e.done)
	return nil
}

// argsLocked builds the ffmpeg command line.
func (e *Encoder) argsLocked() []string {
	filters := []string{"format=yuv420p"}
	// -2 is the other dimension rounded to an even number, which is what
	// 4:2:0 needs and what keeps a source size that is not even from failing.
	switch {
	case e.opts.Width > 0:
		filters = append([]string{fmt.Sprintf("scale=%d:-2", e.opts.Width)}, filters...)
	case e.opts.Height > 0:
		filters = append([]string{fmt.Sprintf("scale=-2:%d", e.opts.Height)}, filters...)
	}
	args := []string{
		"-hide_banner",
		"-loglevel", "error",
		"-nostdin",
		// Without these, probing buffers up to -probesize bytes (5MB by
		// default) before the first frame reaches the encoder, and nothing is
		// encoded or written until that buffer fills or the input ends. One
		// frame is all that is needed here: the format is fixed and the
		// dimensions come out of the JPEG header.
		"-probesize", "32",
		"-analyzeduration", "0",
		// Read the pictures as a run of JPEGs, taking the time each one was
		// written rather than counting them out at a fixed rate. That is what
		// makes the keyframe interval below a real number of seconds however
		// fast or slow the capture loop turns out to be running.
		"-use_wallclock_as_timestamps", "1",
		"-f", "mjpeg",
		// The rate the capture loop may go as fast as, which is what every
		// frame's share of the bitrate is worked out from. The process is
		// started with it and never asked to change it.
		"-framerate", strconv.Itoa(e.opts.MaxFPS),
		"-i", "pipe:0",
		"-an", "-sn", "-dn",
		"-vf", strings.Join(filters, ","),
		"-c:v", e.enc,
		"-b:v", strconv.Itoa(e.bitrate),
		// Keep the average at the target but let it borrow from the next
		// second, so a keyframe can be worth more than a plain frame and a
		// burst after a still screen does not have to be paid for by the frames
		// after it. Pinning every frame to an equal share instead leaves a
		// keyframe unable to afford the context the frames after it need.
		"-maxrate", strconv.Itoa(e.bitrate * 3 / 2),
		"-bufsize", strconv.Itoa(e.bitrate * 3 / 4),
		// A keyframe every KeyframeInterval seconds of wall clock. Asking for
		// them by frame count instead would make the interval stretch as the
		// capture loop slows down, and asking the process to change the interval
		// resets the encoder and asks for a keyframe on every change, which
		// costs far more bandwidth than the interval saves.
		"-force_key_frames", "expr:gte(t,n_forced*" + strconv.Itoa(e.opts.KeyframeInterval) + ")",
		// The captured timing goes straight through, so the rate ffmpeg stamps
		// the images at is the rate they are encoded at.
		"-fps_mode", "passthrough",
	}
	if e.codec == CodecH264 {
		args = append(args, "-preset", "veryfast", "-tune", "zerolatency", "-profile:v", "high", "-f", "h264", "pipe:1")
	} else {
		// A screen has little motion, so a fast setting costs little and
		// leaves the CPU free for capturing.
		cpuUsed := "4"
		if e.codec == CodecVP9 {
			cpuUsed = "8"
		}
		args = append(args,
			"-deadline", "realtime",
			"-cpu-used", cpuUsed,
			// No lookahead and no alt-ref frames: both would hold frames back
			// and add latency for no gain on a mostly static picture.
			"-lag-in-frames", "0",
			"-auto-alt-ref", "0",
			"-f", "ivf", "pipe:1",
		)
	}
	return args
}

// readLoop pulls coded frames out of ffmpeg until the stream ends.
func (e *Encoder) readLoop(cmd *exec.Cmd, stdout io.ReadCloser, stderr *ringBuffer, done chan struct{}) {
	defer close(done)
	reader, err := e.newReader(bufio.NewReaderSize(stdout, 64<<10))
	if err != nil {
		e.finish(cmd, err, stderr)
		return
	}
	// The clock carries over from the ffmpeg before this one, if there was one.
	// Starting again at the wall clock would hand a viewer a clock that had
	// jumped forwards by however long the gap was, and it would sit buffering
	// frames to cover a gap that is already in the past.
	clock := e.clock()
	meter := &rateMeter{}
	nominal := time.Second / time.Duration(e.opts.MaxFPS)
	for {
		f, err := reader.Next()
		if err != nil {
			e.finish(cmd, err, stderr)
			return
		}
		dur, now := meter.gap(nominal)
		if clock.Before(now) {
			// A restart that took longer than the frame it replaced is a gap
			// that cannot be hidden, so the clock goes back to following the
			// wall clock rather than falling further behind at every restart.
			clock = now
		}
		clock = clock.Add(dur)
		e.mu.Lock()
		if f.keyframe {
			e.lastKey = now
		}
		e.at = clock
		e.mu.Unlock()
		e.push(Frame{
			Data:     f.data,
			Keyframe: f.keyframe,
			Time:     clock,
			Duration: dur,
		})
	}
}

// rateMeter works out how fast coded frames are really arriving.
//
// It is the mean gap over the last few frames, and it has to be the mean rather
// than the middle one: ffmpeg hands out frames in bursts of two, so a middle
// gap is not representative of how fast the stream is going, and a clock built
// from one falls behind the wall clock at the rate the bursts are uneven by.
// Over a few frames the mean is not moved far by a single slow one.
type rateMeter struct {
	gaps [rateWindow]time.Duration
	n    int
	prev time.Time
}

// gap records an arrival and reports how far the media clock should advance by,
// and when the frame actually turned up. The nominal rate is used until there is
// anything to go on.
func (m *rateMeter) gap(nominal time.Duration) (time.Duration, time.Time) {
	now := time.Now()
	if !m.prev.IsZero() {
		m.gaps[m.n%rateWindow] = now.Sub(m.prev)
		m.n++
	}
	m.prev = now
	count := min(m.n, rateWindow)
	if count == 0 {
		return nominal, now
	}
	sum := time.Duration(0)
	for i := range count {
		sum += m.gaps[(m.n-1-i)%rateWindow]
	}
	return clampFrame(sum / time.Duration(count)), now
}

// clampFrame keeps the media clock within a range a viewer can make sense of,
// however strange the arrivals were.
func clampFrame(d time.Duration) time.Duration {
	return min(max(d, minFrame), maxFrame)
}

// clock returns the media clock the last coded frame was given.
func (e *Encoder) clock() time.Time {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.at
}

// median returns the middle of a sorted copy of these gaps. Only the tests want
// it; the clock itself is built from the mean, because ffmpeg hands out frames
// in bursts and a middle gap does not describe the rate.
func median(gaps []time.Duration) time.Duration {
	if len(gaps) == 0 {
		return 0
	}
	sorted := slices.Clone(gaps)
	slices.Sort(sorted)
	d := sorted[len(sorted)/2]
	return min(max(d, minFrame), maxFrame)
}

// newReader returns the demuxer for the elementary stream ffmpeg produces.
func (e *Encoder) newReader(r io.Reader) (codedReader, error) {
	if e.codec == CodecH264 {
		return newAnnexBReader(r, e.setParameterSets), nil
	}
	fourcc := "VP80"
	if e.codec == CodecVP9 {
		fourcc = "VP90"
	}
	return newIVFReader(r, fourcc)
}

// finish reaps the ffmpeg process and records why it ended.
func (e *Encoder) finish(cmd *exec.Cmd, readErr error, stderr *ringBuffer) {
	waitErr := cmd.Wait()
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.cmd == cmd {
		e.cmd, e.stdin = nil, nil
	}
	// A process that was stopped on purpose, or killed because ctx is done,
	// did not fail.
	if e.stopping || e.ctx.Err() != nil {
		return
	}
	switch {
	case readErr != nil && !errors.Is(readErr, io.EOF) && !errors.Is(readErr, io.ErrUnexpectedEOF):
		e.err = fmt.Errorf("encode: ffmpeg output: %w: %s", readErr, oneLine(stderr.String()))
	case waitErr != nil:
		e.err = fmt.Errorf("encode: ffmpeg: %w: %s", waitErr, oneLine(stderr.String()))
	default:
		e.err = fmt.Errorf("encode: ffmpeg stopped: %s", oneLine(stderr.String()))
	}
}

// setParameterSets records the H.264 parameter sets seen in a keyframe so that
// new viewers can be told about them in the SDP offer.
//
// A keyframe is where both of them are read, but either can be the one that has
// changed: ffmpeg writes a new picture parameter set on its own after a restart,
// and a viewer told the old one in an offer decodes the new pictures with the
// wrong picture parameter set or not at all. Both are compared rather than the
// sequence one alone, and the pair is stored together so what is offered is
// always the pair the stream is being decoded with.
func (e *Encoder) setParameterSets(sps, pps []byte) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.fmtp == "" || !slices.Equal(sps, e.sps) || !slices.Equal(pps, e.pps) {
		e.sps = append(e.sps[:0], sps...)
		e.pps = append(e.pps[:0], pps...)
		e.fmtp = parameterSetsFmtp(sps, pps)
		e.ready = len(sps) > 0 && len(pps) > 0
	}
}

// push queues a frame, dropping the oldest one if the reader has fallen behind.
func (e *Encoder) push(f Frame) {
	for {
		select {
		case e.frames <- f:
			return
		default:
			select {
			case <-e.frames:
			default:
			}
		}
	}
}

// codedFrame is one frame of an elementary stream.
type codedFrame struct {
	data     []byte
	keyframe bool
}

// codedReader splits an elementary video stream into frames.
type codedReader interface {
	Next() (codedFrame, error)
}

// ringBuffer keeps the tail of a stream, for error messages.
type ringBuffer struct {
	buf  []byte
	next int
	full bool
}

const ringSize = 2 << 10

func newRingBuffer() *ringBuffer {
	return &ringBuffer{buf: make([]byte, ringSize)}
}

func (r *ringBuffer) Write(p []byte) (int, error) {
	for _, b := range p {
		r.buf[r.next] = b
		r.next++
		if r.next == ringSize {
			r.next = 0
			r.full = true
		}
	}
	return len(p), nil
}

func (r *ringBuffer) String() string {
	if !r.full {
		return string(r.buf[:r.next])
	}
	return string(r.buf[r.next:]) + string(r.buf[:r.next])
}

func oneLine(s string) string {
	s = strings.TrimSpace(strings.ReplaceAll(s, "\n", "; "))
	if len(s) > 200 {
		s = s[:200] + "..."
	}
	if s == "" {
		return "no diagnostics"
	}
	return s
}
