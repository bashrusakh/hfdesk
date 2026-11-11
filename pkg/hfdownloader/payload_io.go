// Copyright 2025
// SPDX-License-Identifier: Apache-2.0

package hfdownloader

import (
	"context"
	"io"
	"sync/atomic"
)

type payloadIOKey struct{}

// payloadIOMeasurement is private, opt-in instrumentation used by the local
// performance benchmark. Context propagation scopes counts to one Download.
type payloadIOMeasurement struct {
	readBytes  atomic.Int64
	writeBytes atomic.Int64
}

func payloadIOMeasurementFromContext(ctx context.Context) *payloadIOMeasurement {
	measurement, _ := ctx.Value(payloadIOKey{}).(*payloadIOMeasurement)
	return measurement
}

type measuredPayloadReader struct {
	r io.Reader
	m *payloadIOMeasurement
}

func (r measuredPayloadReader) Read(p []byte) (int, error) {
	n, err := r.r.Read(p)
	if n > 0 {
		r.m.readBytes.Add(int64(n))
	}
	return n, err
}

type measuredPayloadWriter struct {
	w io.Writer
	m *payloadIOMeasurement
}

func (w measuredPayloadWriter) Write(p []byte) (int, error) {
	n, err := w.w.Write(p)
	if n > 0 {
		w.m.writeBytes.Add(int64(n))
	}
	return n, err
}
