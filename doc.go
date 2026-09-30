// Package tailshare is a private, golink-style screen sharing service for
// your tailnet: run one binary on the machine whose screen you want to share
// and open its MagicDNS name in a browser to watch.
//
// Frames are captured with grim (Wayland) or ImageMagick's import (X11),
// encoded with ffmpeg and sent to viewers as WebRTC video. The bitrate
// follows what the tightest viewer's connection can carry, the frame rate
// follows what the machine can manage, and nothing is captured or encoded at
// all while nobody is watching. A viewer gets a small, slow copy of the frames
// as an MJPEG stream underneath the WebRTC video, so the picture is up
// immediately, and keeps it for good if WebRTC does not work.
//
// The default tailnet mode embeds a Tailscale node, so the stream is only
// reachable from your tailnet.
package tailshare
