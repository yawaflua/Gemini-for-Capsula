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

// Package surface ties together what the speaker shows and senses: the
// LED ring, the face on the 7-segment display, the ambient light sensor
// and the touch keys. It lets the ring and the display be taken over by
// hand for a while (from the web panel, over D-Bus or by the voice model)
// and reports state changes to subscribers such as the D-Bus service.
package surface

import (
	"context"
	"errors"
	"log/slog"
	"math"
	"sync"
	"time"

	"github.com/yawaflua/marusya-gemini/internal/controller"
	"github.com/yawaflua/marusya-gemini/internal/indicator"
	"github.com/yawaflua/marusya-gemini/internal/models"
)

// Ring is the LED ring; *ringled.Ring implements it.
type Ring interface {
	Override(models.RingRequest) (time.Time, error)
	ResetOverride()
}

// Face is the display; *face.Face implements it.
type Face interface {
	Override(models.DisplayRequest) (time.Time, error)
	ResetOverride()
	SetAmbient(lux float64)
	Asleep() bool
}

// LightSensor reads the ambient light; *light.Sensor implements it.
type LightSensor interface {
	Lux() (float64, error)
}

// Event kinds.
const (
	EventState   = "state"   // the assistant state changed
	EventLux     = "lux"     // the ambient light changed noticeably
	EventAsleep  = "asleep"  // the face fell asleep or woke up
	EventMic     = "mic"     // the microphone was muted or unmuted
	EventKey     = "key"     // a touch key was pressed or released
	EventRing    = "ring"    // the ring override was set, reset or expired
	EventDisplay = "display" // the display override was set, reset or expired
)

// Event is a change, with the state after it.
type Event struct {
	Kind  string
	State models.SurfaceState
	// Key is set for [EventKey].
	Key Key
}

// Key is a touch key press or release.
type Key struct {
	Name    string
	Code    uint16
	Pressed bool
}

// ErrNoHardware is returned for a ring or display the speaker lacks.
var ErrNoHardware = errors.New("no such hardware on this speaker")

// Surface is the speaker's ring, display and sensors. Set the fields
// before calling any method; nil fields mark missing hardware.
type Surface struct {
	Ring  Ring
	Face  Face
	Light LightSensor
	// LightInterval is how often the light sensor is read; default 2s.
	LightInterval time.Duration
	Logger        *slog.Logger

	mu      sync.Mutex
	state   indicator.State
	stateN  int // counts state changes
	lux     *float64
	asleep  bool
	mic     bool
	key     string
	keyAt   time.Time
	ring    *models.Override
	display *models.Override
	subs    map[chan Event]struct{}
}

var (
	_ indicator.Indicator = (*Surface)(nil)
	_ controller.Ring     = (*Surface)(nil)
	_ controller.Display  = (*Surface)(nil)
	_ controller.Sensors  = (*Surface)(nil)
)

func (s *Surface) log() *slog.Logger {
	if s.Logger == nil {
		return slog.Default()
	}
	return s.Logger
}

// Subscribe returns a channel of changes and a function that ends the
// subscription. Events are dropped for a subscriber that falls behind.
func (s *Surface) Subscribe() (<-chan Event, func()) {
	ch := make(chan Event, 32)
	s.mu.Lock()
	if s.subs == nil {
		s.subs = map[chan Event]struct{}{}
	}
	s.subs[ch] = struct{}{}
	s.mu.Unlock()
	return ch, func() {
		s.mu.Lock()
		defer s.mu.Unlock()
		if _, ok := s.subs[ch]; ok {
			delete(s.subs, ch)
			close(ch)
		}
	}
}

// emit sends an event to the subscribers. It must be called with s.mu held.
func (s *Surface) emit(kind string, key Key) {
	if len(s.subs) == 0 {
		return
	}
	ev := Event{Kind: kind, State: s.snapshot(), Key: key}
	for ch := range s.subs {
		select {
		case ch <- ev:
		default:
		}
	}
}

// snapshot must be called with s.mu held.
func (s *Surface) snapshot() models.SurfaceState {
	st := models.SurfaceState{
		State:      s.state.String(),
		Asleep:     s.asleep,
		MicMuted:   s.mic,
		Key:        s.key,
		KeyAt:      s.keyAt,
		HasRing:    s.Ring != nil,
		HasDisplay: s.Face != nil,
		HasLight:   s.Light != nil,
	}
	if s.lux != nil {
		v := *s.lux
		st.Lux = &v
	}
	now := time.Now()
	if o := s.ring; o != nil && now.Before(o.Until) {
		c := *o
		st.Ring = &c
	}
	if o := s.display; o != nil && now.Before(o.Until) {
		c := *o
		st.Display = &c
	}
	return st
}

// Surface implements [controller.Sensors].
func (s *Surface) Surface(context.Context) (models.SurfaceState, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.snapshot(), nil
}

