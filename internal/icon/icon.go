// Copyright 2026
// SPDX-License-Identifier: Apache-2.0

// Package icon converts the product icon into the Windows icon format.
//
// The desktop host needs the product icon at runtime: the tray icon and the
// window icon are built from the embedded PNG, so the binary does not have to
// ship a second icon file. The release pipeline uses the very same
// conversion to build the icon that is embedded into the executable as a
// resource, so the icon of the tray, of the window and of the task bar is
// always the icon of the product.
//
// The ICO files carry the images in the BMP format (BITMAPINFOHEADER plus
// 32 bit BGRA pixels plus a zero AND mask), which is the format the resource
// compiler of the release pipeline expects.
package icon

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"image"
	"image/draw"
	"image/png"
	"math"
)

// EncodeICO converts a PNG image into the bytes of a Windows .ico file.
//
// The result carries one image at the native size of the source. It is the
// conversion the release pipeline uses to build the embedded resource, so the
// generated file stays byte for byte the same.
func EncodeICO(pngData []byte) ([]byte, error) {
	img, err := png.Decode(bytes.NewReader(pngData))
	if err != nil {
		return nil, fmt.Errorf("icon: decode the PNG image: %w", err)
	}
	return encodeSingle(img)
}

// EncodeICOWithSizes converts a PNG image into the bytes of a Windows .ico
// file that carries one image per requested size.
//
// The Windows shell picks the image that matches the size it renders, so an
// icon with several sizes stays sharp in the notification area, in the title
// bar and in the task bar. Every size must be positive and at most 256.
func EncodeICOWithSizes(pngData []byte, sizes []int) ([]byte, error) {
	if len(sizes) == 0 {
		return nil, errors.New("icon: at least one size is required")
	}
	img, err := png.Decode(bytes.NewReader(pngData))
	if err != nil {
		return nil, fmt.Errorf("icon: decode the PNG image: %w", err)
	}

	images := make([]iconImage, 0, len(sizes))
	for _, size := range sizes {
		if size <= 0 || size > 256 {
			return nil, fmt.Errorf("icon: invalid size %d: the size must be between 1 and 256", size)
		}
		images = append(images, buildImage(scaleTo(img, size, size)))
	}
	return assemble(images), nil
}

// iconImage is one image of an ICO file: its size and its BMP payload.
type iconImage struct {
	// width is the width of the image in pixels.
	width int
	// height is the height of the image in pixels.
	height int
	// data is the BMP payload of the image.
	data []byte
}

// encodeSingle builds the ICO file of one image at its native size.
func encodeSingle(img image.Image) ([]byte, error) {
	nrgba := toNRGBA(img)
	bounds := nrgba.Bounds()
	if bounds.Dx() <= 0 || bounds.Dy() <= 0 {
		return nil, errors.New("icon: the image is empty")
	}
	return assemble([]iconImage{buildImage(nrgba)}), nil
}

// toNRGBA converts an image into straight alpha NRGBA.
//
// NewRGBA would premultiply the color values and darken the transparent
// edges of the icon, so the conversion goes through NRGBA.
func toNRGBA(img image.Image) *image.NRGBA {
	bounds := img.Bounds()
	nrgba := image.NewNRGBA(image.Rect(0, 0, bounds.Dx(), bounds.Dy()))
	draw.Draw(nrgba, nrgba.Bounds(), img, bounds.Min, draw.Src)
	return nrgba
}

