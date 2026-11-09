// Copyright 2026
// SPDX-License-Identifier: Apache-2.0

package desktop

import (
	"fmt"
	"io/fs"

	"github.com/bashrusakh/hfdesk/internal/assets"
	"github.com/bashrusakh/hfdesk/internal/icon"
)

// productIconName is the name of the product icon in the embedded assets.
const productIconName = "apple-touch-icon-180x180.png"

// productIconSizes are the sizes the runtime icon carries.
//
// The Windows shell picks the image that matches the size it renders, so the
// icon stays sharp in the notification area, in the title bar of the runtime
// window and in the task bar.
var productIconSizes = []int{16, 24, 32, 48, 64, 128, 180}

// productIconBytes returns the product icon in the Windows ICO format.
//
// The icon is built from the embedded PNG, so the desktop host does not ship
// a second icon file and the runtime icon is always the icon of the product.
func productIconBytes() ([]byte, error) {
	source, err := fs.ReadFile(assets.StaticFS(), productIconName)
	if err != nil {
		return nil, fmt.Errorf("desktop: read the embedded product icon: %w", err)
	}
	return icon.EncodeICOWithSizes(source, productIconSizes)
}
