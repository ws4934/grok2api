// Tests adapted from SkyKoo/gptGrok2api 1577309; see LICENSE.reference.
package chatgpt

import (
	"bytes"
	"encoding/binary"
	"errors"
	"hash/crc32"
	"image"
	"image/color"
	"image/png"
	"testing"
)

func maskFixture(t *testing.T, width, height int, alpha uint8) []byte {
	t.Helper()
	im := image.NewNRGBA(image.Rect(0, 0, width, height))
	for y := 0; y < height; y++ {
		for x := 0; x < width; x++ {
			im.SetNRGBA(x, y, color.NRGBA{R: 230, G: 80, B: 170, A: alpha})
		}
	}
	var b bytes.Buffer
	if err := png.Encode(&b, im); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}
func TestImageMaskConversionAndSourceBinding(t *testing.T) {
	source := maskFixture(t, 2, 1, 255)
	input := []OpenAIImageInput{{Data: source}}
	im := image.NewNRGBA(image.Rect(0, 0, 2, 1))
	im.SetNRGBA(0, 0, color.NRGBA{R: 255, A: 0})
	im.SetNRGBA(1, 0, color.NRGBA{B: 255, A: 255})
	var b bytes.Buffer
	_ = png.Encode(&b, im)
	mask, err := ParseImageMask(b.Bytes(), input)
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := png.Decode(bytes.NewReader(mask.png))
	if err != nil {
		t.Fatal(err)
	}
	r, _, _, a := decoded.At(0, 0).RGBA()
	if r != 0 || a != 65535 {
		t.Fatal("transparent edit must become opaque black")
	}
	r, _, _, a = decoded.At(1, 0).RGBA()
	if r != 65535 || a != 65535 {
		t.Fatal("protected region must become opaque white")
	}
	if !mask.matches(input) || mask.matches([]OpenAIImageInput{{Data: maskFixture(t, 2, 1, 200)}}) || mask.matches(nil) {
		t.Fatal("mask must bind to the validated source")
	}
}
func TestImageMaskValidation(t *testing.T) {
	source := []OpenAIImageInput{{Data: maskFixture(t, 2, 2, 255)}}
	valid := maskFixture(t, 2, 2, 0)
	huge := bytes.Clone(valid)
	binary.BigEndian.PutUint32(huge[16:20], 100000)
	binary.BigEndian.PutUint32(huge[29:33], crc32.ChecksumIEEE(huge[12:29]))
	for _, tc := range []struct {
		name   string
		raw    []byte
		inputs []OpenAIImageInput
	}{
		{"empty", nil, source}, {"too-large", make([]byte, MaxImageMaskBytes+1), source},
		{"not-png", []byte("not PNG"), source}, {"bad-crc", valid[:len(valid)/2], source},
		{"opaque", maskFixture(t, 2, 2, 255), source}, {"no-transparent-selection", maskFixture(t, 2, 2, 128), source},
		{"wrong-size", maskFixture(t, 3, 2, 0), source}, {"decompression-bomb", huge, source},
		{"missing-source", valid, nil}, {"multiple-sources", valid, append(source, source...)},
		{"invalid-source", valid, []OpenAIImageInput{{Data: []byte("invalid")}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := ParseImageMask(tc.raw, tc.inputs)
			var option *ImageOptionError
			if !errors.As(err, &option) || option.Param != "mask" {
				t.Fatalf("expected mask error, got %v", err)
			}
		})
	}
}
