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

// Package seg7 drives the 4-digit 7-segment panel of the VK Capsula Mini
// (A113X, IS31FL3236A) through the kernel line-display driver.
//
// When every segment is exposed as an LED class device
// (/sys/class/leds/<prefix>:d<digit>:<a..g>, as on the Capsula Mini), the
// panel drives the segments directly, which allows dimming: lit segments get
// the current brightness instead of the driver's fixed maximum.
//
// Builds without the line-display driver (the stock 4.9 kernel) name the
// LEDs clock-led1..clock-led30 instead: seven per digit, segments a..g in
// order, then the colon dots. The panel then uses only the LEDs.
//
// Otherwise arbitrary segment masks are shown by temporarily remapping a few
// unused control codes in the driver's 128-byte ASCII-to-segments table
// (map_seg7) and writing those codes to message. The lower colon dot is a
// separate LED class device.
//
// Ported from the firmware's mordy utility.
package seg7

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Segment bits.
const (
	A byte = 1 << iota
	B
	C
	D
	E
	F
	G
)

// slots are control codes remapped to custom masks; printable characters
// are never touched.
var slots = [4]byte{0x1c, 0x1d, 0x1e, 0x1f}

// Panel is the display. Methods are safe for concurrent use.
type Panel struct {
	dir  string // line-display directory with message and map_seg7
	nose string // LED class directory of the lower colon dot; may be empty
	base [128]byte

	noseLevel int // nose LED brightness when on

	// segs are the LED class directories of each digit's segments a..g, if
	// the panel exposes them; then segments are driven directly.
	segs    *[4][7]string
	level   int            // segment brightness when lit
	written map[string]int // last value written per LED
	frame   [4]byte        // what is shown, for re-rendering on dimming
	frameN  bool

	calibrated bool

	mu sync.Mutex
}

// builtinMap is the ASCII to segments table of panels without a line
// display: digits, a few letters and punctuation.
var builtinMap = func() (m [128]byte) {
	for c, v := range map[byte]byte{
		'0': A | B | C | D | E | F, '1': B | C, '2': A | B | D | E | G, '3': A | B | C | D | G,
		'4': B | C | F | G, '5': A | C | D | F | G, '6': A | C | D | E | F | G, '7': A | B | C,
		'8': A | B | C | D | E | F | G, '9': A | B | C | D | F | G,
		'A': A | B | C | E | F | G, 'B': C | D | E | F | G, 'C': A | D | E | F, 'D': B | C | D | E | G,
		'E': A | D | E | F | G, 'F': A | E | F | G, 'H': B | C | E | F | G, 'L': D | E | F,
		'N': C | E | G, 'O': C | D | E | G, 'P': A | B | E | F | G, 'R': E | G, 'T': D | E | F | G,
		'U': B | C | D | E | F, 'Y': B | C | D | F | G,
		'-': G, '_': D, '=': D | G, '\'': F, '"': B | F,
	} {
		if c < 128 {
			m[c] = v
			if c >= 'A' && c <= 'Z' {
				m[c+'a'-'A'] = v
			}
		}
	}
	return m
}()

// ledsRoot is the LED class directory (a variable for tests).
var ledsRoot = "/sys/class/leds"

// findSegments looks for LED devices named "<prefix>:d1:a" .. "<prefix>:d4:g".
func findSegments() *[4][7]string {
	first, _ := filepath.Glob(filepath.Join(ledsRoot, "*:d1:a"))
	if len(first) == 0 {
		return nil
	}
	prefix := strings.TrimSuffix(first[0], "d1:a")
	var segs [4][7]string
	for d := range 4 {
		for sg := range 7 {
			path := fmt.Sprintf("%sd%d:%c", prefix, d+1, 'a'+sg)
			if _, err := os.Stat(filepath.Join(path, "brightness")); err != nil {
				return nil
			}
			segs[d][sg] = path
		}
	}
	return &segs
}

// numberedPrefix names the segment LEDs of builds without a line display.
const numberedPrefix = "clock-led"

