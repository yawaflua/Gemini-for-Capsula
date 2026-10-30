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

package face

import (
	"errors"
	"log/slog"
	"math"
	"math/rand"
	"sync/atomic"
	"time"

	"github.com/yawaflua/marusya-gemini/internal/indicator"
	"github.com/yawaflua/marusya-gemini/internal/models"
)

// Renderer is the display the face is drawn on; see seg7.Panel.
type Renderer interface {
	Show(digits [4]byte, nose bool) error
	Text(s string) error
	Off()
}

// Dimmer is implemented by renderers with adjustable brightness.
type Dimmer interface {
	SetBrightness(pct int) error
}

// Ambient light thresholds, lux, with hysteresis.
const (
	darkBelow  = 1.0
	lightAbove = 3.0
)

// BrightnessFor maps ambient illuminance to display brightness: 10% in the
// dark, about 40% at 10 lux, 70% at 100 lux and full brightness in daylight.
func BrightnessFor(lux float64) int {
	return int(max(10, min(100, 10+30*math.Log10(max(0, lux)+1))))
}

// errorHold is how long the sad face stays after an error.
const errorHold = 3 * time.Second

type msg struct {
	state   *indicator.State
	muted   *bool
	lux     *float64
	text    string
	hold    time.Duration
	overlay bool
	// setManual replaces the manual override with manual (nil resets).
	setManual bool
	manual    *override
}

// override is a manual display mode set with [Face.Override].
type override struct {
	mode       string
	text       string
	brightness int
	until      time.Time
}

// Face animates the assistant state. It also shows short text overlays such
// as the volume level. All methods are non-blocking.
type Face struct {
	r          Renderer
	log        *slog.Logger
	rnd        *rand.Rand
	ch         chan msg
	state      indicator.State
	muted      bool
	dark       bool
	brightness int
	ov         *msg
	manual     *override
	lux        float64
	luxSeen    bool
	asleep     atomic.Bool

	now       func() time.Time
	errorHold time.Duration
	quit      chan struct{}
	done      chan struct{}
}

var _ indicator.Indicator = (*Face)(nil)

// New starts animating on r.
func New(r Renderer, log *slog.Logger) *Face { return newFace(r, log, errorHold) }

func newFace(r Renderer, log *slog.Logger, hold time.Duration) *Face {
	if log == nil {
		log = slog.Default()
	}
	f := &Face{r: r, log: log, rnd: rand.New(rand.NewSource(time.Now().UnixNano())),
		ch: make(chan msg, 64), errorHold: hold, now: time.Now, quit: make(chan struct{}), done: make(chan struct{})}
	go f.loop()
	return f
}

func (f *Face) post(m msg) {
	select {
	case f.ch <- m:
	default:
		f.log.Warn("face: dropping update, display is busy")
	}
}

// Show implements [indicator.Indicator].
func (f *Face) Show(s indicator.State) { f.post(msg{state: &s}) }

// SetMuted shows covered eyes while idle and muted.
func (f *Face) SetMuted(m bool) { f.post(msg{muted: &m}) }

// SetAmbient reports the ambient illuminance: the display is dimmed in a
// dark room and the idle face falls asleep.
func (f *Face) SetAmbient(lux float64) { f.post(msg{lux: &lux}) }

// Overlay shows text (up to 4 characters) for hold, then resumes the face.
func (f *Face) Overlay(text string, hold time.Duration) {
	f.post(msg{overlay: true, text: text, hold: hold})
}

// ErrNotDimmable is returned for a brightness on a display that has none.
var ErrNotDimmable = errors.New("the display brightness is not adjustable")

// Override puts the display in a manual mode (see [models.DisplayModes])
// until req.Hold passes or [Face.ResetOverride] is called. The assistant
// states still show on top of it. It returns when the override ends.
func (f *Face) Override(req models.DisplayRequest) (time.Time, error) {
	if err := req.Normalize(); err != nil {
		return time.Time{}, err
	}
	if _, ok := f.r.(Dimmer); req.Brightness > 0 && !ok {
		return time.Time{}, ErrNotDimmable
	}
	until := time.Now().Add(req.Hold)
	f.post(msg{setManual: true, manual: &override{mode: req.Mode, text: req.Text, brightness: req.Brightness, until: until}})
	return until, nil
}

// ResetOverride returns the display to the automatic face.
func (f *Face) ResetOverride() { f.post(msg{setManual: true}) }

// Asleep tells whether the face is sleeping: in the dark or by an override.
func (f *Face) Asleep() bool { return f.asleep.Load() }

func (f *Face) apply(m msg) {
	switch {
	case m.setManual:
		f.setManual(m.manual)
	case m.state != nil:
		f.state = *m.state
	case m.muted != nil:
		f.muted = *m.muted
	case m.lux != nil:
		f.ambient(*m.lux)
	case m.overlay:
		f.ov = &m
	}
}

// setManual replaces the override and applies or restores its brightness.
func (f *Face) setManual(o *override) {
	had := f.manual != nil && f.manual.brightness > 0
	f.manual = o
	d, ok := f.r.(Dimmer)
	switch {
	case !ok:
	case o != nil && o.brightness > 0:
		if err := d.SetBrightness(o.brightness); err != nil {
			f.log.Warn("face: set brightness", "err", err)
		}
	case had:
		// Back to the light sensor, or full brightness without one.
		pct := 100
		if f.luxSeen {
			pct = BrightnessFor(f.lux)
		}
		if err := d.SetBrightness(pct); err != nil {
			f.log.Warn("face: restore brightness", "err", err)
		}
		if f.brightness >= 0 {
			f.brightness = pct
		}
	}
}

