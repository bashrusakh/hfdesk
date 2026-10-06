// Copyright 2025
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

func TestWSHub_Broadcast(t *testing.T) {
	hub := NewWSHub()
	go hub.Run()

	// Give hub time to start
	time.Sleep(10 * time.Millisecond)

	// Test broadcast doesn't panic with no clients
	hub.Broadcast("test", map[string]string{"key": "value"})

	// Test BroadcastJob
	job := &Job{
		ID:     "test123",
		Repo:   "test/repo",
		Status: JobStatusRunning,
	}
	hub.BroadcastJob(job)

	// Test BroadcastEvent
	hub.BroadcastEvent(map[string]string{"event": "test"})
}

func TestWSHub_ClientCount(t *testing.T) {
	hub := NewWSHub()
	go hub.Run()

	time.Sleep(10 * time.Millisecond)

	count := hub.ClientCount()
	if count != 0 {
		t.Errorf("Expected 0 clients, got %d", count)
	}
}

// TestWSClient_WritePumpOneMessagePerFrame guards the frontend contract that
// every WebSocket text frame contains exactly one JSON message. The frontend
// calls JSON.parse(event.data) on each frame, so batching several messages into
// one newline-joined frame throws and drops the whole frame.
func TestWSClient_WritePumpOneMessagePerFrame(t *testing.T) {
	// A real connection keeps the gorilla message path faithful.
	upgrader := websocket.Upgrader{}
	// wpDone is closed when the server-side writePump returns so the test can
	// await that goroutine instead of leaving it parked on the 30s ping ticker.
	wpDone := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()
		client := &WSClient{conn: conn, send: make(chan []byte, 256)}

		// Queue three messages before starting writePump so the first receive
		// sees a non-empty send channel; the pre-fix batching loop would then
		// join them into a single frame. All three are independently valid JSON.
		envelope := WSMessage{Type: "job_update", Data: map[string]any{
			"id": "job-1", "status": "running", "progress": 10,
		}}
		for i := 0; i < 3; i++ {
			msg, err := json.Marshal(envelope)
			if err != nil {
				return
			}
			client.send <- msg
		}

		go func() {
			defer close(wpDone)
			client.writePump()
		}()

		// Block until the client disconnects so the handler returns and
		// httptest.Server.Close does not wait forever.
		for {
			if _, _, err := conn.ReadMessage(); err != nil {
				break
			}
		}
		// Closing the send channel wakes an idle writePump (its receive then
		// reports ok == false) so it returns at once instead of waiting for the
		// next 30s ping tick; await it so no goroutine outlives the test.
		close(client.send)
		<-wpDone
	}))
	defer srv.Close()

	url := "ws" + strings.TrimPrefix(srv.URL, "http")
	conn, _, err := websocket.DefaultDialer.Dial(url, nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()

	conn.SetReadDeadline(time.Now().Add(3 * time.Second))

	var frames int
	for frames < 3 {
		_, data, err := conn.ReadMessage()
		if err != nil {
			t.Fatalf("read frame %d: %v", frames, err)
		}
		if bytes.ContainsRune(data, '\n') {
			t.Fatalf("frame %d contains a newline-joined payload: %q", frames, data)
		}
		var got WSMessage
		if err := json.Unmarshal(data, &got); err != nil {
			t.Fatalf("frame %d is not independently valid JSON: %v (%q)", frames, err, data)
		}
		if got.Type != "job_update" {
			t.Fatalf("frame %d unexpected type %q", frames, got.Type)
		}
		frames++
	}

	if frames != 3 {
		t.Fatalf("expected exactly 3 frames, got %d", frames)
	}

	// Disconnect so the handler's read loop breaks; that closes the send
	// channel and lets writePump return. Await it before returning so the
	// goroutine cannot survive past the test.
	conn.Close()
	<-wpDone
}
