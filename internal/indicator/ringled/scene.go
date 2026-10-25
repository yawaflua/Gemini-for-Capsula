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

package ringled

import (
	"math"
	"time"
)

// fcol is a pixel in 0..255 per channel, brightness already applied.
type fcol struct{ r, g, b float64 }

// colors is a frame being computed.
type colors [Pixels]fcol

func scaled(c RGB, bright int, k float64) fcol {
	m := float64(bright) / 100 * k
	return fcol{float64(c.R) * m, float64(c.G) * m, float64(c.B) * m}
}

func lerp(a, b fcol, k float64) fcol {
	return fcol{a.r + (b.r-a.r)*k, a.g + (b.g-a.g)*k, a.b + (b.b-a.b)*k}
}

func (c colors) frame() Frame {
	var f Frame
	q := func(v float64) uint8 { return uint8(math.Round(min(max(v, 0), 255))) }
	for i, p := range c {
		f[i] = RGB{q(p.r), q(p.g), q(p.b)}
	}
	return f
}

// scene computes the ring at time t since it started; moving reports
// whether later frames differ.
type scene interface {
	at(t time.Duration) (c colors, moving bool)
}

type sceneFunc func(t time.Duration) (colors, bool)

func (f sceneFunc) at(t time.Duration) (colors, bool) { return f(t) }

func fill(c fcol) colors {
	var out colors
	for i := range out {
		out[i] = c
	}
	return out
}

func steady(c fcol) scene { return still(fill(c)) }

func still(f colors) scene {
	return sceneFunc(func(time.Duration) (colors, bool) { return f, false })
}

// dist is how far pixel i is from the MICMUTE key (pixel 0), 0..6.
func dist(i int) int { return min(i, Pixels-i) }

// byDist lists the pixels from the MICMUTE key outwards: 0, 1, 11, 2, 10…
var byDist = [Pixels]int{0, 1, 11, 2, 10, 3, 9, 4, 8, 5, 7, 6}

func frac(t, d time.Duration) float64 { return min(max(float64(t)/float64(d), 0), 1) }

// base is the scene of a look once any transition is over.
func base(l look) scene {
	switch l.kind {
	case "off":
		return steady(fcol{})
	case "listening":
		// A short flash on wake (the button press feedback), then steady.
		const settle = 400 * time.Millisecond
		return sceneFunc(func(t time.Duration) (colors, bool) {
			k := 1 - frac(t, settle)
			return fill(scaled(colorListening, l.bright, 1+0.7*k*k)), t < settle
		})
	case "thinking":
		return spinner(colorThinking, l.bright, 1100*time.Millisecond)
	case "speaking":
		const period = 1600 * time.Millisecond
		return sceneFunc(func(t time.Duration) (colors, bool) {
			ph := 2 * math.Pi * float64(t%period) / float64(period)
			return fill(scaled(colorSpeaking, l.bright, 0.55+0.45*(0.5-0.5*math.Cos(ph)))), true
		})
	case "error":
		// Two blinks, then steady until the error look expires.
		const blink = 150 * time.Millisecond
		on, off := fill(scaled(colorError, l.bright, 1)), fill(fcol{})
		return sceneFunc(func(t time.Duration) (colors, bool) {
			if t < 4*blink && (t/blink)%2 == 1 {
				return off, true
			}
			return on, t < 4*blink
		})
	case "announcing":
		// Orange, breathing: someone else is talking through the speaker.
		const period = 1200 * time.Millisecond
		return sceneFunc(func(t time.Duration) (colors, bool) {
			ph := 2 * math.Pi * float64(t%period) / float64(period)
			return fill(scaled(colorAnnounce, l.bright, 0.4+0.6*(0.5-0.5*math.Cos(ph)))), true
		})
	case "disconnected":
		return steady(scaled(colorError, l.bright, 0.5))
	case "volume":
		return still(volumeArc(l))
	}
	return steady(scaled(l.color, l.bright, 1)) // muted, night
}

// spinner is a comet running round the ring, over a faint glow.
func spinner(c RGB, bright int, period time.Duration) scene {
	const tail = 5.0
	return sceneFunc(func(t time.Duration) (colors, bool) {
		head := float64(t%period) / float64(period) * Pixels
		var out colors
		for i := range out {
			d := math.Mod(head-float64(i)+Pixels, Pixels) // pixels behind the head
			var k float64
			switch {
			case d > Pixels-1: // the next pixel fades in
				k = d - (Pixels - 1)
			case d <= tail:
				k = 1 - d/tail
				k *= k
			}
			out[i] = scaled(c, bright, 0.08+0.92*k)
		}
		return out, true
	})
}

// volumeArc lights the level as an arc growing from the MICMUTE key.
func volumeArc(l look) colors {
	var out colors
	if l.muted {
		out[0] = scaled(colorError, l.bright, 1)
		return out
	}
	n := float64(l.level) / 100 * Pixels
	for rank, i := range byDist {
		out[i] = scaled(colorVolume, l.bright, min(max(n-float64(rank), 0), 1))
	}
	return out
}

// transition moves from the colours shown to the scene of next. Muting
// spreads red from the MICMUTE key over the ring; unmuting withdraws it back
// into the key; waking sweeps quickly the same way; the rest crossfade.
func transition(prev, next look, shown colors) scene {
	var delay [Pixels]time.Duration
	fade := 250 * time.Millisecond
	switch {
	case next.kind == "muted" && prev.kind != "volume":
		fade = 160 * time.Millisecond
		for i := range delay {
			delay[i] = time.Duration(dist(i)) * 60 * time.Millisecond
		}
	case prev.kind == "muted" && next.kind != "volume" && next.kind != "listening":
		fade = 160 * time.Millisecond
		for i := range delay {
			delay[i] = time.Duration(Pixels/2-dist(i)) * 60 * time.Millisecond
		}
	case next.kind == "listening":
		fade = 90 * time.Millisecond
		for i := range delay {
			delay[i] = time.Duration(dist(i)) * 20 * time.Millisecond
		}
	case next.kind == "volume" || prev.kind == "volume":
		fade = 120 * time.Millisecond
	}
	target := base(next)
	return sceneFunc(func(t time.Duration) (colors, bool) {
		c, moving := target.at(t)
		for i := range c {
			if k := frac(t-delay[i], fade); k < 1 {
				c[i] = lerp(shown[i], c[i], k)
				moving = true
			}
		}
		return c, moving
	})
}
