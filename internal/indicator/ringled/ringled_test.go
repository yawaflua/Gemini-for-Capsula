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
	"bytes"
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/yawaflua/marusya-gemini/internal/indicator"
)

// TestEncodeMatchesFirmware compares with `ringled --dump -b 100 …` output
// captured on the speaker.
func TestEncodeMatchesFirmware(t *testing.T) {
	red, green, blue := RGB{R: 0xff}, RGB{G: 0xff}, RGB{B: 0xff}
	cases := []struct {
		name string
		f    Frame
		on   []int // data bytes (after the reset) that are 0xcc
	}{
		{"set 0 ff0000", Frame{0: red}, []int{0, 1, 2, 3}},
		{"set 0 00ff00", Frame{0: green}, []int{4, 5, 6, 7}},
		{"set 0 0000ff", Frame{0: blue}, []int{12, 13, 14, 15}},
		{"set 1 ff0000", Frame{1: red}, []int{20, 21, 22, 23}},
		{"raw ff0000 00ff00", Frame{0: red, 1: green}, []int{0, 1, 2, 3, 8, 9, 10, 11}},
	}
	for _, c := range cases {
		buf := encode(c.f)
		if len(buf) != 400 {
			t.Fatalf("%s: %d bytes, want 400", c.name, len(buf))
		}
		if !bytes.Equal(buf[:resetBytes], make([]byte, resetBytes)) || !bytes.Equal(buf[400-resetBytes:], make([]byte, resetBytes)) {
			t.Errorf("%s: reset bytes are not zero", c.name)
		}
		want := bytes.Repeat([]byte{0x88}, Pixels*12)
		for _, i := range c.on {
			want[i] = 0xcc
		}
		if got := buf[resetBytes : resetBytes+Pixels*12]; !bytes.Equal(got, want) {
			t.Errorf("%s:\n got % x\nwant % x", c.name, got[:24], want[:24])
		}
	}
	// Mixed bits: 0x1b = 00 01 10 11 → 88 8c c8 cc, then the word swap.
	buf := encode(Frame{0: {G: 0x1b}})[resetBytes:]
	if want := []byte{0x88, 0x88, 0x88, 0x88, 0xcc, 0xc8, 0x8c, 0x88}; !bytes.Equal(buf[:8], want) {
		t.Errorf("mixed bits: % x, want % x", buf[:8], want)
	}
}

func TestMuteSweepsFromKey(t *testing.T) {
	mute := look{kind: "muted", color: colorError, bright: 100}
	sc := transition(look{kind: "off"}, mute, colors{})
	c, moving := sc.at(100 * time.Millisecond)
	f := c.frame()
	if !moving || f[0].R == 0 || f[1].R == 0 || f[6].R != 0 {
		t.Errorf("at 100ms: %v; want pixel 0 and 1 lit, 6 dark", f)
	}
	for i := 1; i < Pixels/2; i++ {
		if f[i] != f[Pixels-i] {
			t.Errorf("pixels %d and %d differ: %v %v", i, Pixels-i, f[i], f[Pixels-i])
		}
		if f[i].R > f[i-1].R {
			t.Errorf("pixel %d is ahead of %d", i, i-1)
		}
	}
	c, moving = sc.at(time.Second)
	if moving || c.frame() != fill(scaled(colorError, 100, 1)).frame() {
		t.Errorf("at 1s: %v moving=%v; want all red", c.frame(), moving)
	}

	// Unmuting withdraws into the key: the far side goes dark first.
	sc = transition(mute, look{kind: "off"}, fill(scaled(colorError, 100, 1)))
	f = must(sc.at(170 * time.Millisecond)).frame()
	if f[6].R != 0 || f[0].R != 255 {
		t.Errorf("unmute at 170ms: %v; want pixel 6 dark, 0 still red", f)
	}
}

func TestVolumeArc(t *testing.T) {
	f := volumeArc(look{kind: "volume", bright: 100, level: 50}).frame()
	for i, px := range f {
		lit := px.R == 255
		if want := dist(i) <= 2 || i == 3; lit != want { // 0,1,11,2,10,3
			t.Errorf("volume 50: pixel %d lit=%v", i, lit)
		}
	}
	f = volumeArc(look{kind: "volume", bright: 100, level: 50, muted: true}).frame()
	if f[0] != colorError || f[1] != (RGB{}) {
		t.Errorf("muted volume: %v", f)
	}
}

