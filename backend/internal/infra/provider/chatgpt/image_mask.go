// Protocol portions adapted from SkyKoo/gptGrok2api at 15773099641575776025e21d1290b5209b058c5a. See LICENSE.reference.
package chatgpt

import (
	"bytes"
	"image"
	"image/png"
)

const MaxImageMaskBytes = 4 << 20

// maxImageMaskPixels is the largest image the tool draws (4096x4096). The mask
// is fully decoded: at most 64 MiB for 8-bit RGBA, 128 MiB for 16-bit.
const maxImageMaskPixels = 4096 * 4096

// ImageMask is an immutable, validated Web selection. Each generation uploads
// it separately; provider/account-specific file IDs must never be shared.
type ImageMask struct {
	png []byte
}

// ParseImageMask converts the Images alpha convention to ChatGPT Web's opaque
// grayscale selection: transparent -> black (edit), opaque -> white (preserve).
// Currently only the experimentally verified single-source operation is exposed.
func ParseImageMask(raw []byte, inputs []OpenAIImageInput) (*ImageMask, error) {
	invalid := func(message string) (*ImageMask, error) {
		return nil, &ImageOptionError{Param: "mask", Message: message}
	}
	if len(inputs) != 1 {
		return invalid("mask requires exactly one source image")
	}
	if len(raw) == 0 || len(raw) > MaxImageMaskBytes {
		return invalid("mask must contain 1 byte to 4 MiB")
	}
	cfg, err := png.DecodeConfig(bytes.NewReader(raw))
	if err != nil {
		return invalid("mask must be a valid PNG")
	}
	if cfg.Width <= 0 || cfg.Height <= 0 || int64(cfg.Width)*int64(cfg.Height) > maxImageMaskPixels {
		return invalid("mask exceeds the 4096x4096 (16.7 megapixel) limit")
	}
	source, _, err := image.DecodeConfig(bytes.NewReader(inputs[0].Data))
	if err != nil {
		return invalid("mask requires a valid source image")
	}
	if cfg.Width != source.Width || cfg.Height != source.Height {
		return invalid("mask dimensions must match the source image")
	}
	decoded, err := png.Decode(bytes.NewReader(raw))
	if err != nil {
		return invalid("mask must be a valid PNG")
	}
	web := image.NewGray(decoded.Bounds())
	selected := false
	eachAlphaRow(decoded, func(y int, alpha []uint16) bool {
		line := web.Pix[web.PixOffset(web.Rect.Min.X, y):]
		for x, a := range alpha {
			line[x] = uint8(a >> 8)
			selected = selected || a == 0
		}
		return true
	})
	if !selected {
		return invalid("mask must contain a fully transparent editing region")
	}
	var output bytes.Buffer
	if err := png.Encode(&output, web); err != nil {
		return invalid("could not encode mask selection")
	}
	return &ImageMask{png: output.Bytes()}, nil
}
