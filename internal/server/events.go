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

package server

import (
	"slices"
	"sync"
	"time"
)

// EventKind classifies an [Event].
type EventKind string

const (
	EventConnect    EventKind = "connect"
	EventDisconnect EventKind = "disconnect"
	EventTurn       EventKind = "turn"
	EventSkill      EventKind = "skill"
	EventCommand    EventKind = "command"
	EventError      EventKind = "error"
	EventSettings   EventKind = "settings"
	EventWake       EventKind = "wake"
	// EventAsk is an announcement of the ask API: Model is what was said,
	// User the answer.
	EventAsk EventKind = "ask"
)

// Source tells who started a turn or a command.
const (
	SourceVoice = "voice"
	SourcePanel = "panel"
)

// Event is an entry of the activity history shown in the web panel.
type Event struct {
	ID      int64     `json:"id"`
	Time    time.Time `json:"time"`
	Kind    EventKind `json:"kind"`
	Session string    `json:"session,omitempty"`
	Device  string    `json:"device,omitempty"`
	Source  string    `json:"source,omitempty"`
	// User and Model are the transcripts of a turn.
	User      string `json:"user,omitempty"`
	Model     string `json:"model,omitempty"`
	Cancelled bool   `json:"cancelled,omitempty"`
	// Name, Args, Result and Error describe a skill or command.
	Name      string         `json:"name,omitempty"`
	Args      map[string]any `json:"args,omitempty"`
	Result    map[string]any `json:"result,omitempty"`
	Error     string         `json:"error,omitempty"`
	LatencyMS int64          `json:"latency_ms,omitempty"`
}

// historySize is the number of events kept in memory.
const historySize = 500

// subscriber receives live updates. Updates are dropped for a subscriber
// that does not keep up; the panel reloads the state when it reconnects.
type subscriber struct {
	events  chan Event
	devices chan struct{} // the device list changed (coalesced)
}

// hub keeps the recent history and fans out updates.
type hub struct {
	mu   sync.Mutex
	buf  []Event
	next int64
	subs map[*subscriber]struct{}
}

func newHub() *hub { return &hub{subs: make(map[*subscriber]struct{})} }

func (h *hub) publish(e Event) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.next++
	e.ID = h.next
	if e.Time.IsZero() {
		e.Time = time.Now()
	}
	if len(h.buf) == historySize {
		h.buf = slices.Delete(h.buf, 0, 1)
	}
	h.buf = append(h.buf, e)
	for s := range h.subs {
		select {
		case s.events <- e:
		default:
		}
	}
}

// devicesChanged notifies subscribers that the device list or a device's
// state changed.
func (h *hub) devicesChanged() {
	h.mu.Lock()
	defer h.mu.Unlock()
	for s := range h.subs {
		select {
		case s.devices <- struct{}{}:
		default:
		}
	}
}

// history returns the stored events, oldest first.
func (h *hub) history() []Event {
	h.mu.Lock()
	defer h.mu.Unlock()
	// Never nil: the panel expects a JSON array, also before the first event.
	return append([]Event{}, h.buf...)
}

func (h *hub) subscribe() *subscriber {
	s := &subscriber{events: make(chan Event, 64), devices: make(chan struct{}, 1)}
	h.mu.Lock()
	h.subs[s] = struct{}{}
	h.mu.Unlock()
	return s
}

func (h *hub) unsubscribe(s *subscriber) {
	h.mu.Lock()
	delete(h.subs, s)
	h.mu.Unlock()
}
