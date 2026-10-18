// Protocol portions adapted from SkyKoo/gptGrok2api at 15773099641575776025e21d1290b5209b058c5a. See LICENSE.reference.
package chatgpt

import (
	"bytes"
	"errors"
	"image"
	"image/draw"
	"image/jpeg"
	"image/png"
	"net/http"

	"github.com/HugoSmits86/nativewebp"
)

// ImageOutputOptions describes output properties, never an editing operation.
// Defaults follow Images API: background=auto and output_format=png.
type ImageOutputOptions struct {
	Background   string `json:"background,omitempty"`
	OutputFormat string `json:"output_format,omitempty"`
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
	background := o.Background
	if o.OutputFormat == "jpeg" && (background == "" || background == "auto") {
		background = "opaque"
	}
	switch background {
	case "transparent":
		suffix = "\nOutput requirement: use a genuinely transparent background with an alpha channel (background pixels alpha=0). Do not simulate transparency with a painted checkerboard, white backdrop, or solid-color backdrop. Follow the user's requested content and editing operation."
	case "opaque":
		suffix = "\nOutput requirement: use a fully opaque background, with no transparent or semi-transparent pixels. Follow the user's requested content and editing operation."
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
	target := o.EffectiveFormat()
	if o.Background != "transparent" && o.Background != "opaque" && mime == "image/"+target {
		return raw, mime, nil
	}
	// Downloads have already been decoded and bounded by validatedImageMIME.
	decoded, format, err := image.Decode(bytes.NewReader(raw))
	if err != nil {
		return nil, "", invalidImageOutput("OpenAI image output could not be decoded")
	}
	if o.Background == "transparent" || o.Background == "opaque" {
		var transparent, visible, nonOpaque bool
		bounds := decoded.Bounds()
		for y := bounds.Min.Y; y < bounds.Max.Y; y++ {
			for x := bounds.Min.X; x < bounds.Max.X; x++ {
				_, _, _, alpha := decoded.At(x, y).RGBA()
				transparent = transparent || alpha == 0
				visible = visible || alpha > 0
				nonOpaque = nonOpaque || alpha < 65535
			}
		}
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