func TestSpinnerMoves(t *testing.T) {
	sc := spinner(colorThinking, 100, 1200*time.Millisecond)
	a, moving := sc.at(0)
	b, _ := sc.at(300 * time.Millisecond) // a quarter turn
	if !moving || a.frame()[0] != b.frame()[3] {
		t.Errorf("head at 0: %v, at 3 after a quarter turn: %v", a.frame()[0], b.frame()[3])
	}
}

func must(c colors, _ bool) colors { return c }

type fakeStrip struct {
	mu     sync.Mutex
	frames []Frame
}

func (s *fakeStrip) Write(f Frame) error {
	s.mu.Lock()
	s.frames = append(s.frames, f)
	s.mu.Unlock()
	return nil
}

// settled waits until the ring stops changing and returns the last frame.
func (s *fakeStrip) settled(t *testing.T) Frame {
	t.Helper()
	n := -1
	for deadline := time.Now().Add(3 * time.Second); time.Now().Before(deadline); time.Sleep(60 * time.Millisecond) {
		s.mu.Lock()
		cur := len(s.frames)
		s.mu.Unlock()
		if cur == n && cur > 0 {
			break
		}
		n = cur
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.frames[len(s.frames)-1]
}

func TestRingPriorities(t *testing.T) {
	s := &fakeStrip{}
	r := (&Ring{strip: s, brightness: 30, log: slog.Default(), errorHold: 600 * time.Millisecond, interval: 5 * time.Millisecond}).start()
	all := func(c RGB, b int) Frame { return fill(scaled(c, b, 1)).frame() }
	night, _ := ParseRGB(DefaultNightLight)
	ctx := context.Background()
	steps := []struct {
		name string
		do   func()
		want Frame
	}{
		{"idle", func() {}, Frame{}},
		{"night light", func() { r.SetNightLight(ctx, true, "", 0) }, all(night, 30)},
		{"listening wins", func() { r.Show(indicator.Listening) }, all(colorListening, 30)},
		{"back to night light", func() { r.Show(indicator.Idle) }, all(night, 30)},
		{"muted wins over night light", func() { r.SetMuted(true) }, all(colorError, 30)},
		{"error", func() { r.Show(indicator.Error) }, all(colorError, 30)},
		{"night light again", func() { time.Sleep(700 * time.Millisecond); r.SetMuted(false) }, all(night, 30)},
		{"custom colour", func() { r.SetNightLight(ctx, true, "#FF0080", 80) }, all(RGB{0xff, 0x00, 0x80}, 80)},
		{"off", func() { r.SetNightLight(ctx, false, "", 0) }, Frame{}},
	}
	for _, st := range steps {
		st.do()
		if got := s.settled(t); got != st.want {
			t.Errorf("%s: %v, want %v", st.name, got, st.want)
		}
	}
	if err := r.SetNightLight(ctx, true, "yellow", 0); err == nil {
		t.Error("colour names must be rejected")
	}

	r.SetNightLight(ctx, true, "", 0)
	s.settled(t)
	r.Close()
	if got := s.frames[len(s.frames)-1]; got != (Frame{}) {
		t.Errorf("after Close: %v, want off", got)
	}
}

func TestDetect(t *testing.T) {
	dir := t.TempDir()
	defer func(c, d string) { compatiblePath, spiDriver = c, d }(compatiblePath, spiDriver)
	compatiblePath = filepath.Join(dir, "compatible")
	spiDriver = filepath.Join(dir, "meson-spicc")

	for _, tc := range []struct {
		compatible string
		driver     bool
		want       bool
	}{
		{"vk,capsula-mini\x00amlogic,a113d\x00amlogic,meson-axg\x00", true, true},
		{"vk,capsula-mini\x00amlogic,a113d\x00", false, false},
		{"amlogic,s400\x00amlogic,a113d\x00amlogic,meson-axg\x00", true, false},
		{"", true, false},
	} {
		os.RemoveAll(spiDriver)
		if tc.driver {
			os.Mkdir(spiDriver, 0o755)
		}
		os.WriteFile(compatiblePath, []byte(tc.compatible), 0o644)
		if got := Detect(); got != tc.want {
			t.Errorf("Detect() with %q, driver %v = %v, want %v", tc.compatible, tc.driver, got, tc.want)
		}
	}
}
