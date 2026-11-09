// Copyright 2025
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"fmt"
	"net/http"
)

// addressingProbeHeader is the request header the production downloader sends
// to confirm byte addressing. It lives here so a rename in production is
// repaired in one place instead of at every fixture that answers the probe.
const addressingProbeHeader = "X-HFDesk-Addressing-Probe"

// writeAddressingProbeResponse answers one addressing-probe request with the
// fixed partial-content response: Content-Range over totalSize, a one-byte
// Content-Length, status 206, and exactly firstByte as the body. Every test
// fixture routes its probe branch through this writer, so the wire behaviour
// stays identical across fixtures.
func writeAddressingProbeResponse(w http.ResponseWriter, totalSize int, firstByte []byte) {
	w.Header().Set("Content-Range", fmt.Sprintf("bytes 0-0/%d", totalSize))
	w.Header().Set("Content-Length", "1")
	w.WriteHeader(http.StatusPartialContent)
	_, _ = w.Write(firstByte)
}
