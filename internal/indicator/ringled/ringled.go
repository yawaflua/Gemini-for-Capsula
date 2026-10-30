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

// Package ringled drives the 12-pixel LED ring of the speaker. [Ring] is
// both an [indicator.Indicator] and a [controller.Light] (night light), and
// renders animations frame by frame on a [Strip], written straight to the
// kernel's spidev.
package ringled

import (
	"cmp"
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/yawaflua/marusya-gemini/internal/controller"
	"github.com/yawaflua/marusya-gemini/internal/indicator"
	"github.com/yawaflua/marusya-gemini/internal/models"
)

// Colours of the states.
var (
	colorListening = RGB{0x00, 0x40, 0xff}
	colorThinking  = RGB{0x80, 0x00, 0xff}
	colorSpeaking  = RGB{0x00, 0xff, 0x40}
	colorError     = RGB{0xff, 0x00, 0x00}
	colorVolume    = RGB{0xff, 0xff, 0xff}
	colorAnnounce  = RGB{0xff, 0x70, 0x00}
)

// DefaultNightLight is a warm yellow.
const DefaultNightLight = "ffb000"

const (
	// errorHold is how long the transient error look stays on.
	errorHold = 3 * time.Second
	// volumeHold is how long a volume change is shown.
	volumeHold = 1500 * time.Millisecond
	// frameInterval paces animations (30 frames a second).
	frameInterval = time.Second / 30
)

// Ring drives the LED ring. All methods are non-blocking: a background
// worker renders the latest desired look.
type Ring struct {
	strip      Strip
	brightness int
	log        *slog.Logger
	errorHold  time.Duration
	interval   time.Duration

	mu          sync.Mutex
	state       indicator.State
	errorUntil  time.Time
	muted       bool
	night       *nightLight
	volume      int
	volMuted    bool
	volumeUntil time.Time
	manual      *look // set with Override, nil when automatic
	manualUntil time.Time

	notify chan struct{}
	done   chan struct{}
	exited chan struct{}
}

type nightLight struct {
	color      RGB
	brightness int // per cent; zero uses the ring brightness
}

var (
	_ indicator.Indicator = (*Ring)(nil)
	_ controller.Light    = (*Ring)(nil)
)

// New starts a ring driver on strip; brightness (per cent) scales every look
// but the night light, which has its own.
func New(strip Strip, brightness int, log *slog.Logger) *Ring {
	if log == nil {
		log = slog.Default()
	}
	r := &Ring{strip: strip, brightness: brightness, log: log, errorHold: errorHold, interval: frameInterval}
	return r.start()
}

func (r *Ring) start() *Ring {
	r.notify = make(chan struct{}, 1)
	r.done = make(chan struct{})
	r.exited = make(chan struct{})
	go r.loop()
	return r
}

// Close turns the ring off and stops the worker.
func (r *Ring) Close() {
	close(r.done)
	<-r.exited
}

func (r *Ring) poke() {
	select {
	case r.notify <- struct{}{}:
	default:
	}
}

func (r *Ring) update(f func()) {
	r.mu.Lock()
	f()
	r.mu.Unlock()
	r.poke()
}

// Show implements [indicator.Indicator].
func (r *Ring) Show(s indicator.State) {
	r.update(func() {
		r.state = s
		if s == indicator.Error {
			r.errorUntil = time.Now().Add(r.errorHold)
		}
	})
}

// SetMuted shows the muted look while idle: red spreading from the MICMUTE
// key over the whole ring.
func (r *Ring) SetMuted(m bool) { r.update(func() { r.muted = m }) }

// ShowVolume shows the volume level as an arc for a moment.
func (r *Ring) ShowVolume(level int, muted bool) {
	r.update(func() {
		r.volume, r.volMuted = min(max(level, 0), 100), muted
		r.volumeUntil = time.Now().Add(volumeHold)
	})
}

var hexColor = regexp.MustCompile(`^[0-9a-fA-F]{6}$`)

// SetNightLight implements [controller.Light].
func (r *Ring) SetNightLight(_ context.Context, on bool, color string, brightness int) error {
	color = strings.TrimPrefix(strings.TrimSpace(color), "#")
	if color == "" {
		color = DefaultNightLight
	}
	if !hexColor.MatchString(color) {
		return fmt.Errorf("invalid colour %q, want RRGGBB", color)
	}
	if brightness < 0 || brightness > 100 {
		return fmt.Errorf("brightness %d out of range 0..100", brightness)
	}
	c, _ := ParseRGB(strings.ToLower(color))
	r.update(func() {
		r.night = nil
		if on {
			r.night = &nightLight{color: c, brightness: brightness}
		}
	})
	return nil
}

// NightLight implements [controller.Light].
func (r *Ring) NightLight(context.Context) (bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.night != nil, nil
}

// look is what the ring should show; equal looks need no new scene.
// DefaultOverride is the colour of an override that gives none.
const DefaultOverride = "ffffff"