// expire drops an override whose time is up.
func (f *Face) expire() {
	if f.manual != nil && !f.now().Before(f.manual.until) {
		f.setManual(nil)
	}
}

func (f *Face) ambient(lux float64) {
	f.lux, f.luxSeen = lux, true
	was := f.dark
	switch {
	case lux < darkBelow:
		f.dark = true
	case lux > lightAbove:
		f.dark = false
	}
	if f.dark != was {
		f.log.Info("face: ambient light changed", "lux", lux, "asleep", f.dark)
	}
	d, ok := f.r.(Dimmer)
	if !ok || f.brightness < 0 || (f.manual != nil && f.manual.brightness > 0) {
		return
	}
	pct := BrightnessFor(lux)
	if f.brightness != 0 && abs(pct-f.brightness) < 5 {
		return
	}
	if err := d.SetBrightness(pct); err != nil {
		if f.brightness == 0 {
			f.log.Info("face: display brightness is not adjustable", "err", err)
		}
		f.brightness = -1 // do not retry on every reading
		return
	}
	f.brightness = pct
}

func abs(x int) int {
	if x < 0 {
		return -x
	}
	return x
}

// sequence returns the frames for the current state and whether they loop.
func (f *Face) sequence() ([]Frame, bool) {
	switch f.state {
	case indicator.Listening:
		return loopListening, true
	case indicator.Thinking:
		return loopThinking, true
	case indicator.Speaking, indicator.Announcing:
		return loopSpeaking, true
	case indicator.Error:
		return []Frame{sad(int(f.errorHold.Milliseconds()))}, false
	case indicator.Disconnected:
		return loopDisconnected, true
	}
	// In the dark the face sleeps whether or not the microphone is muted
	// (the ring shows the mute state).
	dark := f.dark
	if f.manual != nil {
		switch f.manual.mode {
		case "sleep":
			dark = true
		case "wake":
			dark = false
		}
	}
	f.asleep.Store(dark)
	if dark {
		return loopSleeping, true
	}
	if f.muted {
		return loopMuted, true
	}
	// Idle: rest for a random while, then play a random animation.
	seq := []Frame{neutral(2000 + f.rnd.Intn(4000))}
	var anim []Frame
	switch x := f.rnd.Intn(100); {
	case x < 60:
		anim = animBlink
	case x < 76:
		anim = animLook
	case x < 84:
		anim = animThink
	case x < 90:
		anim = animSquint
	case x < 96:
		anim = animWink
	default:
		anim = animHappy
	}
	return append(seq, anim...), false
}

// Close stops the animation and blanks the display.
func (f *Face) Close() {
	close(f.quit)
	<-f.done
	f.r.Off()
}

// wait sleeps for d unless an update arrives first.
// wait waits for d or a message. It also ends when the override expires,
// so that the loop drops it.
func (f *Face) wait(d time.Duration) (msg, bool) {
	expiring := false
	if f.manual != nil {
		if u := f.manual.until.Sub(f.now()); u < d {
			d, expiring = max(u, 0), true
		}
	}
	t := time.NewTimer(d)
	defer t.Stop()
	for {
		select {
		case <-t.C:
			return msg{}, expiring
		case m := <-f.ch:
			if m.lux == nil {
				return m, true
			}
			// Light readings arrive every few seconds; only restart the
			// animation if the face has to fall asleep or wake up.
			wasDark := f.dark
			f.ambient(*m.lux)
			if f.dark != wasDark {
				return msg{}, true
			}
			continue
		case <-f.quit:
			return msg{}, true // the loop checks quit
		}
	}
}

// showManual renders a text, clock or blank override while the assistant
// is idle. It reports false if the override leaves the face to the loop.
func (f *Face) showManual() bool {
	o := f.manual
	if o == nil || f.state != indicator.Idle {
		return false
	}
	var (
		err  error
		hold = o.until.Sub(f.now())
	)
	switch o.mode {
	case "text":
		err = f.r.Text(o.text)
	case "clock":
		now := f.now()
		err = f.r.Text(now.Format("1504"))
		hold = min(hold, now.Truncate(time.Minute).Add(time.Minute).Sub(now))
	case "off":
		err = f.r.Show([4]byte{}, false)
	default:
		return false
	}
	f.asleep.Store(false)
	if err != nil {
		f.log.Warn("face: render override", "err", err)
	}
	if m, hit := f.wait(hold); hit {
		f.apply(m)
	}
	return true
}

// play renders seq once; it stops early and returns the update that
// interrupted it.
func (f *Face) play(seq []Frame) (msg, bool) {
	for _, fr := range seq {
		if err := f.r.Show(fr.Digits, fr.Nose); err != nil {
			f.log.Warn("face: render", "err", err)
		}
		if m, hit := f.wait(fr.Hold); hit {
			return m, true
		}
	}
	return msg{}, false
}

func (f *Face) loop() {
	defer close(f.done)
	for {
		select {
		case <-f.quit:
			return
		default:
		}
		f.expire()
		if f.ov != nil {
			o := *f.ov
			f.ov = nil
			if err := f.r.Text(o.text); err != nil {
				f.log.Warn("face: render text", "err", err)
			}
			if m, hit := f.wait(o.hold); hit {
				f.apply(m)
			}
			continue
		}
		if f.showManual() {
			continue
		}
		seq, repeat := f.sequence()
		for {
			m, hit := f.play(seq)
			if hit {
				f.apply(m)
				break
			}
			if !repeat {
				if f.state == indicator.Error {
					f.state = indicator.Idle
				}
				break
			}
		}
	}
}