// findNumbered looks for LEDs named clock-led1..clock-led28: digit-major,
// segments a..g in order.
func findNumbered() *[4][7]string {
	var segs [4][7]string
	for d := range 4 {
		for sg := range 7 {
			path := filepath.Join(ledsRoot, fmt.Sprintf("%s%d", numberedPrefix, d*7+sg+1))
			if _, err := os.Stat(filepath.Join(path, "brightness")); err != nil {
				return nil
			}
			segs[d][sg] = path
		}
	}
	return &segs
}

// openLEDs opens a panel driven by the segment LEDs alone.
func openLEDs(segs *[4][7]string, nose string) *Panel {
	n := findNose(nose)
	if n == "" && nose == "" {
		// The lower colon dot follows the segments.
		if path := filepath.Join(ledsRoot, numberedPrefix+"30"); fileExists(filepath.Join(path, "brightness")) {
			n = path
		}
	}
	p := &Panel{nose: n, noseLevel: 255, base: builtinMap, segs: segs, written: map[string]int{}}
	for _, digit := range segs {
		for _, path := range digit {
			writeFile(filepath.Join(path, "trigger"), []byte("none"))
		}
	}
	p.level = scaled(filepath.Join(segs[0][0], "max_brightness"), 100, 255)
	return p
}

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// Open finds and opens the panel. Empty dev and nose are auto-detected;
// nose "none" disables the dot.
func Open(dev, nose string) (*Panel, error) {
	if dev == "" {
		var err error
		if dev, err = findLinedisp(); err != nil {
			if segs := findNumbered(); segs != nil {
				return openLEDs(segs, nose), nil
			}
			return nil, err
		}
	}
	p := &Panel{dir: dev, nose: findNose(nose), noseLevel: 255}
	raw, err := os.ReadFile(filepath.Join(dev, "map_seg7"))
	if err != nil {
		return nil, fmt.Errorf("seg7: read map_seg7: %w", err)
	}
	if len(raw) < 128 {
		return nil, fmt.Errorf("seg7: map_seg7 is %d bytes, want 128", len(raw))
	}
	copy(p.base[:], raw[:128])

	if segs := findSegments(); segs != nil {
		for _, digit := range segs {
			for _, path := range digit {
				writeFile(filepath.Join(path, "trigger"), []byte("none"))
			}
		}
		if cal, ok := p.calibrate(segs); ok {
			segs = cal
		} else {
			segs = rotate180(segs)
		}
		p.segs = segs
		p.written = map[string]int{}
		p.level = scaled(filepath.Join(segs[0][0], "max_brightness"), 100, 255)
		// Blank the line display once; from now on it is not written, so it
		// never overrides the segment levels.
		writeFile(filepath.Join(dev, "map_seg7"), p.base[:])
		writeFile(filepath.Join(dev, "message"), []byte("    "))
	}
	return p, nil
}

// Calibrated reports whether the segment LED layout was measured (see
// [Panel.calibrate]) rather than assumed.
func (p *Panel) Calibrated() bool { return p.calibrated }

// calibrate learns which LED the line display lights for every digit
// position and segment: the LED names need not match the physical layout
// (on the Capsula Mini the panel is mounted upside down). It lights one
// segment at a time through the line display and reads the LEDs back.
func (p *Panel) calibrate(leds *[4][7]string) (*[4][7]string, bool) {
	var all []string
	for _, digit := range leds {
		all = append(all, digit[:]...)
	}
	lit := func() []string {
		var on []string
		for _, path := range all {
			b, _ := os.ReadFile(filepath.Join(path, "brightness"))
			if v, _ := strconv.Atoi(strings.TrimSpace(string(b))); v > 0 {
				on = append(on, path)
			}
		}
		return on
	}
	var out [4][7]string
	used := map[string]bool{}
	for pos := range 4 {
		for sg := range 7 {
			tbl := p.base
			tbl[slots[0]] = 1 << sg
			msg := []byte("    ")
			msg[pos] = slots[0]
			if writeFile(filepath.Join(p.dir, "map_seg7"), tbl[:]) != nil ||
				writeFile(filepath.Join(p.dir, "message"), msg) != nil {
				return nil, false
			}
			var on []string
			for try := 0; try < 10; try++ {
				if on = lit(); len(on) == 1 {
					break
				}
				time.Sleep(10 * time.Millisecond)
			}
			if len(on) != 1 || used[on[0]] {
				return nil, false
			}
			used[on[0]] = true
			out[pos][sg] = on[0]
		}
	}
	p.calibrated = true
	return &out, true
}

