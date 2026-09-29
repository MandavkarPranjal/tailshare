// Package tailshare is a private, golink-style screen sharing service for
// your tailnet: run one binary on the machine whose screen you want to share
// and open its MagicDNS name in a browser to watch.
//
// Frames are captured with grim (Wayland) or ImageMagick's import (X11) and
// streamed as MJPEG. The default tailnet mode embeds a Tailscale node, so the
// stream is only reachable from your tailnet.
package tailshare
