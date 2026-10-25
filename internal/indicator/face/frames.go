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

// Package face implements [indicator.Indicator] as an animated face on the
// 7-segment panel: two eyes on the outer digits and the colon dot as a nose,
// e.g. "0 . 0". Looking aside moves both eyes to one half, like turning the
// head. Faces and animations are ported from the firmware's mordy utility.
package face

import (
	"time"

	"github.com/yawaflua/marusya-gemini/internal/display/seg7"
)

// Eye masks.
const (
	eyeOpen   = seg7.A | seg7.B | seg7.F | seg7.G                   // upper square
	eyeBig    = seg7.A | seg7.B | seg7.C | seg7.D | seg7.E | seg7.F // 0
	eyeClosed = seg7.D                                              // _
	eyeLine   = seg7.G                                              // -
	eyeHappy  = seg7.F | seg7.A | seg7.B                            // ∩
	eyeSad    = seg7.C | seg7.E | seg7.G                            // drooping
	eyeLow    = seg7.C | seg7.D | seg7.E | seg7.G                   // lower square
	eyeLeft   = seg7.E | seg7.F                                     // pupil left
	eyeRight  = seg7.B | seg7.C                                     // pupil right
	eyeAngryL = seg7.A | seg7.B | seg7.G
	eyeAngryR = seg7.A | seg7.F | seg7.G
	letterZ   = seg7.A | seg7.B | seg7.G | seg7.E | seg7.D
)

// Frame is one picture of an animation.
type Frame struct {
	Digits [4]byte
	Nose   bool
	Hold   time.Duration
}

// Layouts place the two eyes on the panel.
func wide(l, r byte) [4]byte    { return [4]byte{l, 0, 0, r} } // "0 . 0"
func narrow(l, r byte) [4]byte  { return [4]byte{0, l, r, 0} } // " 0.0 "
func toLeft(l, r byte) [4]byte  { return [4]byte{l, r, 0, 0} } // "00. "
func toRight(l, r byte) [4]byte { return [4]byte{0, 0, l, r} } // " .00"

func fr(d [4]byte, nose bool, ms int) Frame {
	return Frame{Digits: d, Nose: nose, Hold: time.Duration(ms) * time.Millisecond}
}

// Static faces, held for ms.
func neutral(ms int) Frame   { return fr(wide(eyeOpen, eyeOpen), true, ms) }
func happy(ms int) Frame     { return fr(wide(eyeHappy, eyeHappy), true, ms) }
func sad(ms int) Frame       { return fr(wide(eyeSad, eyeSad), true, ms) }
func surprised(ms int) Frame { return fr(wide(eyeBig, eyeBig), true, ms) }
func asleep(ms int) Frame    { return fr(wide(eyeClosed, eyeClosed), false, ms) }
func tired(ms int) Frame     { return fr(wide(eyeLine, eyeLine), true, ms) }
func winking(ms int) Frame   { return fr(wide(eyeOpen, eyeClosed), true, ms) }
func down(ms int) Frame      { return fr(wide(eyeLow, eyeLow), true, ms) }
func angry(ms int) Frame     { return fr(wide(eyeAngryL, eyeAngryR), true, ms) }
func lookLeft(ms int) Frame  { return fr(toLeft(eyeLeft, eyeLeft), true, ms) }
func lookRight(ms int) Frame { return fr(toRight(eyeRight, eyeRight), true, ms) }

// Idle animations, played between pauses.
var (
	animBlink = []Frame{neutral(150), tired(60), asleep(100), tired(60), neutral(300)}
	animWink  = []Frame{neutral(200), winking(450), neutral(300)}
	animLook  = []Frame{neutral(300), lookLeft(600), neutral(250), lookRight(600), neutral(300)}
	animThink = []Frame{
		neutral(200), fr(wide(eyeRight, eyeRight), true, 300),
		fr(toRight(eyeRight, eyeRight), false, 400), fr(toRight(eyeRight, eyeRight), true, 400),
		neutral(300),
	}
	animHappy = []Frame{neutral(200), happy(300), neutral(120), happy(500)}
	// animSquint pulls the eyes together for a moment, mordy-style.
	animSquint = []Frame{neutral(200), fr(narrow(eyeOpen, eyeOpen), true, 500), tired(80), neutral(300)}
)

// State loops.
var (
	// loopListening: wide-open attentive eyes with a rare quick blink.
	loopListening = []Frame{surprised(1800), fr(wide(eyeLine, eyeLine), true, 70)}
	// loopThinking: eyes wander from side to side, the nose flickers.
	loopThinking = []Frame{
		fr(toRight(eyeRight, eyeRight), true, 350), fr(toRight(eyeRight, eyeRight), false, 250),
		down(250),
		fr(toLeft(eyeLeft, eyeLeft), true, 350), fr(toLeft(eyeLeft, eyeLeft), false, 250),
		fr(wide(eyeOpen, eyeOpen), false, 200),
	}
	// loopSpeaking: happy eyes, the nose "talks".
	loopSpeaking = []Frame{happy(260), fr(wide(eyeHappy, eyeHappy), false, 160), happy(320), fr(narrow(eyeHappy, eyeHappy), true, 140)}
	// loopDisconnected: sad face that sometimes nods off.
	loopDisconnected = []Frame{sad(2500), tired(300), asleep(500), tired(200)}
	// loopSleeping: "- . -" whose eyes slowly sink and rise as if breathing.
	loopSleeping = []Frame{
		fr(wide(eyeLine, eyeLine), true, 2600),
		fr(wide(eyeClosed, eyeClosed), true, 2600),
	}
	// loopMuted: eyes covered while the microphone is muted.
	loopMuted = []Frame{tired(3_600_000)}
)
