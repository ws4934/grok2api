package mediafile

import (
	"encoding/binary"
	"mime"
	"net/http"
	"strings"
)

// VideoType sniffs the video type of data from its first bytes: MP4 or
// QuickTime by the brands of the ISO BMFF ftyp box (HEIC and AVIF images share
// the box and get ""), WebM as net/http sniffs it. Anything else gets "".
func VideoType(data []byte) string {
	if len(data) >= 16 && string(data[4:8]) == "ftyp" {
		// A real ftyp box lists a few brands: a forged box size must not make
		// the scan walk the whole file.
		end := min(int(binary.BigEndian.Uint32(data[:4])), len(data), 256)
		brands := []string{string(data[8:12])} // major brand; data[12:16] is the minor version
		for i := 16; i+4 <= end; i += 4 {
			brands = append(brands, string(data[i:i+4]))
		}
		if brands[0] == "qt  " {
			return "video/quicktime"
		}
		for _, brand := range brands {
			switch brand {
			case "isom", "iso2", "iso3", "iso4", "iso5", "iso6", "mp41", "mp42", "avc1", "M4V ", "dash":
				return "video/mp4"
			case "qt  ":
				return "video/quicktime"
			}
		}
		return ""
	}
	if http.DetectContentType(data) == "video/webm" {
		return "video/webm"
	}
	return ""
}

// VideoExtension returns the canonical file extension for a supported video
// media type. Parameters such as codecs are ignored when selecting the suffix.
func VideoExtension(contentType string) (string, bool) {
	mediaType, _, err := mime.ParseMediaType(strings.TrimSpace(contentType))
	if err != nil {
		return "", false
	}
	switch strings.ToLower(mediaType) {
	case "video/mp4":
		return ".mp4", true
	case "video/webm":
		return ".webm", true
	case "video/quicktime":
		return ".mov", true
	default:
		return "", false
	}
}

// VideoContentDisposition builds a safe inline filename for video responses.
// Public request and asset identifiers are restricted to a conservative ASCII
// subset before they are placed in an HTTP response header.
func VideoContentDisposition(name, contentType string) string {
	name = strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
			return r
		case r == '-', r == '_':
			return r
		default:
			return -1
		}
	}, strings.TrimSpace(name))
	if name == "" {
		name = "video"
	}
	if extension, ok := VideoExtension(contentType); ok {
		name += extension
	}
	return `inline; filename="` + name + `"`
}
