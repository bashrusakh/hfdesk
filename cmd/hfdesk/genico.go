//go:build ignore

package main

import (
	"bytes"
	"encoding/binary"
	"image"
	"image/draw"
	"image/png"
	"os"
)

// main reads the 180×180 apple-touch-icon PNG, converts it to a BMP-format
// .ico file (BITMAPINFOHEADER + 32-bit BGRA pixels + zero AND mask), and
// writes it to cmd/hfdesk/hfdesk.ico for embedding into the Windows binary
// via rsrc.
func main() {
	src, err := os.ReadFile("internal/assets/static/apple-touch-icon-180x180.png")
	if err != nil {
		panic(err)
	}

	img, err := png.Decode(bytes.NewReader(src))
	if err != nil {
		panic(err)
	}

	bounds := img.Bounds()
	w, h := bounds.Dx(), bounds.Dy()

	// Convert to RGBA (straight alpha, not premultiplied)
	rgba := image.NewRGBA(bounds)
	draw.Draw(rgba, bounds, img, bounds.Min, draw.Src)

	// Build BMP pixel data (BGRA, bottom-up, no compression)
	// Each row must be 4-byte aligned
	rowSize := (w*4 + 3) & ^3
	bmpPixels := make([]byte, rowSize*h)
	for y := 0; y < h; y++ {
		dstRow := (h - 1 - y) * rowSize // bottom-up
		for x := 0; x < w; x++ {
			off := rgba.PixOffset(x, y)
			r, g, b, a := rgba.Pix[off], rgba.Pix[off+1], rgba.Pix[off+2], rgba.Pix[off+3]
			// BGRA order
			bmpPixels[dstRow+x*4] = b
			bmpPixels[dstRow+x*4+1] = g
			bmpPixels[dstRow+x*4+2] = r
			bmpPixels[dstRow+x*4+3] = a
		}
	}

	// BITMAPINFOHEADER (40 bytes)
	bmpHeader := make([]byte, 40)
	binary.LittleEndian.PutUint32(bmpHeader[0:4], 40)           // header size
	binary.LittleEndian.PutUint32(bmpHeader[4:8], uint32(w))     // width
	binary.LittleEndian.PutUint32(bmpHeader[8:12], uint32(h*2))  // height (doubled for ICO)
	binary.LittleEndian.PutUint16(bmpHeader[12:14], 1)          // planes
	binary.LittleEndian.PutUint16(bmpHeader[14:16], 32)         // bpp
	binary.LittleEndian.PutUint32(bmpHeader[16:20], 0)           // compression (BI_RGB)
	binary.LittleEndian.PutUint32(bmpHeader[20:24], 0)                       // image size (0 for BI_RGB)
	binary.LittleEndian.PutUint32(bmpHeader[24:28], 0)           // x pixels per meter
	binary.LittleEndian.PutUint32(bmpHeader[28:32], 0)           // y pixels per meter
	binary.LittleEndian.PutUint32(bmpHeader[32:36], 0)           // colors used
	binary.LittleEndian.PutUint32(bmpHeader[36:40], 0)           // important colors

	// ICO data: AND mask (1 bit per pixel, row-aligned to 4 bytes) — all zeros for 32bpp
	andRowSize := (w + 31) / 32 * 4
	andMask := make([]byte, andRowSize*h)

	// Assemble ICO
	icoData := append(bmpHeader, bmpPixels...)
	icoData = append(icoData, andMask...)

	// ICO header: reserved(2) + type(2) + count(2)
	// Entry: w(1) + h(1) + colors(1) + reserved(1) + planes(2) + bpp(2) + size(4) + offset(4)
	header := make([]byte, 6+16)
	binary.LittleEndian.PutUint16(header[0:2], 0)          // reserved
	binary.LittleEndian.PutUint16(header[2:4], 1)          // type: ICO
	binary.LittleEndian.PutUint16(header[4:6], 1)          // count: 1

	entryW := byte(w)
	entryH := byte(h)
	if w >= 256 {
		entryW = 0
	}
	if h >= 256 {
		entryH = 0
	}
	header[6] = entryW
	header[7] = entryH
	header[8] = 0  // colors
	header[9] = 0  // reserved
	binary.LittleEndian.PutUint16(header[10:12], 1)        // planes
	binary.LittleEndian.PutUint16(header[12:14], 32)       // bpp
	binary.LittleEndian.PutUint32(header[14:18], uint32(len(icoData))) // size
	binary.LittleEndian.PutUint32(header[18:22], 22)       // offset (6+16)

	out := append(header, icoData...)
	if err := os.WriteFile("cmd/hfdesk/hfdesk.ico", out, 0644); err != nil {
		panic(err)
	}
	println("Created cmd/hfdesk/hfdesk.ico (BMP format)")
}