// rotate180 maps a layout to the same panel turned upside down: the digit
// order is reversed and a<->d, b<->e, c<->f.
func rotate180(l *[4][7]string) *[4][7]string {
	flip := [7]int{3, 4, 5, 0, 1, 2, 6}
	var out [4][7]string
	for d := range 4 {
		for sg := range 7 {
			out[d][sg] = l[3-d][flip[sg]]
		}
	}
	return &out
}

// Dimmable reports whether [Panel.SetBrightness] can work.
func (p *Panel) Dimmable() bool {
	if p.segs != nil {
		return true
	}
	if _, err := os.Stat(filepath.Join(p.dir, "brightness")); err == nil {
		return true
	}
	bl, _ := filepath.Glob("/sys/class/backlight/*/brightness")
	return len(bl) > 0
}

// setLED writes v to the LED at path unless it already has that value.
func (p *Panel) setLED(path string, v int) error {
	if old, ok := p.written[path]; ok && old == v {
		return nil
	}
	if err := writeFile(filepath.Join(path, "brightness"), []byte(strconv.Itoa(v))); err != nil {
		delete(p.written, path)
		return err
	}
	p.written[path] = v
	return nil
}

// render shows masks by driving the segment LEDs. p.mu must be held.
func (p *Panel) render(m [4]byte) error {
	p.frame, p.frameN = m, true
	var first error
	for d, digit := range p.segs {
		for sg, path := range digit {
			v := 0
			if m[d]&(1<<sg) != 0 {
				v = p.level
			}
			if err := p.setLED(path, v); err != nil && first == nil {
				first = fmt.Errorf("seg7: %w", err)
			}
		}
	}
	return first
}

// Dir returns the line-display directory, or the first segment LED of a
// panel without one.
func (p *Panel) Dir() string {
	if p.dir == "" && p.segs != nil {
		return p.segs[0][0]
	}
	return p.dir
}

// Rotate180 turns the layout of a panel driven by its segment LEDs upside
// down, for a panel mounted the other way round.
func (p *Panel) Rotate180() {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.segs == nil {
		return
	}
	p.segs = rotate180(p.segs)
	if p.frameN {
		p.render(p.frame)
	}
}

func findLinedisp() (string, error) {
	var found string
	stop := errors.New("stop")
	filepath.WalkDir("/sys/devices", func(path string, de fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if strings.Count(path, "/") > 12 {
			return fs.SkipDir
		}
		if !de.IsDir() && de.Name() == "map_seg7" {
			found = filepath.Dir(path)
			return stop
		}
		return nil
	})
	if found == "" {
		return "", errors.New("seg7: map_seg7 not found under /sys/devices")
	}
	return found, nil
}

func findNose(v string) string {
	switch {
	case v == "none":
		return ""
	case v != "" && strings.HasPrefix(v, "/"):
		return v
	case v != "":
		return "/sys/class/leds/" + v
	}
	ents, _ := os.ReadDir("/sys/class/leds")
	var cand []string
	for _, en := range ents {
		n := strings.ToLower(en.Name())
		if strings.Contains(n, "colon") || strings.Contains(n, "dot") {
			cand = append(cand, en.Name())
		}
	}
	sort.Strings(cand)
	for _, n := range cand {
		l := strings.ToLower(n)
		if strings.Contains(l, "low") || strings.Contains(l, "bot") {
			return "/sys/class/leds/" + n
		}
	}
	if len(cand) > 0 {
		return "/sys/class/leds/" + cand[len(cand)-1]
	}
	return ""
}

