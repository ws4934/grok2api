package provider

import (
	"math"
	"strconv"
	"strings"
)

// Clients written for one image API send its shape parameters to every
// route: a ratio ("16:9"), a size ("1824x1024") or a resolution tier ("2k").
// These helpers read them so each provider can map them onto what it takes.

// ImagePair reads "16:9", "19.5:9", "1024x768" or "1024×768" as two positive
// numbers; 0, 0 when the value is neither a ratio nor a size.
func ImagePair(value string) (float64, float64) {
	value = strings.TrimSpace(value)
	for _, sep := range []string{"x", "X", "×", "*", ":", "/"} {
		left, right, ok := strings.Cut(value, sep)
		if !ok {
			continue
		}
		w, errW := strconv.ParseFloat(strings.TrimSpace(left), 64)
		h, errH := strconv.ParseFloat(strings.TrimSpace(right), 64)
		if errW != nil || errH != nil || w <= 0 || h <= 0 || math.IsInf(w, 0) || math.IsInf(h, 0) {
			return 0, 0
		}
		return w, h
	}
	return 0, 0
}

// ClosestImageRatio returns the choice whose ratio is nearest to that of
// value (a ratio or a size), or "" when value has no shape.
func ClosestImageRatio(value string, choices []string) string {
	w, h := ImagePair(value)
	if w == 0 {
		return ""
	}
	best, bestDiff := "", math.Inf(1)
	for _, choice := range choices {
		cw, ch := ImagePair(choice)
		if cw == 0 {
			continue
		}
		if diff := math.Abs(math.Log(w * ch / (h * cw))); diff < bestDiff {
			best, bestDiff = choice, diff
		}
	}
	return best
}

// ImageTierPixels reads a resolution tier as the long side it stands for:
// "1k" and "1K" are 1024, "2k" 2048, "4k" 4096, a bare number is itself and
// "1080p" is the long side of a 16:9 frame; 0 when the value is no tier.
func ImageTierPixels(value string) int {
	value = strings.ToLower(strings.TrimSpace(value))
	scale := 1.0
	switch {
	case strings.HasSuffix(value, "k"):
		value, scale = strings.TrimSuffix(value, "k"), 1024
	case strings.HasSuffix(value, "p"):
		value, scale = strings.TrimSuffix(value, "p"), 16.0/9
	}
	number, err := strconv.ParseFloat(value, 64)
	if err != nil || number <= 0 || number > 100000 {
		return 0
	}
	return int(math.Round(number * scale))
}
