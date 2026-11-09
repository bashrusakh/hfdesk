// Copyright 2026
// SPDX-License-Identifier: Apache-2.0

package icon

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"image"
	"image/color"
	"image/png"
	"io/fs"
	"testing"

	"github.com/bashrusakh/hfdesk/internal/assets"
)

// icoEntry is a parsed ICO directory entry.
type icoEntry struct {
	width  int
	height int
	planes uint16
	bpp    uint16
	size   uint32
	offset uint32
}

// parseICO parses the header and the directory entries of an ICO file.
//
// The parser is deliberately independent of the encoder, so a test does not
// check the encoder against itself.
func parseICO(t *testing.T, data []byte) []icoEntry {
	t.Helper()

	if len(data) < 6 {
		t.Fatalf("the ICO file is %d bytes long, want at least 6", len(data))
	}
	if reserved := binary.LittleEndian.Uint16(data[0:2]); reserved != 0 {
		t.Fatalf("the reserved field is %d, want 0", reserved)
	}
	if kind := binary.LittleEndian.Uint16(data[2:4]); kind != 1 {
		t.Fatalf("the type field is %d, want 1 (ICO)", kind)
	}
	count := int(binary.LittleEndian.Uint16(data[4:6]))
	if count == 0 {
		t.Fatal("the ICO file carries no image")
	}
	if len(data) < 6+16*count {
		t.Fatalf("the ICO file is %d bytes long, want at least %d", len(data), 6+16*count)
	}

	entries := make([]icoEntry, 0, count)
	for i := 0; i < count; i++ {
		entry := data[6+16*i : 6+16*(i+1)]
		entries = append(entries, icoEntry{
			width:  dimensionValue(entry[0]),
			height: dimensionValue(entry[1]),
			planes: binary.LittleEndian.Uint16(entry[4:6]),
			bpp:    binary.LittleEndian.Uint16(entry[6:8]),
			size:   binary.LittleEndian.Uint32(entry[8:12]),
			offset: binary.LittleEndian.Uint32(entry[12:16]),
		})
	}
	return entries
}

// dimensionValue decodes the dimension of an ICO directory entry. A stored
// zero means 256 pixels.
func dimensionValue(value byte) int {
	if value == 0 {
		return 256
	}
	return int(value)
}

// pngBytes encodes an image as a PNG.
func pngBytes(t *testing.T, img image.Image) []byte {
	t.Helper()

	var buffer bytes.Buffer
	if err := png.Encode(&buffer, img); err != nil {
		t.Fatalf("encode the PNG image: %v", err)
	}
	return buffer.Bytes()
}

// solidImage builds an opaque image of the given size.
func solidImage(width, height int, value color.NRGBA) *image.NRGBA {
	img := image.NewNRGBA(image.Rect(0, 0, width, height))
	for y := 0; y < height; y++ {
		for x := 0; x < width; x++ {
			img.SetNRGBA(x, y, value)
		}
	}
	return img
}

// TestEncodeICOProducesOneImage proves that the conversion of a single image
// produces an ICO file with exactly one directory entry that describes the
// size of the source.
func TestEncodeICOProducesOneImage(t *testing.T) {
	source := pngBytes(t, solidImage(64, 48, color.NRGBA{R: 10, G: 20, B: 30, A: 255}))

	data, err := EncodeICO(source)
	if err != nil {
		t.Fatalf("encode the icon: %v", err)
	}

	entries := parseICO(t, data)
	if len(entries) != 1 {
		t.Fatalf("the ICO file carries %d images, want one", len(entries))
	}
	entry := entries[0]
	if entry.width != 64 || entry.height != 48 {
		t.Errorf("the image is %dx%d, want 64x48", entry.width, entry.height)
	}
	if entry.bpp != 32 {
		t.Errorf("the image carries %d bits per pixel, want 32", entry.bpp)
	}
	if int(entry.offset)+int(entry.size) > len(data) {
		t.Errorf("the image payload ends at %d, past the %d bytes of the file", int(entry.offset)+int(entry.size), len(data))
	}
}

