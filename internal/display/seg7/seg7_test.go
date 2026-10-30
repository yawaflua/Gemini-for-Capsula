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

package seg7

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fakeSysfs builds a line display and Capsula-style segment LEDs.
func fakeSysfs(t *testing.T) (disp, leds string) {
	t.Helper()
	root := t.TempDir()
	disp = filepath.Join(root, "linedisp.0")
	leds = filepath.Join(root, "leds")
	os.MkdirAll(disp, 0o755)
	tbl := make([]byte, 128)
	tbl['8'] = 0x7f
	tbl['1'] = B | C
	os.WriteFile(filepath.Join(disp, "map_seg7"), tbl, 0o644)
	os.WriteFile(filepath.Join(disp, "message"), nil, 0o644)
	names := []string{"clock:colon:bottom", "clock:colon:top"}
	for d := 1; d <= 4; d++ {
		for c := 'a'; c <= 'g'; c++ {
			names = append(names, fmt.Sprintf("clock:d%d:%c", d, c))
		}
	}
	for _, n := range names {
		dir := filepath.Join(leds, n)
		os.MkdirAll(dir, 0o755)
		for f, v := range map[string]string{"brightness": "0", "max_brightness": "255", "trigger": ""} {
			os.WriteFile(filepath.Join(dir, f), []byte(v), 0o644)
		}
	}
	old := ledsRoot
	ledsRoot = leds
	t.Cleanup(func() { ledsRoot = old })
	return disp, leds
}

func level(t *testing.T, leds, name string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(leds, name, "brightness"))
	if err != nil {
		t.Fatal(err)
	}
	return strings.TrimSpace(string(b))
}

func TestSegmentLEDsDimming(t *testing.T) {
	disp, leds := fakeSysfs(t)
	p, err := Open(disp, filepath.Join(leds, "clock:colon:bottom"))
	if err != nil {
		t.Fatal(err)
	}
	if !p.Dimmable() {
		t.Fatal("panel with segment LEDs must be dimmable")
	}
	if err := p.Show([4]byte{A | G, 0, 0, D}, true); err != nil {
		t.Fatal(err)
	}
	// Without a real line display driver calibration fails and the panel
	// is assumed to be upside down: digit 1 is d4, segment a is d.
	if p.Calibrated() {
		t.Fatal("calibration must fail without a driver")
	}
	for name, want := range map[string]string{
		"clock:d4:d": "255", "clock:d4:g": "255", "clock:d4:e": "0",
		"clock:d1:a": "255", "clock:d3:d": "0", "clock:colon:bottom": "255",
	} {
		if got := level(t, leds, name); got != want {
			t.Errorf("%s = %s, want %s", name, got, want)
		}
	}

	// Dimming re-renders the current frame.
	if err := p.SetBrightness(10); err != nil {
		t.Fatal(err)
	}
	if got := level(t, leds, "clock:d4:d"); got != "25" {
		t.Errorf("dimmed segment = %s, want 25", got)
	}
	if got := level(t, leds, "clock:d4:e"); got != "0" {
		t.Errorf("unlit segment = %s, want 0", got)
	}

	// Text uses the stock table at the current level.
	if err := p.Text("18"); err != nil {
		t.Fatal(err)
	}
	if got := level(t, leds, "clock:d2:e"); got != "25" {
		t.Errorf("'1' segment b = %s, want 25", got)
	}
	if got := level(t, leds, "clock:d2:d"); got != "0" {
		t.Errorf("'1' segment a = %s, want 0", got)
	}
	if got := level(t, leds, "clock:d1:g"); got != "25" {
		t.Errorf("'8' segment g = %s, want 25", got)
	}
	// The line display is never written with content.
	if b, _ := os.ReadFile(filepath.Join(disp, "message")); strings.TrimSpace(string(b)) != "" {
		t.Errorf("message = %q, want blank", b)
	}

	p.Off()
	if got := level(t, leds, "clock:d1:g"); got != "0" {
		t.Errorf("after Off = %s, want 0", got)
	}
}

func TestNoSegmentLEDs(t *testing.T) {
	disp, leds := fakeSysfs(t)
	os.RemoveAll(filepath.Join(leds, "clock:d3:c"))
	p, err := Open(disp, "none")
	if err != nil {
		t.Fatal(err)
	}
	if p.segs != nil {
		t.Fatal("incomplete segment set must fall back to the line display")
	}
	if err := p.SetBrightness(50); err != ErrNoBrightness {
		t.Errorf("SetBrightness = %v, want ErrNoBrightness", err)
	}
}

// TestNumberedLEDs covers builds without a line display, whose LEDs are
// clock-led1..clock-led30: seven per digit, then the colon dots.
func TestNumberedLEDs(t *testing.T) {
	leds := filepath.Join(t.TempDir(), "leds")
	for i := 1; i <= 30; i++ {
		dir := filepath.Join(leds, fmt.Sprintf("clock-led%d", i))
		os.MkdirAll(dir, 0o755)
		for f, v := range map[string]string{"brightness": "0", "max_brightness": "255", "trigger": ""} {
			os.WriteFile(filepath.Join(dir, f), []byte(v), 0o644)
		}
	}
	old := ledsRoot
	ledsRoot = leds
	defer func() { ledsRoot = old }()

	p, err := Open("", "")
	if err != nil {
		t.Fatal(err)
	}
	if !p.Dimmable() {
		t.Error("numbered panel must be dimmable")
	}
	// Digit 1 segment a is clock-led1, digit 2 segment g is clock-led14,
	// the nose is clock-led30.
	if err := p.Show([4]byte{A, G, 0, 0}, true); err != nil {
		t.Fatal(err)
	}
	for name, want := range map[string]string{
		"clock-led1": "255", "clock-led14": "255", "clock-led2": "0", "clock-led30": "255", "clock-led29": "0",
	} {
		if got := level(t, leds, name); got != want {
			t.Errorf("%s = %s, want %s", name, got, want)
		}
	}

	// Text uses the built-in table: "1" lights b and c of the last digit.
	if err := p.Text("1"); err != nil {
		t.Fatal(err)
	}
	for name, want := range map[string]string{"clock-led23": "255", "clock-led24": "255", "clock-led22": "0", "clock-led1": "0"} {
		if got := level(t, leds, name); got != want {
			t.Errorf("after Text: %s = %s, want %s", name, got, want)
		}
	}

	// Upside down, the last digit's b and c are the first digit's e and f.
	p.Rotate180()
	for name, want := range map[string]string{"clock-led5": "255", "clock-led6": "255", "clock-led23": "0"} {
		if got := level(t, leds, name); got != want {
			t.Errorf("rotated: %s = %s, want %s", name, got, want)
		}
	}
}
