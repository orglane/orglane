package identity_test

import (
	"bytes"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"hash/crc32"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
	"testing"

	"github.com/orglane/orglane/core/internal/identity"
)

func rasterJPEG(t *testing.T) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, 1200, 600))
	for y := range 600 {
		for x := range 1200 {
			img.SetRGBA(x, y, color.RGBA{R: uint8(x % 256), G: uint8(y % 256), A: 255})
		}
	}
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, img, nil); err != nil {
		t.Fatal(err)
	}
	// A real JPEG APP1 marker, carrying an EXIF payload before image data.
	const exif = "Exif\x00\x00GPS secret location"
	marker := []byte{0xff, 0xe1, 0, byte(len(exif) + 2)}
	out := append([]byte{}, buf.Bytes()[:2]...)
	out = append(out, marker...)
	out = append(out, exif...)
	out = append(out, buf.Bytes()[2:]...)
	return out
}

func TestProcessAvatarReencodesBoundsAndStripsMetadata(t *testing.T) {
	in := rasterJPEG(t)
	avatar, err := identity.ProcessAvatar("image/jpeg", in)
	if err != nil {
		t.Fatal(err)
	}
	if avatar.ContentType != "image/png" || len(avatar.Data) > 256*1024 || bytes.Contains(avatar.Data, []byte("Exif")) || bytes.Contains(avatar.Data, []byte("GPS secret")) {
		t.Fatal("metadata or unbounded raster persisted")
	}
	img, format, err := image.Decode(bytes.NewReader(avatar.Data))
	if err != nil || format != "png" {
		t.Fatalf("output decode %s %v", format, err)
	}
	if img.Bounds().Dx() != 512 || img.Bounds().Dy() != 256 {
		t.Fatalf("proportional bounds = %v", img.Bounds())
	}
	again, err := identity.ProcessAvatar("image/png", avatar.Data)
	if err != nil || again.ContentType != "image/png" {
		t.Fatalf("PNG input: %v", err)
	}
}

func TestProcessAvatarAcceptsWebP(t *testing.T) {
	// One pixel, lossless WebP fixture.
	data, err := base64.StdEncoding.DecodeString("UklGRhoAAABXRUJQVlA4TA0AAAAvAAAAEAcQERGIiP4HAA==")
	if err != nil {
		t.Fatal(err)
	}
	avatar, err := identity.ProcessAvatar("image/webp", data)
	if err != nil {
		t.Fatal(err)
	}
	img, err := png.Decode(bytes.NewReader(avatar.Data))
	if err != nil || img.Bounds().Dx() != 1 || img.Bounds().Dy() != 1 {
		t.Fatalf("WebP output: %v %v", img, err)
	}
}

func TestProcessAvatarRejectsUnsafeInputs(t *testing.T) {
	// Valid PNG IHDR with oversized dimensions: reject before image allocation.
	huge := append([]byte{}, []byte("\x89PNG\r\n\x1a\n\x00\x00\x00\rIHDR")...)
	ihdr := make([]byte, 13)
	binary.BigEndian.PutUint32(ihdr, 100000)
	binary.BigEndian.PutUint32(ihdr[4:], 100000)
	ihdr[8] = 8
	ihdr[9] = 2
	huge = append(huge, ihdr...)
	crc := make([]byte, 4)
	binary.BigEndian.PutUint32(crc, crc32.ChecksumIEEE(huge[12:]))
	huge = append(huge, crc...)
	for name, tc := range map[string]struct {
		mime string
		data []byte
	}{
		"safe SVG is still rejected": {"image/svg+xml", []byte(safeSVG)},
		"script":                     {"image/png", []byte("<svg onload='alert(1)'/>")},
		"signature only":             {"image/png", []byte("\x89PNG\r\n\x1a\nrest")},
		"mismatched MIME":            {"image/png", rasterJPEG(t)},
		"truncated JPEG":             {"image/jpeg", rasterJPEG(t)[:80]},
		"decompression bomb":         {"image/png", huge},
		"oversized file":             {"image/png", make([]byte, 5*1024*1024+1)},
		"empty":                      {"image/jpeg", nil},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := identity.ProcessAvatar(tc.mime, tc.data); !errors.Is(err, identity.ErrUnsafeAvatar) {
				t.Fatalf("want unsafe avatar, got %v", err)
			}
		})
	}
}
