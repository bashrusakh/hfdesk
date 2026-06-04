// Copyright 2025
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"encoding/json"
	"net/http"
)

func writeJSON(w http.ResponseWriter, status int, data any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(data)
}

func readJSON(r *http.Request, dst any) error {
	return json.NewDecoder(r.Body).Decode(dst)
}

func writeError(w http.ResponseWriter, status int, message, details string) {
	writeAPIError(w, status, errorCodeFromStatus(status), message, details)
}

func writeAPIError(w http.ResponseWriter, status int, code, message, details string) {
	writeJSON(w, status, ErrorResponse{
		OK:      false,
		Error:   message,
		Details: details,
		ErrorDetail: &APIErrorDetail{
			Code:    code,
			Message: message,
			Details: details,
		},
	})
}

func errorCodeFromStatus(status int) string {
	switch status {
	case http.StatusBadRequest:
		return "bad_request"
	case http.StatusUnauthorized:
		return "unauthorized"
	case http.StatusForbidden:
		return "forbidden"
	case http.StatusNotFound:
		return "not_found"
	case http.StatusConflict:
		return "conflict"
	case http.StatusBadGateway:
		return "bad_gateway"
	default:
		if status >= 500 {
			return "server_error"
		}
		return "api_error"
	}
}
