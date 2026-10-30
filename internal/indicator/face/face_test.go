// Copyright 2026 The Marusya-Gemini Authors.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//	https://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.
package face

import (
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/yawaflua/marusya-gemini/internal/indicator"
	"github.com/yawaflua/marusya-gemini/internal/models"
)

type fakePanel struct {
	mu  sync.Mutex
	log []string
}

func (p *fakePanel) Show(d [4]byte, nose bool) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.log = append(p.log, fmt.Sprintf("%02x%02x%02x%02x/%v", d[0], d[1], d[2], d[3], nose))
	return nil
}

func (p *fakePanel) Text(s string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.log = append(p.log, "text:"+s)
	return nil
}

func (p *fakePanel) Off() {}

func (p *fakePanel) last() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.log[len(p.log)-1]
}

func (p *fakePanel) all() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return slices.Clone(p.log)
}

func TestWideEyes(t *testing.T) {
	f := neutral(0)
	if f.Digits[1] != 0 || f.Digits[2] != 0 || f.Digits[0] == 0 || f.Digits[3] == 0 || !f.Nose {
		t.Errorf("neutral face = %v, want eyes on the outer digits with the nose", f)
	}
}

func TestStatesAndOverlay(t *testing.T) {
	p := &fakePanel{}
	f := newFace(p, nil, 100*time.Millisecond)
	defer f.Close()
	wait := func() { time.Sleep(40 * time.Millisecond) }

	wait()
	if got, want := p.last(), "63000063/true"; got != want { // neutral, idle
		t.Errorf("idle: %s, want %s", got, want)
	}
	f.Show(indicator.Listening)
	wait()
	if got, want := p.last(), "3f00003f/true"; got != want { // big round eyes
		t.Errorf("listening: %s, want %s", got, want)
	}
	f.Overlay("42", 50*time.Millisecond)
	wait()
	if got := p.last(); got != "text:42" {
		t.Errorf("overlay: %s, want text:42", got)
	}
	time.Sleep(60 * time.Millisecond)
	if got, want := p.last(), "3f00003f/true"; got != want {
		t.Errorf("after overlay: %s, want listening face %s", got, want)
	}
	f.Show(indicator.Error)
	wait()
	if got, want := p.last(), "54000054/true"; got != want { // sad
		t.Errorf("error: %s, want %s", got, want)
	}
	time.Sleep(120 * time.Millisecond)
	if got, want := p.last(), "63000063/true"; got != want {
		t.Errorf("error should revert to idle: %s, want %s\n%s", got, want, strings.Join(p.all(), " "))
	}
}

type dimPanel struct {
	fakePanel
	mu  sync.Mutex
	pct []int
}

func (p *dimPanel) SetBrightness(pct int) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.pct = append(p.pct, pct)
	return nil
}

func TestAmbient(t *testing.T) {
	for lux, want := range map[float64]int{0: 10, 10: 41, 100: 70, 5000: 100} {
		if got := BrightnessFor(lux); got != want {
			t.Errorf("BrightnessFor(%v) = %d, want %d", lux, got, want)
		}
	}

	p := &dimPanel{}
	f := newFace(p, nil, time.Second)
	defer f.Close()
	f.SetAmbient(0)
	time.Sleep(50 * time.Millisecond)
	if got, want := p.last(), "40000040/true"; got != want { // "- . -"
		t.Errorf("dark: %s, want %s", got, want)
	}
	f.SetAmbient(0.2) // same mode, no restart, same brightness bucket
	f.SetAmbient(200)
	time.Sleep(50 * time.Millisecond)
	if got, want := p.last(), "63000063/true"; got != want {
		t.Errorf("light: %s, want %s", got, want)
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if want := []int{10, 79}; !slices.Equal(p.pct, want) {
		t.Errorf("brightness = %v, want %v", p.pct, want)
	}
}

func TestOverride(t *testing.T) {
	p := &dimPanel{}
	f := newFace(p, nil, time.Second)
	defer f.Close()
	wait := func() { time.Sleep(40 * time.Millisecond) }
	f.SetAmbient(200)
	wait()

	if _, err := f.Override(models.DisplayRequest{Mode: "text", Text: "Привет"}); err == nil {
		t.Error("Cyrillic text must be rejected")
	}
	if _, err := f.Override(models.DisplayRequest{Text: "hello"}); err == nil {
		t.Error("text longer than 4 characters must be rejected")
	}
	if _, err := f.Override(models.DisplayRequest{Text: "HI", Brightness: 30, Hold: time.Second}); err != nil {
		t.Fatal(err)
	}
	wait()
	if got := p.last(); got != "text:HI" {
		t.Errorf("text override: %s, want text:HI", got)
	}
	f.Show(indicator.Listening)
	wait()
	if got, want := p.last(), "3f00003f/true"; got != want {
		t.Errorf("listening over the override: %s, want %s", got, want)
	}
	f.Show(indicator.Idle)
	wait()
	if got := p.last(); got != "text:HI" {
		t.Errorf("back to the override: %s, want text:HI", got)
	}
	time.Sleep(time.Second)
	if got, want := p.last(), "63000063/true"; got != want {
		t.Errorf("after expiry: %s, want the idle face %s\n%s", got, want, strings.Join(p.all(), " "))
	}
	p.mu.Lock()
	if want := []int{79, 30, 79}; !slices.Equal(p.pct, want) {
		t.Errorf("brightness = %v, want %v (sensor, override, sensor again)", p.pct, want)
	}
	p.mu.Unlock()

	f.Override(models.DisplayRequest{Mode: "sleep"})
	wait()
	if got, want := p.last(), "40000040/true"; got != want || !f.Asleep() {
		t.Errorf("forced sleep: %s asleep=%v, want %s", got, f.Asleep(), want)
	}
	f.SetAmbient(0)
	f.Override(models.DisplayRequest{Mode: "wake"})
	wait()
	if got, want := p.last(), "63000063/true"; got != want || f.Asleep() {
		t.Errorf("forced awake in the dark: %s asleep=%v, want %s", got, f.Asleep(), want)
	}
	f.ResetOverride()
	wait()
	if got, want := p.last(), "40000040/true"; got != want {
		t.Errorf("reset in the dark: %s, want sleeping %s", got, want)
	}

	f.Override(models.DisplayRequest{Mode: "clock"})
	wait()
	if got, want := p.last(), "text:"+time.Now().Format("1504"); got != want {
		t.Errorf("clock: %s, want %s", got, want)
	}
	f.Override(models.DisplayRequest{Mode: "off"})
	wait()
	if got, want := p.last(), "00000000/false"; got != want {
		t.Errorf("off: %s, want %s", got, want)
	}
}

func TestOverrideNotDimmable(t *testing.T) {
	f := newFace(&fakePanel{}, nil, time.Second)
	defer f.Close()
	if _, err := f.Override(models.DisplayRequest{Mode: "face", Brightness: 50}); err != ErrNotDimmable {
		t.Errorf("err = %v, want ErrNotDimmable", err)
	}
}
