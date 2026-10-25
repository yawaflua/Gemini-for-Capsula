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
	"log/slog"
	"math"
	"math/rand"
	"time"

	"github.com/yawaflua/marusya-gemini/internal/indicator"
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
		ch: make(chan msg, 64), errorHold: hold, quit: make(chan struct{}), done: make(chan struct{})}
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

func (f *Face) apply(m msg) {
	switch {
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

func (f *Face) ambient(lux float64) {
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
	if !ok || f.brightness < 0 {
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
	if f.dark {
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
func (f *Face) wait(d time.Duration) (msg, bool) {
	t := time.NewTimer(d)
	defer t.Stop()
	for {
		select {
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
		case <-t.C:
			return msg{}, false
		}
	}
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
