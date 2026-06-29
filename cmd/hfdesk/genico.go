//go:build ignore

package main

import (
	"bytes"
	"encoding/binary"
	"image/png"
	"os"
)

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

	// Re-encode PNG data
	var pngBuf bytes.Buffer
	if err := png.Encode(&pngBuf, img); err != nil {
		panic(err)
	}
	pngData := pngBuf.Bytes()

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
	binary.LittleEndian.PutUint32(header[14:18], uint32(len(pngData))) // size
	binary.LittleEndian.PutUint32(header[18:22], 22)       // offset (6+16)

	out := append(header, pngData...)
	if err := os.WriteFile("cmd/hfdesk/hfdesk.ico", out, 0644); err != nil {
		panic(err)
	}
	println("Created cmd/hfdesk/hfdesk.ico")
}
