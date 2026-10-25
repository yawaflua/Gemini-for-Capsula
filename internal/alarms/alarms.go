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

// Package alarms keeps the alarms and timers of every speaker and computes
// when they ring. The server is the source of truth (speakers run from RAM);
// speakers get the upcoming rings (see models.TypeAlarms) and ring on their
// own.
package alarms

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"
)

// MaxPerDevice bounds the number of alarms of one speaker.
const MaxPerDevice = 30

// Alarm is an alarm clock or a timer.
type Alarm struct {
	ID string `json:"id"`
	// Time is the local time of day, "HH:MM" (or "HH:MM:SS" for timers).
	Time string `json:"time"`
	// Days makes the alarm repeat on these weekdays. Empty: rings once, on
	// Date.
	Days []time.Weekday `json:"days,omitempty"`
	// Date is the local date of a one-time alarm, "YYYY-MM-DD".
	Date string `json:"date,omitempty"`
	// Label is what the alarm is for; "таймер" for timers.
	Label string `json:"label,omitempty"`
	// Music is a search query to wake up to instead of beeps.
	Music string `json:"music,omitempty"`
	// Volume is the speaker volume while ringing, 1..100; 0 keeps the
	// current volume.
	Volume int `json:"volume,omitempty"`
	// Off disables the alarm without deleting it.
	Off bool `json:"off,omitempty"`
}

// Repeats reports whether the alarm rings on more than one day.
func (a Alarm) Repeats() bool { return len(a.Days) > 0 }

func (a Alarm) clock() (h, m, sec int, err error) {
	t, err := time.Parse("15:04", a.Time)
	if err != nil {
		if t, err = time.Parse("15:04:05", a.Time); err != nil {
			return 0, 0, 0, fmt.Errorf("alarms: bad time %q, want HH:MM", a.Time)
		}
	}
	return t.Hour(), t.Minute(), t.Second(), nil
}

// Validate checks the alarm.
func (a Alarm) Validate() error {
	if _, _, _, err := a.clock(); err != nil {
		return err
	}
	if !a.Repeats() {
		if _, err := time.Parse(time.DateOnly, a.Date); err != nil {
			return fmt.Errorf("alarms: a one-time alarm needs a date: %q", a.Date)
		}
	}
	for _, d := range a.Days {
		if d < time.Sunday || d > time.Saturday {
			return fmt.Errorf("alarms: bad weekday %d", d)
		}
	}
	if a.Volume < 0 || a.Volume > 100 {
		return fmt.Errorf("alarms: bad volume %d", a.Volume)
	}
	if len(a.Label) > 100 || len(a.Music) > 200 {
		return errors.New("alarms: label or music is too long")
	}
	return nil
}

// Rings returns the times the alarm rings in [from, until) in loc.
func (a Alarm) Rings(from, until time.Time, loc *time.Location) []time.Time {
	if a.Off {
		return nil
	}
	h, m, sec, err := a.clock()
	if err != nil {
		return nil
	}
	if !a.Repeats() {
		d, err := time.ParseInLocation(time.DateOnly, a.Date, loc)
		if err != nil {
			return nil
		}
		t := time.Date(d.Year(), d.Month(), d.Day(), h, m, sec, 0, loc)
		if !t.Before(from) && t.Before(until) {
			return []time.Time{t}
		}
		return nil
	}
	var out []time.Time
	f := from.In(loc)
	for day := time.Date(f.Year(), f.Month(), f.Day(), 0, 0, 0, 0, loc); day.Before(until); day = day.AddDate(0, 0, 1) {
		t := time.Date(day.Year(), day.Month(), day.Day(), h, m, sec, 0, loc)
		if slices.Contains(a.Days, t.Weekday()) && !t.Before(from) && t.Before(until) {
			out = append(out, t)
		}
	}
	return out
}

// Next returns the next ring after from, if any within a week.
func (a Alarm) Next(from time.Time, loc *time.Location) (time.Time, bool) {
	r := a.Rings(from, from.AddDate(0, 0, 8), loc)
	if len(r) == 0 {
		return time.Time{}, false
	}
	return r[0], true
}

// Expired reports whether a one-time alarm has rung (before now).
func (a Alarm) Expired(now time.Time, loc *time.Location) bool {
	if a.Repeats() {
		return false
	}
	h, m, sec, err := a.clock()
	if err != nil {
		return true
	}
	d, err := time.ParseInLocation(time.DateOnly, a.Date, loc)
	if err != nil {
		return true
	}
	return time.Date(d.Year(), d.Month(), d.Day(), h, m, sec, 0, loc).Before(now.Add(-time.Minute))
}

