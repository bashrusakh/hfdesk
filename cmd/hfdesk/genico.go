//go:build ignore

package main

import (
	"os"

	"github.com/bashrusakh/hfdesk/internal/icon"
)

// main reads the 180×180 apple-touch-icon PNG, converts it to a BMP-format
// .ico file (BITMAPINFOHEADER + 32-bit BGRA pixels + zero AND mask), and
// writes it to cmd/hfdesk/hfdesk.ico for embedding into the Windows binary
// via rsrc.
//
// The conversion itself lives in internal/icon, so the desktop host builds
// the very same icon at runtime and the shipped icon and the runtime icon
// cannot drift apart.
func main() {
	src, err := os.ReadFile("internal/assets/static/apple-touch-icon-180x180.png")
	if err != nil {
		panic(err)
	}

	out, err := icon.EncodeICO(src)
	if err != nil {
		panic(err)
	}

	if err := os.WriteFile("cmd/hfdesk/hfdesk.ico", out, 0644); err != nil {
		panic(err)
	}
	println("Created cmd/hfdesk/hfdesk.ico (BMP format)")
}
