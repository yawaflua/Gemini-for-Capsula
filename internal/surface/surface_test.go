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

package surface

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/yawaflua/marusya-gemini/internal/indicator"
	"github.com/yawaflua/marusya-gemini/internal/models"
)

type fakeRing struct {
	mu    sync.Mutex
	scene string
}

func (r *fakeRing) Override(req models.RingRequest) (time.Time, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.scene = req.Scene
	return time.Now().Add(req.Hold), nil
}

func (r *fakeRing) ResetOverride() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.scene = ""
}

type fakeFace struct {
	mu     sync.Mutex
	mode   string
	lux    []float64
	asleep bool
}

func (f *fakeFace) Override(req models.DisplayRequest) (time.Time, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.mode = req.Mode
	return time.Now().Add(req.Hold), nil
}

func (f *fakeFace) ResetOverride() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.mode = ""
}

func (f *fakeFace) SetAmbient(lux float64) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.lux = append(f.lux, lux)
	f.asleep = lux < 1
}

func (f *fakeFace) Asleep() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.asleep
}

type fakeLight struct {
	mu  sync.Mutex
	lux float64
	err error
}

func (l *fakeLight) set(v float64) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.lux = v
}

func (l *fakeLight) Lux() (float64, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.lux, l.err
}

// next returns the next event of the given kind, skipping others.
func next(t *testing.T, ch <-chan Event, kind string) Event {
	t.Helper()
	timeout := time.After(3 * time.Second)
	for {
		select {
		case ev := <-ch:
			if ev.Kind == kind {
				return ev
			}
		case <-timeout:
			t.Fatalf("no %s event", kind)
		}
	}
}

func TestOverrides(t *testing.T) {
	ctx := context.Background()
	ring, face := &fakeRing{}, &fakeFace{}
	s := &Surface{Ring: ring, Face: face}
	events, cancel := s.Subscribe()
	defer cancel()

	if _, err := s.SetRing(ctx, models.RingRequest{Scene: "disco"}); err == nil {
		t.Error("unknown scene must be rejected")
	}
	o, err := s.SetRing(ctx, models.RingRequest{Scene: "Breathe", Color: "#FF0000", Hold: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	if o.Scene != "breathe" || o.Color != "ff0000" || ring.scene != "breathe" {
		t.Errorf("override = %+v, ring scene %q", o, ring.scene)
	}
	if ev := next(t, events, EventRing); ev.State.Ring == nil || ev.State.Ring.Scene != "breathe" {
		t.Errorf("ring event = %+v", ev.State.Ring)
	}
	// It expires on its own.
	if ev := next(t, events, EventRing); ev.State.Ring != nil {
		t.Errorf("after expiry ring = %+v, want nil", ev.State.Ring)
	}

	if _, err := s.ShowDisplay(ctx, models.DisplayRequest{Text: "12:30"}); err == nil {
		t.Error("5 characters must be rejected")
	}
	if _, err := s.ShowDisplay(ctx, models.DisplayRequest{Mode: "clock"}); err != nil {
		t.Fatal(err)
	}
	st, _ := s.Surface(ctx)
	if st.Display == nil || st.Display.Mode != "clock" || face.mode != "clock" {
		t.Errorf("display override = %+v", st.Display)
	}
	if d := time.Until(st.Display.Until); d < models.DefaultOverride-time.Minute {
		t.Errorf("default hold = %s, want %s", d, models.DefaultOverride)
	}
	s.ResetDisplay(ctx)
	if st, _ := s.Surface(ctx); st.Display != nil || face.mode != "" {
		t.Errorf("after reset display = %+v, face mode %q", st.Display, face.mode)
	}

	if err := (&Surface{}).ResetRing(ctx); !errors.Is(err, ErrNoHardware) {
		t.Errorf("no ring: %v", err)
	}
}

func TestStateKeysAndLight(t *testing.T) {
	old := ErrorHold
	ErrorHold = 50 * time.Millisecond
	defer func() { ErrorHold = old }()

	face, light := &fakeFace{}, &fakeLight{lux: 100}
	s := &Surface{Face: face, Light: light, LightInterval: 10 * time.Millisecond}
	events, cancel := s.Subscribe()
	defer cancel()
	ctx, stop := context.WithCancel(context.Background())
	defer stop()
	go s.Run(ctx)

	if ev := next(t, events, EventLux); *ev.State.Lux != 100 {
		t.Errorf("lux = %v", *ev.State.Lux)
	}
	light.set(105) // within a tenth: no event
	time.Sleep(50 * time.Millisecond)
	light.set(0.2)
	if ev := next(t, events, EventLux); *ev.State.Lux != 0.2 {
		t.Errorf("lux = %v, want 0.2 (105 must have been skipped)", *ev.State.Lux)
	}
	if ev := next(t, events, EventAsleep); !ev.State.Asleep {
		t.Error("the face should be asleep in the dark")
	}

	s.Show(indicator.Listening)
	if ev := next(t, events, EventState); ev.State.State != "listening" {
		t.Errorf("state = %s", ev.State.State)
	}
	s.Show(indicator.Error)
	next(t, events, EventState)
	if ev := next(t, events, EventState); ev.State.State != "idle" {
		t.Errorf("error should turn into idle, got %s", ev.State.State)
	}

	s.KeyEvent("volume_up", 115, true)
	if ev := next(t, events, EventKey); ev.Key != (Key{"volume_up", 115, true}) || ev.State.Key != "volume_up" {
		t.Errorf("key event = %+v", ev)
	}
	s.SetMicMuted(true)
	if ev := next(t, events, EventMic); !ev.State.MicMuted {
		t.Error("mic should be muted")
	}
	st, _ := s.Surface(ctx)
	if m := st.Map(); m["has_light"] != true || m["has_ring"] != false || m["key"] != "volume_up" {
		t.Errorf("map = %v", m)
	}
}