// Override shows a manual look (see [models.RingScenes]) until req.Hold
// passes or [Ring.ResetOverride] is called. The assistant states and the
// volume still show on top of it; the override wins over the night light
// and the mute indicator. It returns when the override ends.
func (r *Ring) Override(req models.RingRequest) (time.Time, error) {
	if err := req.Normalize(); err != nil {
		return time.Time{}, err
	}
	c, _ := ParseRGB(cmp.Or(req.Color, DefaultOverride))
	until := time.Now().Add(req.Hold)
	r.update(func() {
		r.manual = &look{kind: "manual:" + req.Scene, color: c, bright: req.Brightness}
		r.manualUntil = until
	})
	return until, nil
}

// ResetOverride returns the ring to the automatic state indicator.
func (r *Ring) ResetOverride() { r.update(func() { r.manual = nil }) }

type look struct {
	kind   string
	color  RGB
	bright int
	level  int
	muted  bool
}

// desired returns the look for the inputs at now and when it changes on its
// own next (zero: not before an input changes).
func (r *Ring) desired(now time.Time) (look, time.Time) {
	r.mu.Lock()
	defer r.mu.Unlock()
	var next time.Time
	if r.state == indicator.Error && !now.Before(r.errorUntil) {
		r.state = indicator.Idle
	}
	if r.state == indicator.Error {
		next = r.errorUntil
	}
	if r.manual != nil && !now.Before(r.manualUntil) {
		r.manual = nil
	}
	if r.manual != nil {
		next = earliest(next, r.manualUntil)
	}
	b := r.brightness
	switch {
	case now.Before(r.volumeUntil):
		return look{kind: "volume", bright: b, level: r.volume, muted: r.volMuted}, earliest(next, r.volumeUntil)
	case r.state != indicator.Idle:
		return look{kind: r.state.String(), bright: b}, next
	case r.manual != nil:
		m := *r.manual
		if m.bright == 0 {
			m.bright = b
		}
		return m, next
	case r.muted:
		return look{kind: "muted", color: colorError, bright: b}, next
	case r.night != nil:
		nb := r.night.brightness
		if nb == 0 {
			nb = b
		}
		return look{kind: "night", color: r.night.color, bright: nb}, next
	}
	return look{kind: "off"}, next
}

func earliest(a, b time.Time) time.Time {
	if a.IsZero() || b.Before(a) {
		return b
	}
	return a
}

func (r *Ring) loop() {
	defer close(r.exited)
	var (
		cur     look
		sc      scene = steady(fcol{})
		t0            = time.Now()
		shown   colors
		written *Frame
		failing bool
		lastLog time.Time
	)
	write := func(f Frame) {
		if written != nil && *written == f {
			return
		}
		if err := r.strip.Write(f); err != nil {
			if !failing || time.Since(lastLog) > 30*time.Second {
				r.log.Warn("LED ring", "err", err)
				lastLog = time.Now()
			}
			failing = true
			written = nil
			return
		}
		failing = false
		written = &f
	}
	first := true
	for {
		now := time.Now()
		lk, next := r.desired(now)
		if first || lk != cur {
			sc = transition(cur, lk, shown)
			cur, t0, first = lk, now, false
		}
		var moving bool
		shown, moving = sc.at(now.Sub(t0))
		write(shown.frame())

		var wait <-chan time.Time
		switch {
		case moving:
			d := r.interval - time.Since(now)
			wait = time.After(max(d, time.Millisecond))
		case !next.IsZero():
			wait = time.After(max(time.Until(next), time.Millisecond))
		}
		select {
		case <-r.done:
			if err := r.strip.Write(Frame{}); err != nil {
				r.log.Warn("LED ring: turn off", "err", err)
			}
			return
		case <-r.notify:
		case <-wait:
		}
	}
}

// spiDriver is the sysfs directory of the SoC SPI controller driver.
var spiDriver = "/sys/bus/platform/drivers/meson-spicc"

// bindSPI binds every unbound meson-spicc controller. The controller is
// rebound to reprogram the SPI clock; if that is interrupted in between
// (the agent killed or restarted), it stays unbound and the ring
// stops working.
func bindSPI() ([]string, error) {
	devs, _ := filepath.Glob("/sys/bus/platform/devices/*")
	var bound, seen []string
	for _, d := range devs {
		name := filepath.Base(d)
		compat, err := os.ReadFile(filepath.Join(d, "of_node", "compatible"))
		if err != nil || !(strings.Contains(string(compat), "spicc") || strings.Contains(name, "spi")) {
			continue
		}
		if drv, err := os.Readlink(filepath.Join(d, "driver")); err == nil {
			seen = append(seen, name+"->"+filepath.Base(drv))
			continue // already bound
		}
		seen = append(seen, name+"->(none)")
		if !strings.Contains(string(compat), "spicc") {
			continue
		}
		if err := os.WriteFile(filepath.Join(spiDriver, "bind"), []byte(name), 0); err != nil {
			return nil, fmt.Errorf("bind %s: %w", name, err)
		}
		bound = append(bound, name)
	}
	if len(bound) == 0 {
		return nil, fmt.Errorf("no unbound spicc controller found (spi devices: %v)", seen)
	}
	return bound, nil
}
