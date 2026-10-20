// Protocol portions adapted from SkyKoo/gptGrok2api at 15773099641575776025e21d1290b5209b058c5a. See LICENSE.reference.
package chatgpt

import (
	"bytes"
	"errors"
	"fmt"
	"image"
	"image/draw"
	"image/jpeg"
	"image/png"
	"net/http"
	"strings"

	"github.com/HugoSmits86/nativewebp"
)

// ImageOutputOptions describes output properties, never an editing operation.
// Defaults follow Images API: background=auto and output_format=png.
type ImageOutputOptions struct {
	Background   string `json:"background,omitempty"`
	OutputFormat string `json:"output_format,omitempty"`
	// Size is the requested WIDTHxHEIGHT, stated in the prompt only.
	Size string `json:"size,omitempty"`
}

type ImageOptionError struct {
	Param   string
	Message string
}

func (e *ImageOptionError) Error() string { return e.Message }

func (o ImageOutputOptions) Validate() error {
	switch o.Background {
	case "", "auto", "transparent", "opaque":
	default:
		return &ImageOptionError{Param: "background", Message: "background must be auto, transparent, or opaque"}
	}
	switch o.OutputFormat {
	case "", "png", "jpeg", "webp":
	default:
		return &ImageOptionError{Param: "output_format", Message: "output_format must be png, jpeg, or webp"}
	}
	if o.Background == "transparent" && o.OutputFormat == "jpeg" {
		return &ImageOptionError{Param: "output_format", Message: "background=transparent requires output_format=png or webp; jpeg does not support transparency"}
	}
	return nil
}

func (o ImageOutputOptions) EffectiveFormat() string {
	if o.OutputFormat == "" {
		return "png"
	}
	return o.OutputFormat
}

func (o ImageOutputOptions) promptSuffix() string {
	var suffix string
	// ChatGPT Web ignores image_generation_size: the model takes the shape
	// from the prompt and draws about 1.57 MP, so the shape matches, not the
	// pixel count.
	var width, height int
	if _, err := fmt.Sscanf(o.Size, "%dx%d", &width, &height); err == nil && width > 0 && height > 0 {
		shape := "square"
		if width > height {
			shape = "landscape"
		} else if width < height {
			shape = "portrait"
		}
		divisor := gcd(width, height)
		suffix = fmt.Sprintf("\nOutput requirement: %dx%d pixels (%s, aspect ratio %d:%d).", width, height, shape, width/divisor, height/divisor)
	}
	background := o.Background
	if o.OutputFormat == "jpeg" && (background == "" || background == "auto") {
		background = "opaque"
	}
	switch background {
	case "transparent":
		suffix += "\nOutput requirement: use a genuinely transparent background with an alpha channel (background pixels alpha=0). Do not simulate transparency with a painted checkerboard, white backdrop, or solid-color backdrop. Follow the user's requested content and editing operation."
	case "opaque":
		suffix += "\nOutput requirement: use a fully opaque background, with no transparent or semi-transparent pixels. Follow the user's requested content and editing operation."
	}
	// Request a PNG source from Web; final format is encoded locally. This does
	// not rely on unverified Web support for Images API output_format fields.
	if o.OutputFormat != "" || o.Background == "transparent" {
		suffix += "\nOutput file format: PNG. Preserve the alpha channel when the background is transparent."
	}
	return suffix
}

// A completed generation that fails the requested output contract must not
// trigger another paid generation or mark its account as invalid.
type imageOutputError struct{ err error }

func (e *imageOutputError) Error() string { return e.err.Error() }
func (e *imageOutputError) Unwrap() error { return e.err }

// ImageOutputFailure tells callers outside this package that downloading the
// same image again cannot satisfy the output options.
func (e *imageOutputError) ImageOutputFailure() bool { return true }
func IsImageOutputError(err error) bool {
	var output *imageOutputError
	return errors.As(err, &output)
}

func invalidImageOutput(message string) error {
	return &imageOutputError{err: &upstreamError{Status: http.StatusBadGateway, Message: message}}
}

// apply checks actual pixels and encodes the final format before persistence.
// JPEG with automatic background composites alpha over white to avoid black
// edges; PNG/WebP preserve alpha. No subject segmentation is performed.
func (o ImageOutputOptions) apply(raw []byte, mime string) ([]byte, string, error) {
	return o.applyDecoded(downloadedImage{raw: raw, mime: mime})
}