// Show implements [indicator.Indicator]; add the surface to the agent's
// indicators to follow the assistant state.
func (s *Surface) Show(st indicator.State) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if st == s.state {
		return
	}
	s.state = st
	s.stateN++
	s.emit(EventState, Key{})
	if st == indicator.Error {
		// Error is transient: the indicators return to idle on their own.
		n := s.stateN
		time.AfterFunc(ErrorHold, func() {
			s.mu.Lock()
			defer s.mu.Unlock()
			if s.stateN == n {
				s.state = indicator.Idle
				s.emit(EventState, Key{})
			}
		})
	}
}

// ErrorHold is how long the error state lasts, as on the ring and the face.
var ErrorHold = 3 * time.Second

// SetMicMuted records the microphone mute state.
func (s *Surface) SetMicMuted(m bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if m == s.mic {
		return
	}
	s.mic = m
	s.emit(EventMic, Key{})
}

// KeyEvent records a touch key; it fits [touchkeys.Panel.OnKey].
func (s *Surface) KeyEvent(name string, code uint16, pressed bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if pressed {
		s.key, s.keyAt = name, time.Now()
	}
	s.emit(EventKey, Key{Name: name, Code: code, Pressed: pressed})
}

// SetRing implements [controller.Ring].
func (s *Surface) SetRing(_ context.Context, req models.RingRequest) (models.Override, error) {
	if s.Ring == nil {
		return models.Override{}, ErrNoHardware
	}
	if err := req.Normalize(); err != nil {
		return models.Override{}, err
	}
	until, err := s.Ring.Override(req)
	if err != nil {
		return models.Override{}, err
	}
	o := &models.Override{Scene: req.Scene, Color: req.Color, Brightness: req.Brightness, Until: until}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.ring = o
	s.emit(EventRing, Key{})
	s.expireLater(o, EventRing)
	return *o, nil
}

// ResetRing implements [controller.Ring].
func (s *Surface) ResetRing(context.Context) error {
	if s.Ring == nil {
		return ErrNoHardware
	}
	s.Ring.ResetOverride()
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.ring != nil {
		s.ring = nil
		s.emit(EventRing, Key{})
	}
	return nil
}

// ShowDisplay implements [controller.Display].
func (s *Surface) ShowDisplay(_ context.Context, req models.DisplayRequest) (models.Override, error) {
	if s.Face == nil {
		return models.Override{}, ErrNoHardware
	}
	if err := req.Normalize(); err != nil {
		return models.Override{}, err
	}
	until, err := s.Face.Override(req)
	if err != nil {
		return models.Override{}, err
	}
	o := &models.Override{Mode: req.Mode, Text: req.Text, Brightness: req.Brightness, Until: until}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.display = o
	s.emit(EventDisplay, Key{})
	s.expireLater(o, EventDisplay)
	return *o, nil
}

// ResetDisplay implements [controller.Display].
func (s *Surface) ResetDisplay(context.Context) error {
	if s.Face == nil {
		return ErrNoHardware
	}
	s.Face.ResetOverride()
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.display != nil {
		s.display = nil
		s.emit(EventDisplay, Key{})
	}
	return nil
}

// expireLater forgets the override o when it ends, unless it has been
// replaced by then. The ring and the face drop it on their own.
func (s *Surface) expireLater(o *models.Override, kind string) {
	time.AfterFunc(time.Until(o.Until), func() {
		s.mu.Lock()
		defer s.mu.Unlock()
		p := &s.ring
		if kind == EventDisplay {
			p = &s.display
		}
		if *p == o {
			*p = nil
			s.emit(kind, Key{})
		}
	})
}

// Run reads the light sensor, feeds the face and watches whether it
// sleeps, until ctx is done. Without a sensor it only watches the face.
func (s *Surface) Run(ctx context.Context) {
	every := s.LightInterval
	if every <= 0 {
		every = 2 * time.Second
	}
	t := time.NewTicker(every)
	defer t.Stop()
	failing := false
	for {
		if s.Light != nil {
			lux, err := s.Light.Lux()
			switch {
			case err == nil:
				failing = false
				if s.Face != nil {
					s.Face.SetAmbient(lux)
				}
				s.setLux(lux)
			case !failing:
				s.log().Warn("ambient light sensor read failed", "err", err)
				failing = true
			}
		}
		if s.Face != nil {
			s.setAsleep(s.Face.Asleep())
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// setLux records a reading and reports it if it changed noticeably: by a
// tenth, or by half a lux in the dark.
func (s *Surface) setLux(lux float64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.lux != nil && math.Abs(lux-*s.lux) < max(0.5, *s.lux/10) {
		return
	}
	s.lux = &lux
	s.emit(EventLux, Key{})
}

func (s *Surface) setAsleep(a bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if a == s.asleep {
		return
	}
	s.asleep = a
	s.emit(EventAsleep, Key{})
}
