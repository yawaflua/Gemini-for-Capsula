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

// Package echo implements a loopback [provider.Provider] that plays the
// user's utterance back. It is meant for testing the device and the network
// path without spending model quota.
package echo

import (
	"context"
	"sync"

	"github.com/yawaflua/marusya-gemini/internal/models"
	"github.com/yawaflua/marusya-gemini/internal/provider"
)

// Provider is a loopback provider.
type Provider struct{}

var _ provider.Provider = Provider{}

// Name implements [provider.Provider].
func (Provider) Name() string { return "echo" }

// OutputFormat implements [provider.Provider]. Audio is returned unchanged.
func (Provider) OutputFormat() models.AudioFormat { return models.DeviceInputFormat }

// Connect implements [provider.Provider].
func (Provider) Connect(_ context.Context, opts provider.SessionOptions) (provider.Session, error) {
	return &session{events: make(chan provider.Event, 1024)}, nil
}

type session struct {
	mu     sync.Mutex
	buf    []byte
	events chan provider.Event
	closed bool
}

func (s *session) StartTurn() error {
	s.mu.Lock()
	s.buf = s.buf[:0]
	s.mu.Unlock()
	return nil
}

func (s *session) SendAudio(pcm []byte) error {
	s.mu.Lock()
	s.buf = append(s.buf, pcm...)
	s.mu.Unlock()
	return nil
}

func (s *session) EndTurn() error {
	s.mu.Lock()
	audio := append([]byte(nil), s.buf...)
	s.mu.Unlock()
	go func() {
		const chunk = 8192
		for len(audio) > 0 {
			n := min(chunk, len(audio))
			if !s.emit(provider.Event{Kind: provider.EventAudio, Audio: audio[:n]}) {
				return
			}
			audio = audio[n:]
		}
		s.emit(provider.Event{Kind: provider.EventTurnComplete})
	}()
	return nil
}

// emit delivers e without blocking; events are dropped if the consumer is
// too slow.
func (s *session) emit(e provider.Event) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return false
	}
	select {
	case s.events <- e:
	default:
	}
	return true
}

func (s *session) SendToolResults([]provider.ToolResult) error { return nil }
func (s *session) Events() <-chan provider.Event               { return s.events }
func (s *session) Err() error                                  { return nil }

func (s *session) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.closed {
		s.closed = true
		close(s.events)
	}
	return nil
}
