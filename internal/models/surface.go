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

package models

import (
	"fmt"
	"regexp"
	"slices"
	"strings"
	"time"
)

// Manual overrides of the ring and the display always expire: a forgotten
// override must not hide the assistant's state for good.
const (
	// DefaultOverride is used when a request gives no duration.
	DefaultOverride = 10 * time.Minute
	// MaxOverride is the longest override.
	MaxOverride = 12 * time.Hour
)

// RingScenes are the looks [ActionRingSet] accepts. "solid", "breathe",
// "spin" and "pulse" use the requested colour; "rainbow" ignores it; the
// rest replay the assistant's own animations in their own colours.
var RingScenes = []string{
	"solid", "breathe", "spin", "pulse", "rainbow", "off",
	"listening", "thinking", "speaking", "error", "announcing", "disconnected",
}

// DisplayModes are the modes [ActionDisplayShow] accepts: "text" shows a
// short text, "clock" the time, "off" blanks the display, "sleep" and
// "wake" force the sleeping or the awake face whatever the light, and
// "face" keeps the automatic face (to change only the brightness).
var DisplayModes = []string{"text", "clock", "off", "sleep", "wake", "face"}

var hexColor = regexp.MustCompile(`^[0-9a-f]{6}$`)

// RingRequest is a manual look of the LED ring.
type RingRequest struct {
	Scene string
	// Color is RRGGBB in lower case; empty uses the scene default.
	Color string
	// Brightness is per cent, 1..100; zero uses the ring default.
	Brightness int
	// Hold is how long the override lasts; zero is [DefaultOverride].
	Hold time.Duration
}

// Normalize fills defaults and validates the request.
func (r *RingRequest) Normalize() error {
	r.Scene = strings.ToLower(strings.TrimSpace(r.Scene))
	if r.Scene == "" {
		r.Scene = "solid"
	}
	if !slices.Contains(RingScenes, r.Scene) {
		return fmt.Errorf("unknown scene %q, want one of %s", r.Scene, strings.Join(RingScenes, ", "))
	}
	r.Color = strings.ToLower(strings.TrimPrefix(strings.TrimSpace(r.Color), "#"))
	if r.Color != "" && !hexColor.MatchString(r.Color) {
		return fmt.Errorf("invalid colour %q, want RRGGBB", r.Color)
	}
	if r.Brightness < 0 || r.Brightness > 100 {
		return fmt.Errorf("brightness %d out of range 0..100", r.Brightness)
	}
	var err error
	r.Hold, err = normalizeHold(r.Hold)
	return err
}

// DisplayRequest is a manual mode of the 7-segment display.
type DisplayRequest struct {
	// Mode is one of [DisplayModes]; empty is "text" with a text and
	// "face" without.
	Mode string
	Text string
	// Brightness is per cent, 1..100; zero follows the light sensor.
	Brightness int
	// Hold is how long the override lasts; zero is [DefaultOverride].
	Hold time.Duration
}

// Normalize fills defaults and validates the request.
func (r *DisplayRequest) Normalize() error {
	r.Mode = strings.ToLower(strings.TrimSpace(r.Mode))
	r.Text = strings.TrimSpace(r.Text)
	if r.Mode == "" {
		r.Mode = "face"
		if r.Text != "" {
			r.Mode = "text"
		}
	}
	if !slices.Contains(DisplayModes, r.Mode) {
		return fmt.Errorf("unknown mode %q, want one of %s", r.Mode, strings.Join(DisplayModes, ", "))
	}
	if r.Mode == "text" {
		if r.Text == "" {
			return fmt.Errorf("missing text")
		}
		for _, c := range r.Text {
			if c < ' ' || c > '~' {
				return fmt.Errorf("the display shows Latin letters, digits and signs only, not %q", c)
			}
		}
		if len(r.Text) > 4 {
			return fmt.Errorf("text %q is longer than 4 characters", r.Text)
		}
	} else {
		r.Text = ""
	}
	if r.Brightness < 0 || r.Brightness > 100 {
		return fmt.Errorf("brightness %d out of range 0..100", r.Brightness)
	}
	var err error
	r.Hold, err = normalizeHold(r.Hold)
	return err
}

func normalizeHold(d time.Duration) (time.Duration, error) {
	switch {
	case d == 0:
		return DefaultOverride, nil
	case d < 0 || d > MaxOverride:
		return 0, fmt.Errorf("duration %s out of range 1s..%s", d, MaxOverride)
	}
	return max(d, time.Second), nil
}

// Override is an active manual ring or display override.
type Override struct {
	Scene      string    `json:"scene,omitempty"` // ring
	Mode       string    `json:"mode,omitempty"`  // display
	Text       string    `json:"text,omitempty"`
	Color      string    `json:"color,omitempty"`
	Brightness int       `json:"brightness,omitempty"`
	Until      time.Time `json:"until"`
}

// Map converts the override for command results.
func (o *Override) Map() map[string]any {
	if o == nil {
		return nil
	}
	m := map[string]any{
		"until":   o.Until.UTC().Format(time.RFC3339),
		"seconds": max(0, int(time.Until(o.Until).Round(time.Second).Seconds())),
	}
	for k, v := range map[string]string{"scene": o.Scene, "mode": o.Mode, "text": o.Text, "color": o.Color} {
		if v != "" {
			m[k] = v
		}
	}
	if o.Brightness != 0 {
		m["brightness"] = o.Brightness
	}
	return m
}

// SurfaceState is what the speaker shows and senses.
type SurfaceState struct {
	// State is the assistant state: idle, listening, thinking, ...
	State string
	// Lux is the ambient light; nil without a sensor or a reading.
	Lux *float64
	// Asleep tells whether the face sleeps (in the dark or forced).
	Asleep bool
	// MicMuted tells whether the microphone is muted with the touch key.
	MicMuted bool
	// Key is the last touch key, e.g. "volume_up", and when it was pressed.
	Key   string
	KeyAt time.Time
	// Ring and Display are the manual overrides; nil means automatic.
	Ring, Display *Override
	// HasRing, HasDisplay and HasLight tell which hardware is present.
	HasRing, HasDisplay, HasLight bool
}

// Map converts the state for command results.
func (s SurfaceState) Map() map[string]any {
	m := map[string]any{
		"state": s.State, "asleep": s.Asleep, "mic_muted": s.MicMuted,
		"has_ring": s.HasRing, "has_display": s.HasDisplay, "has_light": s.HasLight,
	}
	if s.HasRing {
		m["ring_scenes"] = RingScenes
	}
	if s.HasDisplay {
		m["display_modes"] = DisplayModes
	}
	if s.Lux != nil {
		m["lux"] = *s.Lux
	}
	if s.Key != "" {
		m["key"] = s.Key
		m["key_at"] = s.KeyAt.UTC().Format(time.RFC3339)
	}
	if s.Ring != nil {
		m["ring"] = s.Ring.Map()
	}
	if s.Display != nil {
		m["display"] = s.Display.Map()
	}
	return m
}