// TestEncodeICOKeepsTheSourcePixels proves that the pixels of the ICO image
// are the pixels of the source image: the conversion must not darken the
// transparent edges and must not swap the color channels.
func TestEncodeICOKeepsTheSourcePixels(t *testing.T) {
	source := image.NewNRGBA(image.Rect(0, 0, 2, 2))
	source.SetNRGBA(0, 0, color.NRGBA{R: 10, G: 20, B: 30, A: 255})
	source.SetNRGBA(1, 0, color.NRGBA{R: 40, G: 50, B: 60, A: 128})
	source.SetNRGBA(0, 1, color.NRGBA{R: 70, G: 80, B: 90, A: 0})
	source.SetNRGBA(1, 1, color.NRGBA{R: 100, G: 110, B: 120, A: 255})

	data, err := EncodeICO(pngBytes(t, source))
	if err != nil {
		t.Fatalf("encode the icon: %v", err)
	}

	entries := parseICO(t, data)
	entry := entries[0]
	payload := data[entry.offset : entry.offset+entry.size]

	// The payload starts with the BITMAPINFOHEADER, the pixels follow it.
	const headerSize = 40
	if got := binary.LittleEndian.Uint32(payload[0:4]); got != 40 {
		t.Fatalf("the DIB header size is %d, want 40", got)
	}
	if got := binary.LittleEndian.Uint32(payload[4:8]); got != 2 {
		t.Fatalf("the DIB width is %d, want 2", got)
	}
	if got := binary.LittleEndian.Uint32(payload[8:12]); got != 4 {
		t.Fatalf("the DIB height is %d, want 4 (doubled for ICO)", got)
	}

	// The pixels are stored bottom-up in the BGRA order.
	pixels := payload[headerSize:]
	rowSize := (2*4 + 3) & ^3
	want := [][][4]uint8{
		{{70, 80, 90, 0}, {100, 110, 120, 255}},
		{{10, 20, 30, 255}, {40, 50, 60, 128}},
	}
	for y := 0; y < 2; y++ {
		for x := 0; x < 2; x++ {
			off := y*rowSize + x*4
			got := [4]uint8{pixels[off+2], pixels[off+1], pixels[off], pixels[off+3]}
			if got != want[y][x] {
				t.Errorf("the pixel at (%d,%d) is %v, want %v", x, y, got, want[y][x])
			}
		}
	}
}

// TestEncodeICOWithSizesProducesOneImagePerSize proves that the multi size
// conversion produces one directory entry per requested size and that every
// entry carries the size it was asked for.
func TestEncodeICOWithSizesProducesOneImagePerSize(t *testing.T) {
	source := pngBytes(t, solidImage(180, 180, color.NRGBA{R: 200, G: 100, B: 50, A: 255}))

	sizes := []int{16, 32, 48}
	data, err := EncodeICOWithSizes(source, sizes)
	if err != nil {
		t.Fatalf("encode the icon: %v", err)
	}

	entries := parseICO(t, data)
	if len(entries) != len(sizes) {
		t.Fatalf("the ICO file carries %d images, want %d", len(entries), len(sizes))
	}
	for i, size := range sizes {
		if entries[i].width != size || entries[i].height != size {
			t.Errorf("the image %d is %dx%d, want %dx%d", i, entries[i].width, entries[i].height, size, size)
		}
	}
}

