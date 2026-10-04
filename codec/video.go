package codec

import (
	"fmt"
	"strconv"
	"strings"
)

// VideoFormats lists the pixel formats ParseVideo understands, for help text.
const VideoFormats = "gray8, gray16, rgb24, bgr24, rgba, bgra, yuv420 (planar 4:2:0); default rgb24"

// ParseVideo turns a description of raw video, "WIDTHxHEIGHT" or
// "WIDTHxHEIGHT:FORMAT" (for example "1920x1080:yuv420"), into the two numbers
// Options needs: the bytes in one row of the folded matrix (Options.Width) and
// the rows in one frame (Options.FrameRows). Frames are assumed to follow one
// another with no headers, which is what raw capture tools write.
func ParseVideo(spec string) (width, frameRows int, err error) {
	dims, format, _ := strings.Cut(spec, ":")
	ws, hs, ok := strings.Cut(dims, "x")
	w, werr := strconv.Atoi(ws)
	h, herr := strconv.Atoi(hs)
	if !ok || werr != nil || herr != nil || w < 1 || h < 1 {
		return 0, 0, fmt.Errorf("video %q: want WIDTHxHEIGHT or WIDTHxHEIGHT:FORMAT, such as 1280x720:rgb24", spec)
	}
	switch strings.ToLower(format) {
	case "", "rgb24", "bgr24":
		width, frameRows = w*3, h
	case "gray8", "gray", "y8":
		width, frameRows = w, h
	case "gray16":
		width, frameRows = w*2, h
	case "rgba", "bgra", "argb":
		width, frameRows = w*4, h
	case "yuv420", "yuv420p", "i420":
		if h%2 != 0 {
			return 0, 0, fmt.Errorf("video %q: yuv420 needs an even height", spec)
		}
		width, frameRows = w, h*3/2
	default:
		return 0, 0, fmt.Errorf("video %q: unknown format %q (known: %s)", spec, format, VideoFormats)
	}
	if frameRows > 1<<16-1 {
		return 0, 0, fmt.Errorf("video %q: more than %d rows per frame", spec, 1<<16-1)
	}
	return width, frameRows, nil
}
