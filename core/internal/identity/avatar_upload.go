package identity

import (
	"bytes"
	"context"
	"fmt"
	"image"
	_ "image/jpeg" // Register the JPEG decoder for image.Decode.
	"image/png"

	"github.com/google/uuid"
	"golang.org/x/image/draw"
	_ "golang.org/x/image/webp" // Register the WebP decoder for image.Decode.

	"github.com/orglane/orglane/core/internal/audit"
)

// MaxAvatarUploadBytes bounds untrusted input before image decoding.
const MaxAvatarUploadBytes = 5 * 1024 * 1024

const maxAvatarPixels = 16 * 1024 * 1024
const maxAvatarDimension = 512

// ProcessAvatar accepts untrusted raster input only. Decoding and fresh PNG
// encoding strip metadata, animation, trailing payloads and source encodings.
// Trusted demo SVGs continue through SetAvatar; uploads never use that path.
func ProcessAvatar(contentType string, data []byte) (Avatar, error) {
	formats := map[string]string{AvatarPNG: "png", "image/jpeg": "jpeg", AvatarWebP: "webp"}
	format, ok := formats[contentType]
	if !ok || len(data) == 0 || len(data) > MaxAvatarUploadBytes {
		return Avatar{}, fmt.Errorf("%w: expected PNG, JPEG or WebP of 1 to %d bytes", ErrUnsafeAvatar, MaxAvatarUploadBytes)
	}
	config, detected, err := image.DecodeConfig(bytes.NewReader(data))
	if err != nil || detected != format || config.Width <= 0 || config.Height <= 0 || config.Width > maxAvatarPixels/config.Height {
		return Avatar{}, fmt.Errorf("%w: invalid image type or dimensions (maximum 16 megapixels)", ErrUnsafeAvatar)
	}
	src, detected, err := image.Decode(bytes.NewReader(data))
	if err != nil || detected != format || src.Bounds().Dx() != config.Width || src.Bounds().Dy() != config.Height {
		return Avatar{}, fmt.Errorf("%w: image could not be decoded", ErrUnsafeAvatar)
	}
	for bound := maxAvatarDimension; bound >= 32; bound = bound * 3 / 4 {
		width, height := config.Width, config.Height
		if width > bound || height > bound {
			if width >= height {
				height = max(1, height*bound/width)
				width = bound
			} else {
				width = max(1, width*bound/height)
				height = bound
			}
		}
		dst := image.NewNRGBA(image.Rect(0, 0, width, height))
		draw.CatmullRom.Scale(dst, dst.Bounds(), src, src.Bounds(), draw.Src, nil)
		var output bytes.Buffer
		if err := png.Encode(&output, dst); err != nil {
			return Avatar{}, fmt.Errorf("identity: encode avatar: %w", err)
		}
		if output.Len() <= MaxAvatarBytes {
			return Avatar{ContentType: AvatarPNG, Data: output.Bytes()}, nil
		}
	}
	return Avatar{}, fmt.Errorf("%w: encoded image exceeds storage limit", ErrUnsafeAvatar)
}

// UploadAvatar processes an untrusted raster and stores it with its audit entry.
func (s *Service) UploadAvatar(ctx context.Context, actor audit.Actor, personID uuid.UUID, contentType string, data []byte) error {
	avatar, err := ProcessAvatar(contentType, data)
	if err != nil {
		return err
	}
	return s.SetAvatar(ctx, actor, personID, avatar)
}
