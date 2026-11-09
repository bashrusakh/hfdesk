// Copyright 2026
// SPDX-License-Identifier: Apache-2.0

package desktop

import (
	"encoding/binary"
	"testing"
)

// TestProductIconBytesBuildsTheIconFromTheEmbeddedAsset proves that the
// runtime icon is built from the embedded product icon, so the desktop host
// does not need a second icon file.
func TestProductIconBytesBuildsTheIconFromTheEmbeddedAsset(t *testing.T) {
	data, err := productIconBytes()
	if err != nil {
		t.Fatalf("build the product icon: %v", err)
	}
	if len(data) < 6 {
		t.Fatalf("the icon is %d bytes long, want at least 6", len(data))
	}
	if kind := binary.LittleEndian.Uint16(data[2:4]); kind != 1 {
		t.Errorf("the icon type is %d, want 1 (ICO)", kind)
	}

	count := int(binary.LittleEndian.Uint16(data[4:6]))
	if count != len(productIconSizes) {
		t.Errorf("the icon carries %d images, want %d", count, len(productIconSizes))
	}

	for i, size := range productIconSizes {
		entry := data[6+16*i : 6+16*(i+1)]
		width := int(entry[0])
		if width == 0 {
			width = 256
		}
		if width != size {
			t.Errorf("the image %d is %d pixels wide, want %d", i, width, size)
		}
	}
}