// buildImage builds the BMP payload of one ICO image.
//
// The payload is a BITMAPINFOHEADER, the 32 bit BGRA pixels in bottom-up
// order and a zero AND mask, which is the format an ICO file expects for a
// 32 bit image.
func buildImage(nrgba *image.NRGBA) iconImage {
	bounds := nrgba.Bounds()
	w, h := bounds.Dx(), bounds.Dy()

	// The pixel data of a BMP row is padded to a multiple of four bytes.
	rowSize := (w*4 + 3) & ^3
	bmpPixels := make([]byte, rowSize*h)
	for y := 0; y < h; y++ {
		dstRow := (h - 1 - y) * rowSize // bottom-up
		for x := 0; x < w; x++ {
			off := nrgba.PixOffset(bounds.Min.X+x, bounds.Min.Y+y)
			r, g, b, a := nrgba.Pix[off], nrgba.Pix[off+1], nrgba.Pix[off+2], nrgba.Pix[off+3]
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
	binary.LittleEndian.PutUint32(bmpHeader[4:8], uint32(w))    // width
	binary.LittleEndian.PutUint32(bmpHeader[8:12], uint32(h*2)) // height (doubled for ICO)
	binary.LittleEndian.PutUint16(bmpHeader[12:14], 1)          // planes
	binary.LittleEndian.PutUint16(bmpHeader[14:16], 32)         // bpp
	binary.LittleEndian.PutUint32(bmpHeader[16:20], 0)          // compression (BI_RGB)
	binary.LittleEndian.PutUint32(bmpHeader[20:24], 0)          // image size (0 for BI_RGB)
	binary.LittleEndian.PutUint32(bmpHeader[24:28], 0)          // x pixels per meter
	binary.LittleEndian.PutUint32(bmpHeader[28:32], 0)          // y pixels per meter
	binary.LittleEndian.PutUint32(bmpHeader[32:36], 0)          // colors used
	binary.LittleEndian.PutUint32(bmpHeader[36:40], 0)          // important colors

	// ICO data: AND mask (1 bit per pixel, row-aligned to 4 bytes) — all
	// zeros for 32bpp
	andRowSize := (w + 31) / 32 * 4
	andMask := make([]byte, andRowSize*h)

	data := append(bmpHeader, bmpPixels...)
	data = append(data, andMask...)

	return iconImage{width: w, height: h, data: data}
}

// assemble builds the ICO file of the given images.
//
// The file starts with the ICO header and one directory entry per image,
// followed by the payloads of the images.
func assemble(images []iconImage) []byte {
	// ICO header: reserved(2) + type(2) + count(2), then one 16 byte
	// directory entry per image.
	header := make([]byte, 6+16*len(images))
	binary.LittleEndian.PutUint16(header[0:2], 0)                   // reserved
	binary.LittleEndian.PutUint16(header[2:4], 1)                   // type: ICO
	binary.LittleEndian.PutUint16(header[4:6], uint16(len(images))) // count

	offset := 6 + 16*len(images)
	for i, img := range images {
		entry := header[6+16*i : 6+16*(i+1)]
		// A dimension of 256 is stored as zero.
		entry[0] = dimensionByte(img.width)
		entry[1] = dimensionByte(img.height)
		entry[2] = 0                                                      // colors
		entry[3] = 0                                                      // reserved
		binary.LittleEndian.PutUint16(entry[4:6], 1)                      // planes
		binary.LittleEndian.PutUint16(entry[6:8], 32)                     // bpp
		binary.LittleEndian.PutUint32(entry[8:12], uint32(len(img.data))) // size
		binary.LittleEndian.PutUint32(entry[12:16], uint32(offset))       // offset
		offset += len(img.data)
	}

	out := header
	for _, img := range images {
		out = append(out, img.data...)
	}
	return out
}

// dimensionByte encodes the dimension of an ICO directory entry. A dimension
// of 256 pixels is stored as zero.
func dimensionByte(value int) byte {
	if value >= 256 {
		return 0
	}
	return byte(value)
}

// scaleTo resizes an image to the given size with a bilinear interpolation.
//
// The interpolation runs on the premultiplied colors, so the transparent
// edges of the icon keep their color instead of bleeding the color of the
// transparent pixels into the visible ones.
func scaleTo(src image.Image, width, height int) *image.NRGBA {
	nrgba := toNRGBA(src)
	bounds := nrgba.Bounds()
	srcWidth, srcHeight := bounds.Dx(), bounds.Dy()

	dst := image.NewNRGBA(image.Rect(0, 0, width, height))
	if srcWidth <= 0 || srcHeight <= 0 {
		return dst
	}

	for y := 0; y < height; y++ {
		sourceY := (float64(y)+0.5)*float64(srcHeight)/float64(height) - 0.5
		y0 := int(math.Floor(sourceY))
		weightY := sourceY - float64(y0)
		row0 := clampInt(y0, 0, srcHeight-1)
		row1 := clampInt(y0+1, 0, srcHeight-1)

		for x := 0; x < width; x++ {
			sourceX := (float64(x)+0.5)*float64(srcWidth)/float64(width) - 0.5
			x0 := int(math.Floor(sourceX))
			weightX := sourceX - float64(x0)
			col0 := clampInt(x0, 0, srcWidth-1)
			col1 := clampInt(x0+1, 0, srcWidth-1)

			top := blend(
				blend(premultiply(pixelAt(nrgba, col0, row0)), premultiply(pixelAt(nrgba, col1, row0)), weightX),
				blend(premultiply(pixelAt(nrgba, col0, row1)), premultiply(pixelAt(nrgba, col1, row1)), weightX),
				weightY,
			)

			color := unpremultiply(top)
			off := dst.PixOffset(x, y)
			dst.Pix[off] = color[0]
			dst.Pix[off+1] = color[1]
			dst.Pix[off+2] = color[2]
			dst.Pix[off+3] = color[3]
		}
	}
	return dst
}

// premultiply multiplies the color channels with the alpha channel.
//
// The premultiplied colors are the ones that can be interpolated without
// bleeding the color of the transparent pixels into the visible ones.
func premultiply(color [4]uint8) [4]uint8 {
	alpha := float64(color[3]) / 255
	return [4]uint8{
		uint8(float64(color[0])*alpha + 0.5),
		uint8(float64(color[1])*alpha + 0.5),
		uint8(float64(color[2])*alpha + 0.5),
		color[3],
	}
}

// unpremultiply divides the color channels by the alpha channel.
func unpremultiply(color [4]uint8) [4]uint8 {
	if color[3] == 0 {
		return [4]uint8{0, 0, 0, 0}
	}
	scale := 255 / float64(color[3])
	return [4]uint8{
		uint8(clampFloat(float64(color[0])*scale+0.5, 0, 255)),
		uint8(clampFloat(float64(color[1])*scale+0.5, 0, 255)),
		uint8(clampFloat(float64(color[2])*scale+0.5, 0, 255)),
		color[3],
	}
}

// pixelAt returns the NRGBA pixel at the given position.
func pixelAt(img *image.NRGBA, x, y int) [4]uint8 {
	off := img.PixOffset(img.Bounds().Min.X+x, img.Bounds().Min.Y+y)
	return [4]uint8{img.Pix[off], img.Pix[off+1], img.Pix[off+2], img.Pix[off+3]}
}

// blend mixes two colors. A weight of zero returns the first color, a weight
// of one the second one.
func blend(a, b [4]uint8, weight float64) [4]uint8 {
	var out [4]uint8
	for i := range out {
		value := float64(a[i])*(1-weight) + float64(b[i])*weight
		out[i] = uint8(clampFloat(value+0.5, 0, 255))
	}
	return out
}

// clampFloat limits a value to the range [low, high].
func clampFloat(value, low, high float64) float64 {
	if value < low {
		return low
	}
	if value > high {
		return high
	}
	return value
}

// clampInt limits a value to the range [low, high].
func clampInt(value, low, high int) int {
	if value < low {
		return low
	}
	if value > high {
		return high
	}
	return value
}