// TestEncodeICOWithSizesKeepsTheColor proves that the resizing keeps the
// color of a solid image: a resized icon must not shift its colors.
func TestEncodeICOWithSizesKeepsTheColor(t *testing.T) {
	source := pngBytes(t, solidImage(180, 180, color.NRGBA{R: 200, G: 100, B: 50, A: 255}))

	data, err := EncodeICOWithSizes(source, []int{16})
	if err != nil {
		t.Fatalf("encode the icon: %v", err)
	}

	entries := parseICO(t, data)
	entry := entries[0]
	payload := data[entry.offset : entry.offset+entry.size]

	const headerSize = 40
	pixels := payload[headerSize:]
	rowSize := (16*4 + 3) & ^3
	// The first row of the payload is the bottom row of the image.
	off := 15*rowSize + 8*4
	got := [4]uint8{pixels[off+2], pixels[off+1], pixels[off], pixels[off+3]}
	want := [4]uint8{200, 100, 50, 255}
	if got != want {
		t.Errorf("the resized pixel is %v, want %v", got, want)
	}
}

// TestEncodeICOWithSizesRejectsBadSizes proves that an unusable size is
// rejected instead of producing a broken icon.
func TestEncodeICOWithSizesRejectsBadSizes(t *testing.T) {
	source := pngBytes(t, solidImage(8, 8, color.NRGBA{A: 255}))

	if _, err := EncodeICOWithSizes(source, nil); err == nil {
		t.Error("the conversion without a size reported no error")
	}
	if _, err := EncodeICOWithSizes(source, []int{0}); err == nil {
		t.Error("the conversion with a size of zero reported no error")
	}
	if _, err := EncodeICOWithSizes(source, []int{257}); err == nil {
		t.Error("the conversion with a size above 256 reported no error")
	}
}

// TestEncodeICORejectsBrokenData proves that a broken PNG is reported
// instead of producing an empty icon.
func TestEncodeICORejectsBrokenData(t *testing.T) {
	if _, err := EncodeICO([]byte("not a PNG")); err == nil {
		t.Error("the conversion of broken data reported no error")
	}
	if _, err := EncodeICOWithSizes([]byte("not a PNG"), []int{16}); err == nil {
		t.Error("the conversion of broken data reported no error")
	}
}

// TestScaleToKeepsTheColorOfTransparentEdges proves that the resizing does
// not bleed the color of the transparent pixels into the visible ones.
//
// The image is a red pixel next to a fully transparent black pixel. A
// resizing that mixes the straight colors would darken the edge of the red
// pixel; a resizing that mixes the premultiplied colors keeps it red.
func TestScaleToKeepsTheColorOfTransparentEdges(t *testing.T) {
	source := image.NewNRGBA(image.Rect(0, 0, 2, 1))
	source.SetNRGBA(0, 0, color.NRGBA{R: 255, G: 0, B: 0, A: 255})
	source.SetNRGBA(1, 0, color.NRGBA{R: 0, G: 0, B: 0, A: 0})

	scaled := scaleTo(source, 4, 1)

	// The third pixel is the edge between the two source pixels.
	edge := scaled.NRGBAAt(1, 0)
	if edge.A == 0 {
		t.Fatal("the edge pixel is fully transparent")
	}
	if edge.R < 200 || edge.G > 40 || edge.B > 40 {
		t.Errorf("the edge pixel is %v, want a red tone", edge)
	}
}

// TestEncodeICOOfTheProductIconIsStable proves that the conversion of the
// embedded product icon does not change.
//
// The release pipeline feeds the result of the conversion to the resource
// compiler of the executable, so a change of the bytes changes the icon of
// the shipped binary.
func TestEncodeICOOfTheProductIconIsStable(t *testing.T) {
	source, err := fs.ReadFile(assets.StaticFS(), "apple-touch-icon-180x180.png")
	if err != nil {
		t.Fatalf("read the embedded product icon: %v", err)
	}

	data, err := EncodeICO(source)
	if err != nil {
		t.Fatalf("encode the product icon: %v", err)
	}

	const want = "a75571eb8dcd41b364309fff97bee242953dca7bbf3784a1943268e2909f3beb"
	sum := sha256.Sum256(data)
	if got := hex.EncodeToString(sum[:]); got != want {
		t.Errorf("the product icon hash is %s, want %s", got, want)
	}
}
