package creators

import "bytes"

// sniffFamily looks at the first bytes of a file and says which container family it belongs to:
// "iso" (MP4/MOV), "ebml" (Matroska/WebM), or "" when it is neither. It is a cheap first filter:
// the worker's ffprobe is the real authority on whether the file decodes.
func sniffFamily(head []byte) string {
	// EBML header: 1A 45 DF A3.
	if bytes.HasPrefix(head, []byte{0x1A, 0x45, 0xDF, 0xA3}) {
		return "ebml"
	}
	// ISO base media: a box whose type sits at offset 4. Files normally start with "ftyp", but
	// "moov", "mdat", "free", "wide" and "skip" boxes are also seen first in the wild.
	if len(head) >= 12 {
		switch string(head[4:8]) {
		case "ftyp", "moov", "mdat", "free", "wide", "skip":
			return "iso"
		}
	}
	return ""
}
