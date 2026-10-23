package fingerprint

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"image"
	"image/jpeg"
	"io/fs"
	"math/bits"
	"os"
	"path/filepath"
	"strings"
	"time"

	"golang.org/x/image/draw"
)

// Storage keeps screenshot files under content-addressed keys.
type Storage interface {
	Put(ctx context.Context, key string, data []byte) error
	Get(ctx context.Context, key string) ([]byte, error)
	Delete(ctx context.Context, key string) error
	// List calls fn for every stored key with its last write time.
	List(ctx context.Context, fn func(key string, written time.Time) error) error
}

// Dir stores files in a local directory, sharded by the key's first two
// characters so no directory holds hundreds of thousands of entries.
type Dir struct{ Root string }

func (d Dir) path(key string) (string, error) {
	if len(key) < 3 || strings.ContainsAny(key, "/\\") || strings.Contains(key, "..") {
		return "", fmt.Errorf("bad storage key %q", key)
	}
	return filepath.Join(d.Root, key[:2], key), nil
}

// Put writes atomically: a temporary file renamed into place.
func (d Dir) Put(_ context.Context, key string, data []byte) error {
	p, err := d.path(key)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o750); err != nil {
		return err
	}
	tmp := p + ".part"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, p)
}

// Get reads a file.
func (d Dir) Get(_ context.Context, key string) ([]byte, error) {
	p, err := d.path(key)
	if err != nil {
		return nil, err
	}
	return os.ReadFile(p) //nolint:gosec // the key is validated by path
}

// List walks the directory. Temporary files of a write in progress are skipped.
func (d Dir) List(ctx context.Context, fn func(key string, written time.Time) error) error {
	err := filepath.WalkDir(d.Root, func(path string, e fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if e.IsDir() || strings.HasSuffix(e.Name(), ".part") {
			return nil
		}
		info, err := e.Info()
		if err != nil {
			return err
		}
		return fn(e.Name(), info.ModTime())
	})
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	return err
}

// Delete removes a file; a missing one is not an error.
func (d Dir) Delete(_ context.Context, key string) error {
	p, err := d.path(key)
	if err != nil {
		return err
	}
	if err := os.Remove(p); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	return nil
}

// Shot is a screenshot ready to store.
type Shot struct {
	Image, Thumb  []byte
	Key, ThumbKey string
	PHash         int64
	Width, Height int
}

// ThumbWidth is the width of the thumbnail the grid shows.
const ThumbWidth = 320

// Prepare decodes the Fingerprinter's base64 JPEG, hashes it, and re-encodes
// it and its thumbnail at quality.
func Prepare(b64 string, quality int) (Shot, error) {
	raw, err := base64.StdEncoding.DecodeString(b64)
	if err != nil {
		return Shot{}, fmt.Errorf("screenshot: %w", err)
	}
	img, _, err := image.Decode(bytes.NewReader(raw))
	if err != nil {
		return Shot{}, fmt.Errorf("screenshot: %w", err)
	}
	full, err := encode(img, quality)
	if err != nil {
		return Shot{}, err
	}
	b := img.Bounds()
	tw := min(ThumbWidth, b.Dx())
	th := max(1, b.Dy()*tw/max(1, b.Dx()))
	small := image.NewRGBA(image.Rect(0, 0, tw, th))
	draw.ApproxBiLinear.Scale(small, small.Bounds(), img, b, draw.Src, nil)
	thumb, err := encode(small, quality)
	if err != nil {
		return Shot{}, err
	}
	return Shot{
		Image: full, Thumb: thumb, Key: contentKey(full), ThumbKey: contentKey(thumb),
		PHash: PHash(img), Width: b.Dx(), Height: b.Dy(),
	}, nil
}

func encode(img image.Image, quality int) ([]byte, error) {
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, img, &jpeg.Options{Quality: quality}); err != nil {
		return nil, fmt.Errorf("screenshot: %w", err)
	}
	return buf.Bytes(), nil
}

func contentKey(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:]) + ".jpg"
}

// PHash is the 64-bit perceptual hash of an image: reduced to 8x8 grey pixels,
// one bit per pixel brighter than their mean. A two-pixel shift or a JPEG
// re-encode moves a few bits; another layout or a theme change moves many.
func PHash(img image.Image) int64 {
	small := image.NewGray(image.Rect(0, 0, 8, 8))
	draw.ApproxBiLinear.Scale(small, small.Bounds(), img, img.Bounds(), draw.Src, nil)
	sum := 0
	for _, v := range small.Pix {
		sum += int(v)
	}
	mean := sum / len(small.Pix)
	var h uint64
	for _, v := range small.Pix {
		h <<= 1
		if int(v) > mean {
			h |= 1
		}
	}
	return int64(h) //nolint:gosec // stored as bigint, compared bitwise
}

// SameImage is the distance under which two screenshots are the same page.
const SameImage = 6

// Distance is the Hamming distance between two hashes.
func Distance(a, b int64) int { return bits.OnesCount64(uint64(a ^ b)) } //nolint:gosec // bit pattern
