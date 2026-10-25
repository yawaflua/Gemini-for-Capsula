// Copyright 2026 The Marusya-Gemini Authors.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     https://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

// Package transport implements the device <-> server stream on top of
// WebSocket. See package models for the protocol description.
package transport

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/coder/websocket"

	"github.com/yawaflua/marusya-gemini/internal/models"
)

// Path is the HTTP path of the streaming endpoint.
const Path = "/v1/stream"

// maxMessageSize bounds a single frame. Audio frames are small (~20-100 ms),
// but the model may send larger chunks.
const maxMessageSize = 4 << 20

// Frame is a single inbound frame: either a control message or audio.
type Frame struct {
	Message *models.Message
	Audio   []byte
}

// Conn is a bidirectional message/audio stream.
//
// Write methods are safe for concurrent use. Read must be called from a
// single goroutine.
type Conn struct {
	ws *websocket.Conn
}

// Dial connects to the server at url, authenticating with token if non-empty.
func Dial(ctx context.Context, url, token string) (*Conn, error) {
	h := http.Header{}
	if token != "" {
		h.Set("Authorization", "Bearer "+token)
	}
	ws, _, err := websocket.Dial(ctx, url, &websocket.DialOptions{HTTPHeader: h})
	if err != nil {
		return nil, fmt.Errorf("transport: dial %s: %w", url, err)
	}
	ws.SetReadLimit(maxMessageSize)
	return &Conn{ws: ws}, nil
}

// Accept upgrades an HTTP request to a stream.
func Accept(w http.ResponseWriter, r *http.Request) (*Conn, error) {
	ws, err := websocket.Accept(w, r, nil)
	if err != nil {
		return nil, fmt.Errorf("transport: accept: %w", err)
	}
	ws.SetReadLimit(maxMessageSize)
	return &Conn{ws: ws}, nil
}

// Send writes a control message.
func (c *Conn) Send(ctx context.Context, m *models.Message) error {
	b, err := json.Marshal(m)
	if err != nil {
		return fmt.Errorf("transport: marshal %s: %w", m.Type, err)
	}
	return c.ws.Write(ctx, websocket.MessageText, b)
}

// SendAudio writes a chunk of PCM audio.
func (c *Conn) SendAudio(ctx context.Context, pcm []byte) error {
	return c.ws.Write(ctx, websocket.MessageBinary, pcm)
}

// Read blocks until the next frame arrives.
func (c *Conn) Read(ctx context.Context) (Frame, error) {
	typ, b, err := c.ws.Read(ctx)
	if err != nil {
		return Frame{}, err
	}
	if typ == websocket.MessageBinary {
		return Frame{Audio: b}, nil
	}
	m := new(models.Message)
	if err := json.Unmarshal(b, m); err != nil {
		return Frame{}, fmt.Errorf("transport: decode message: %w", err)
	}
	if err := m.Validate(); err != nil {
		return Frame{}, fmt.Errorf("transport: %w", err)
	}
	return Frame{Message: m}, nil
}

// Ping checks that the peer is alive.
func (c *Conn) Ping(ctx context.Context) error { return c.ws.Ping(ctx) }

// KeepAlive pings the peer every interval until ctx is done or a ping fails.
// A failed ping closes the connection, unblocking pending reads.
func (c *Conn) KeepAlive(ctx context.Context, interval time.Duration) {
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			pctx, cancel := context.WithTimeout(ctx, interval)
			err := c.ws.Ping(pctx)
			cancel()
			if err != nil {
				c.ws.Close(websocket.StatusGoingAway, "ping timeout")
				return
			}
		}
	}
}

// IsNormalClosure reports whether err is the result of the peer closing the
// stream gracefully.
func IsNormalClosure(err error) bool {
	s := websocket.CloseStatus(err)
	return s == websocket.StatusNormalClosure || s == websocket.StatusGoingAway
}

// Close closes the stream with a normal status.
func (c *Conn) Close(reason string) error {
	return c.ws.Close(websocket.StatusNormalClosure, reason)
}