// applyDecoded is apply on a download whose pixels decodeImageDownload already
// decoded (bounded there); without them raw is decoded here.
func (o ImageOutputOptions) applyDecoded(download downloadedImage) ([]byte, string, error) {
	raw, mime, decoded := download.raw, download.mime, download.decoded
	target := o.EffectiveFormat()
	if o.Background != "transparent" && o.Background != "opaque" && mime == "image/"+target {
		return raw, mime, nil
	}
	format := strings.TrimPrefix(mime, "image/")
	var err error
	if decoded == nil {
		if decoded, format, err = image.Decode(bytes.NewReader(raw)); err != nil {
			return nil, "", invalidImageOutput("OpenAI image output could not be decoded")
		}
	}
	if o.Background == "transparent" || o.Background == "opaque" {
		var transparent, visible, nonOpaque bool
		eachAlphaRow(decoded, func(_ int, alpha []uint16) bool {
			for _, a := range alpha {
				transparent, visible, nonOpaque = transparent || a == 0, visible || a > 0, nonOpaque || a < 0xffff
			}
			// Stop once the requested background is decided.
			if o.Background == "opaque" {
				return !nonOpaque
			}
			return !transparent || !visible
		})
		if o.Background == "transparent" && (!transparent || !visible) {
			return nil, "", invalidImageOutput("OpenAI image output did not satisfy background=transparent: expected transparent and visible pixels")
		}
		if o.Background == "opaque" && nonOpaque {
			return nil, "", invalidImageOutput("OpenAI image output did not satisfy background=opaque: transparent pixels were returned")
		}
	}
	if format == target {
		return raw, mime, nil
	}
	var output bytes.Buffer
	switch target {
	case "png":
		err = png.Encode(&output, decoded)
	case "jpeg":
		// JPEG has no alpha channel. draw.Over handles premultiplied edge colors
		// correctly instead of dropping alpha and creating dark fringes.
		flattened := image.NewRGBA(decoded.Bounds())
		draw.Draw(flattened, flattened.Bounds(), image.White, image.Point{}, draw.Src)
		draw.Draw(flattened, flattened.Bounds(), decoded, decoded.Bounds().Min, draw.Over)
		err = jpeg.Encode(&output, flattened, &jpeg.Options{Quality: 100})
	case "webp":
		if decoded.Bounds().Dx() > 16384 || decoded.Bounds().Dy() > 16384 {
			return nil, "", invalidImageOutput("OpenAI image output exceeds WebP's 16384-pixel edge limit")
		}
		// VP8L retains alpha without an extended VP8X container or C libraries.
		err = nativewebp.Encode(&output, decoded, nil)
	default:
		return nil, "", invalidImageOutput("OpenAI image output format is unsupported")
	}
	if err != nil || output.Len() > 64<<20 {
		return nil, "", invalidImageOutput("OpenAI image output could not be encoded as " + target + " within the size limit")
	}
	return output.Bytes(), "image/" + target, nil
}

// eachAlphaRow calls fn with the alpha of each row of img, as color.Color.RGBA
// reports it, until fn returns false. *image.NRGBA and *image.RGBA, which the
// decoders return for 8-bit images, are read straight from Pix: At would box
// every pixel.
func eachAlphaRow(img image.Image, fn func(y int, alpha []uint16) bool) {
	bounds := img.Bounds()
	row := make([]uint16, bounds.Dx())
	for y := bounds.Min.Y; y < bounds.Max.Y; y++ {
		var pix []uint8
		switch m := img.(type) {
		case *image.NRGBA:
			pix = m.Pix[m.PixOffset(bounds.Min.X, y):]
		case *image.RGBA:
			pix = m.Pix[m.PixOffset(bounds.Min.X, y):]
		}
		for i := range row {
			if pix == nil {
				_, _, _, a := img.At(bounds.Min.X+i, y).RGBA()
				row[i] = uint16(a)
				continue
			}
			row[i] = uint16(pix[4*i+3]) * 0x101
		}
		if !fn(y, row) {
			return
		}
	}
}

func gcd(a, b int) int {
	for b != 0 {
		a, b = b, a%b
	}
	return a
}