func writeFile(path string, data []byte) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_TRUNC, 0)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = f.Write(data)
	return err
}

func (p *Panel) setNose(on bool) {
	if p.nose == "" {
		return
	}
	writeFile(filepath.Join(p.nose, "trigger"), []byte("none"))
	v := "0"
	if on {
		v = strconv.Itoa(max(1, p.noseLevel))
	}
	writeFile(filepath.Join(p.nose, "brightness"), []byte(v))
}

// ErrNoBrightness is returned when the panel has no brightness control.
var ErrNoBrightness = errors.New("seg7: no brightness control found")

// SetBrightness dims the panel, per cent: the segment LEDs if they are
// exposed, otherwise a brightness attribute of the line-display device or a
// backlight class device. The nose LED is dimmed too.
func (p *Panel) SetBrightness(pct int) error {
	pct = max(1, min(100, pct))
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.nose != "" {
		p.noseLevel = scaled(filepath.Join(p.nose, "max_brightness"), pct, 255)
	}
	if p.segs != nil {
		level := scaled(filepath.Join(p.segs[0][0], "max_brightness"), pct, 255)
		if level == p.level {
			return nil
		}
		p.level = level
		if !p.frameN {
			return nil
		}
		return p.render(p.frame)
	}
	path := filepath.Join(p.dir, "brightness")
	maxPath := filepath.Join(p.dir, "max_brightness")
	if _, err := os.Stat(path); err != nil {
		bl, _ := filepath.Glob("/sys/class/backlight/*/brightness")
		if len(bl) == 0 {
			return ErrNoBrightness
		}
		path = bl[0]
		maxPath = filepath.Join(filepath.Dir(bl[0]), "max_brightness")
	}
	return writeFile(path, []byte(strconv.Itoa(scaled(maxPath, pct, 255))))
}

// scaled returns pct per cent of the value stored in maxPath (or def).
func scaled(maxPath string, pct, def int) int {
	m := def
	if b, err := os.ReadFile(maxPath); err == nil {
		if v, err := strconv.Atoi(strings.TrimSpace(string(b))); err == nil && v > 0 {
			m = v
		}
	}
	return max(1, m*pct/100)
}

// Show displays raw segment masks, one per digit, and the nose dot.
func (p *Panel) Show(m [4]byte, nose bool) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.segs != nil {
		p.setNose(nose)
		return p.render(m)
	}
	tbl := p.base
	msg := make([]byte, 4)
	for i, mask := range m {
		if mask == 0 {
			msg[i] = ' '
			continue
		}
		tbl[slots[i]] = mask & 0x7f
		msg[i] = slots[i]
	}
	if err := writeFile(filepath.Join(p.dir, "map_seg7"), tbl[:]); err != nil {
		return fmt.Errorf("seg7: write map_seg7: %w", err)
	}
	if err := writeFile(filepath.Join(p.dir, "message"), msg); err != nil {
		return fmt.Errorf("seg7: write message: %w", err)
	}
	p.setNose(nose)
	return nil
}

// Text displays up to 4 characters right-aligned using the stock table.
func (p *Panel) Text(s string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(s) > 4 {
		s = s[len(s)-4:]
	}
	if p.segs != nil {
		var m [4]byte
		for i, c := range []byte(fmt.Sprintf("%4s", s)) {
			if c < 128 {
				m[i] = p.base[c] & 0x7f
			}
		}
		p.setNose(false)
		return p.render(m)
	}
	if err := writeFile(filepath.Join(p.dir, "map_seg7"), p.base[:]); err != nil {
		return err
	}
	p.setNose(false)
	return writeFile(filepath.Join(p.dir, "message"), []byte(fmt.Sprintf("%4s", s)))
}

// Off blanks the panel and restores the stock table.
func (p *Panel) Off() {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.segs != nil {
		p.render([4]byte{})
		p.frameN = false
		p.setNose(false)
		return
	}
	writeFile(filepath.Join(p.dir, "map_seg7"), p.base[:])
	writeFile(filepath.Join(p.dir, "message"), []byte("    "))
	p.setNose(false)
}