// Store keeps the alarms of every speaker, optionally in a JSON file.
type Store struct {
	path string
	// OnChange is called after the alarms of a device changed.
	OnChange func(device string)

	mu sync.Mutex
	m  map[string][]Alarm
}

// Open loads the store from path; empty keeps it in memory. A missing file
// is an empty store.
func Open(path string) (*Store, error) {
	s := &Store{path: path, m: map[string][]Alarm{}}
	if path == "" {
		return s, nil
	}
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return s, nil
	}
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(b, &s.m); err != nil {
		return nil, fmt.Errorf("alarms: %s: %w", path, err)
	}
	return s, nil
}

// List returns the alarms of device.
func (s *Store) List(device string) []Alarm {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.m[device])
}

// Devices returns the devices that have alarms.
func (s *Store) Devices() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]string, 0, len(s.m))
	for d := range s.m {
		out = append(out, d)
	}
	slices.Sort(out)
	return out
}

// Add stores a new alarm and returns it with its ID.
func (s *Store) Add(device string, a Alarm) (Alarm, error) {
	a.Label, a.Music = strings.TrimSpace(a.Label), strings.TrimSpace(a.Music)
	slices.Sort(a.Days)
	a.Days = slices.Compact(a.Days)
	if a.Repeats() {
		a.Date = ""
	}
	if err := a.Validate(); err != nil {
		return Alarm{}, err
	}
	b := make([]byte, 4)
	rand.Read(b)
	a.ID = hex.EncodeToString(b)
	err := s.update(device, func(list []Alarm) ([]Alarm, error) {
		if len(list) >= MaxPerDevice {
			return nil, fmt.Errorf("alarms: at most %d alarms per speaker", MaxPerDevice)
		}
		return append(list, a), nil
	})
	return a, err
}

// Set replaces the alarm with the same ID.
func (s *Store) Set(device string, a Alarm) error {
	if err := a.Validate(); err != nil {
		return err
	}
	return s.update(device, func(list []Alarm) ([]Alarm, error) {
		i := slices.IndexFunc(list, func(x Alarm) bool { return x.ID == a.ID })
		if i < 0 {
			return nil, fmt.Errorf("alarms: no alarm %q", a.ID)
		}
		list[i] = a
		return list, nil
	})
}

// Delete removes the alarms with the given IDs, or all with none, and
// returns how many were removed.
func (s *Store) Delete(device string, ids ...string) (int, error) {
	n := 0
	err := s.update(device, func(list []Alarm) ([]Alarm, error) {
		out := list[:0]
		for _, a := range list {
			if len(ids) == 0 || slices.Contains(ids, a.ID) {
				n++
				continue
			}
			out = append(out, a)
		}
		return out, nil
	})
	return n, err
}

// Prune deletes the one-time alarms of device that have rung.
func (s *Store) Prune(device string, now time.Time, loc *time.Location) {
	s.mu.Lock()
	expired := false
	for _, a := range s.m[device] {
		expired = expired || a.Expired(now, loc)
	}
	s.mu.Unlock()
	if !expired {
		return
	}
	s.update(device, func(list []Alarm) ([]Alarm, error) {
		return slices.DeleteFunc(list, func(a Alarm) bool { return a.Expired(now, loc) }), nil
	})
}

func (s *Store) update(device string, f func([]Alarm) ([]Alarm, error)) error {
	s.mu.Lock()
	list, err := f(slices.Clone(s.m[device]))
	if err != nil {
		s.mu.Unlock()
		return err
	}
	prev := s.m[device]
	if len(list) == 0 {
		delete(s.m, device)
	} else {
		s.m[device] = list
	}
	if err := s.save(); err != nil {
		if prev == nil {
			delete(s.m, device)
		} else {
			s.m[device] = prev
		}
		s.mu.Unlock()
		return err
	}
	s.mu.Unlock()
	if s.OnChange != nil {
		s.OnChange(device)
	}
	return nil
}

// save writes the file atomically. Called with mu held.
func (s *Store) save() error {
	if s.path == "" {
		return nil
	}
	b, err := json.MarshalIndent(s.m, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(s.path), ".alarms-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(b); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), s.path)
}
