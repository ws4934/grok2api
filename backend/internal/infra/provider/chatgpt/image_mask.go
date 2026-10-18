// Protocol portions adapted from SkyKoo/gptGrok2api at 15773099641575776025e21d1290b5209b058c5a. See LICENSE.reference.
package chatgpt

import (
	"bytes"
	"crypto/sha256"
	"image"
	"image/color"
	"image/png"
)

const MaxImageMaskBytes = 4 << 20

// maxImageMaskPixels is the largest image the tool draws (4096x4096). The mask
// is fully decoded: at most 64 MiB for 8-bit RGBA, 128 MiB for 16-bit.
const maxImageMaskPixels = 4096 * 4096

// ImageMask is an immutable, validated Web selection. Each generation uploads
// it separately; provider/account-specific file IDs must never be shared.
type ImageMask struct {
	png        []byte
	sourceHash [sha256.Size]byte
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
		return invalid("mask exceeds 4096x4096 pixels")
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
	for y := 0; y < cfg.Height; y++ {
		for x := 0; x < cfg.Width; x++ {
			_, _, _, alpha := decoded.At(x, y).RGBA()
			web.SetGray(x, y, color.Gray{Y: uint8(alpha >> 8)})
			selected = selected || alpha == 0
		}
	}
	if !selected {
		return invalid("mask must contain a fully transparent editing region")
	}
	var output bytes.Buffer
	if err := png.Encode(&output, web); err != nil {
		return invalid("could not encode mask selection")
	}
	return &ImageMask{png: output.Bytes(), sourceHash: sha256.Sum256(inputs[0].Data)}, nil
}

func (m *ImageMask) matches(inputs []OpenAIImageInput) bool {
	return m != nil && len(m.png) > 0 && len(inputs) == 1 && m.sourceHash == sha256.Sum256(inputs[0].Data)
}
