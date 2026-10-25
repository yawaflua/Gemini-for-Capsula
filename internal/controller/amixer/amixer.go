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

// Package amixer implements [controller.Volume] with the ALSA amixer utility.
package amixer

import (
	"context"
	"fmt"
	"regexp"
	"strconv"
	"sync"

	"github.com/yawaflua/marusya-gemini/internal/controller"
	"github.com/yawaflua/marusya-gemini/internal/models"
	"github.com/yawaflua/marusya-gemini/internal/syscmd"
)

// Volume controls an ALSA simple mixer control.
type Volume struct {
	// Card is the ALSA card index or name; empty uses the default card.
	Card string
	// Control is the simple mixer control, e.g. "Master", "PCM", "Speaker".
	Control string
	// Runner executes amixer. Defaults to [syscmd.Exec].
	Runner syscmd.Runner

	// Controls without a playback switch (e.g. Master on the VK Capsule)
	// reject "mute"; muting is then emulated by setting the level to 0 and
	// restoring the saved level on unmute.
	mu        sync.Mutex
	softMuted bool
	saved     int
}

// unmutedLevel is restored when a soft mute started at level 0.
const unmutedLevel = 30

var _ controller.Volume = (*Volume)(nil)

func (v *Volume) run(ctx context.Context, args ...string) (string, error) {
	if v.Card != "" {
		args = append([]string{"-c", v.Card}, args...)
	}
	r := v.Runner
	if r == nil {
		r = syscmd.Exec{}
	}
	return r.Run(ctx, "amixer", args...)
}

func (v *Volume) control() string {
	if v.Control == "" {
		return "Master"
	}
	return v.Control
}

var (
	levelRE  = regexp.MustCompile(`\[(\d{1,3})%\]`)
	switchRE = regexp.MustCompile(`\[(on|off)\]`)
)

// Get implements [controller.Volume].
func (v *Volume) Get(ctx context.Context) (models.VolumeState, error) {
	out, err := v.run(ctx, "sget", v.control())
	if err != nil {
		return models.VolumeState{}, err
	}
	s, err := parse(out)
	if err != nil {
		return s, err
	}
	v.mu.Lock()
	defer v.mu.Unlock()
	if v.softMuted {
		if s.Level != 0 {
			v.softMuted = false // changed behind our back
		} else {
			s.Level, s.Muted = v.saved, true
		}
	}
	return s, nil
}

func parse(out string) (models.VolumeState, error) {
	m := levelRE.FindStringSubmatch(out)
	if m == nil {
		return models.VolumeState{}, fmt.Errorf("amixer: cannot parse level from %q", out)
	}
	level, _ := strconv.Atoi(m[1])
	s := models.VolumeState{Level: level}
	if sw := switchRE.FindStringSubmatch(out); sw != nil {
		s.Muted = sw[1] == "off"
	}
	return s, nil
}

// Set implements [controller.Volume]. Setting a level also unmutes.
func (v *Volume) Set(ctx context.Context, level int) error {
	v.mu.Lock()
	v.softMuted = false
	v.mu.Unlock()
	_, err := v.run(ctx, "-q", "sset", v.control(), fmt.Sprintf("%d%%", level), "unmute")
	if err != nil {
		// Controls without a switch reject "unmute"; retry with the level only.
		_, err = v.run(ctx, "-q", "sset", v.control(), fmt.Sprintf("%d%%", level))
	}
	return err
}

// SetMuted implements [controller.Volume].
func (v *Volume) SetMuted(ctx context.Context, muted bool) error {
	state := "unmute"
	if muted {
		state = "mute"
	}
	_, err := v.run(ctx, "-q", "sset", v.control(), state)
	if err == nil {
		return nil
	}
	return v.softMute(ctx, muted)
}

func (v *Volume) softMute(ctx context.Context, muted bool) error {
	v.mu.Lock()
	defer v.mu.Unlock()
	if muted {
		if v.softMuted {
			return nil
		}
		out, err := v.run(ctx, "sget", v.control())
		if err != nil {
			return err
		}
		s, err := parse(out)
		if err != nil {
			return err
		}
		if _, err := v.run(ctx, "-q", "sset", v.control(), "0%"); err != nil {
			return err
		}
		v.softMuted, v.saved = true, s.Level
		return nil
	}
	if !v.softMuted {
		return nil
	}
	level := v.saved
	if level == 0 {
		level = unmutedLevel
	}
	if _, err := v.run(ctx, "-q", "sset", v.control(), fmt.Sprintf("%d%%", level)); err != nil {
		return err
	}
	v.softMuted = false
	return nil
}
