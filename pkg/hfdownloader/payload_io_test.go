// Copyright 2025
// SPDX-License-Identifier: Apache-2.0

package hfdownloader

import "context"

func withPayloadIOMeasurement(ctx context.Context, measurement *payloadIOMeasurement) context.Context {
	return context.WithValue(ctx, payloadIOKey{}, measurement)
}

func (m *payloadIOMeasurement) snapshot() (readBytes, writeBytes int64) {
	return m.readBytes.Load(), m.writeBytes.Load()
}
