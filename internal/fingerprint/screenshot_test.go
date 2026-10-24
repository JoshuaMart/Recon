package fingerprint

import (
	"bytes"
	"encoding/base64"
	"image"
	"image/color"
	"image/jpeg"
	"testing"
)

// page draws a fake page: a header bar and some blocks, with the text area
// shifted by offset pixels.
func page(t *testing.T, offset int, dark bool) string {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, 1280, 800))
	bg := color.RGBA{250, 250, 250, 255}
	if dark {
		bg = color.RGBA{20, 20, 20, 255}
	}
	for y := range 800 {
		for x := range 1280 {
			c := bg
			switch {
			case y < 80:
				c = color.RGBA{30, 90, 200, 255}
			case x > 100+offset && x < 700+offset && y > 200 && y < 600 && (y/20)%2 == 0:
				c = color.RGBA{60, 60, 60, 255}
			}
			img.Set(x, y, c)
		}
	}
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, img, &jpeg.Options{Quality: 85}); err != nil {
		t.Fatal(err)
	}
	return base64.StdEncoding.EncodeToString(buf.Bytes())
}

func TestPrepareAndCompare(t *testing.T) {
	a, err := Prepare(page(t, 0, false), 70)
	if err != nil {
		t.Fatal(err)
	}
	if a.Width != 1280 || a.Height != 800 || len(a.Thumb) >= len(a.Image) {
		t.Errorf("shot = %dx%d, %d bytes, thumb %d bytes", a.Width, a.Height, len(a.Image), len(a.Thumb))
	}
	same, _ := Prepare(page(t, 2, false), 70)
	other, _ := Prepare(page(t, 0, true), 70)
	if d := Distance(a.PHash, same.PHash); d > SameImage {
		t.Errorf("a two-pixel shift is the same page, distance %d", d)
	}
	if d := Distance(a.PHash, other.PHash); d <= SameImage {
		t.Errorf("a dark theme is another page, distance %d", d)
	}
}

func TestDirRefusesTraversal(t *testing.T) {
	d := Dir{Root: t.TempDir()}
	for _, k := range []string{"../etc/passwd", "a/b.jpg", ".."} {
		if err := d.Put(t.Context(), k, []byte("x")); err == nil {
			t.Errorf("Put(%q) accepted", k)
		}
	}
	if err := d.Put(t.Context(), "abcdef.jpg", []byte("x")); err != nil {
		t.Fatal(err)
	}
	if b, err := d.Get(t.Context(), "abcdef.jpg"); err != nil || string(b) != "x" {
		t.Fatalf("Get = %q, %v", b, err)
	}
	if err := d.Delete(t.Context(), "abcdef.jpg"); err != nil {
		t.Fatal(err)
	}
	if err := d.Delete(t.Context(), "abcdef.jpg"); err != nil {
		t.Errorf("deleting a missing file: %v", err)
	}
}
