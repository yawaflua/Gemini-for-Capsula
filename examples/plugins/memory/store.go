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

package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

// Fact is one remembered thing.
type Fact struct {
	ID   int    `json:"id"`
	Text string `json:"text"`
	// Device is the speaker it was said to; empty if added in the panel.
	Device string    `json:"device,omitempty"`
	Added  time.Time `json:"added"`
}

// Store keeps the facts in a JSON file.
type Store struct {
	path    string
	maxN    int
	maxLen  int
	mu      sync.Mutex
	facts   []Fact
	nextID  int
	nowFunc func() time.Time
}

type storeFile struct {
	NextID int    `json:"next_id"`
	Facts  []Fact `json:"facts"`
}

// Open reads the store at path; a missing file is an empty store.
func Open(path string, maxN, maxLen int) (*Store, error) {
	s := &Store{path: path, maxN: maxN, maxLen: maxLen, nextID: 1, nowFunc: time.Now}
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return s, nil
	}
	if err != nil {
		return nil, err
	}
	var f storeFile
	if err := json.Unmarshal(b, &f); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	s.facts = f.Facts
	s.nextID = max(f.NextID, 1)
	for _, x := range s.facts {
		s.nextID = max(s.nextID, x.ID+1)
	}
	return s, nil
}

// Facts returns the facts, oldest first.
func (s *Store) Facts() []Fact {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.facts)
}

// Add remembers text. A fact already known is returned as is.
func (s *Store) Add(text, device string) (Fact, error) {
	text = strings.Join(strings.Fields(text), " ")
	if text == "" {
		return Fact{}, errors.New("empty fact")
	}
	if n := utf8.RuneCountInString(text); n > s.maxLen {
		return Fact{}, fmt.Errorf("fact is %d characters, at most %d: say it shorter", n, s.maxLen)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, x := range s.facts {
		if strings.EqualFold(x.Text, text) {
			return x, nil
		}
	}
	if len(s.facts) >= s.maxN {
		return Fact{}, fmt.Errorf("memory is full (%d facts): forget something first", s.maxN)
	}
	f := Fact{ID: s.nextID, Text: text, Device: device, Added: s.nowFunc().UTC().Truncate(time.Second)}
	s.facts = append(s.facts, f)
	s.nextID++
	if err := s.save(); err != nil {
		s.facts = s.facts[:len(s.facts)-1]
		return Fact{}, err
	}
	return f, nil
}

// Remove forgets the fact with the given ID.
func (s *Store) Remove(id int) (Fact, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	i := slices.IndexFunc(s.facts, func(x Fact) bool { return x.ID == id })
	if i < 0 {
		return Fact{}, fmt.Errorf("no fact #%d", id)
	}
	old := slices.Clone(s.facts)
	f := s.facts[i]
	s.facts = slices.Delete(s.facts, i, i+1)
	if err := s.save(); err != nil {
		s.facts = old
		return Fact{}, err
	}
	return f, nil
}

// Find returns the facts containing text, case-insensitively.
func (s *Store) Find(text string) []Fact {
	text = strings.ToLower(strings.Join(strings.Fields(text), " "))
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []Fact
	for _, x := range s.facts {
		if text != "" && strings.Contains(strings.ToLower(x.Text), text) {
			out = append(out, x)
		}
	}
	return out
}

// Clear forgets everything.
func (s *Store) Clear() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	old := s.facts
	s.facts = nil
	if err := s.save(); err != nil {
		s.facts = old
		return err
	}
	return nil
}

// save writes the file atomically; s.mu is held.
func (s *Store) save() error {
	b, err := json.MarshalIndent(storeFile{NextID: s.nextID, Facts: s.facts}, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(s.path), ".memory-*.json")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(append(b, '\n')); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), s.path)
}

// Instructions is what the plugin adds to the system instruction.
func Instructions(facts []Fact) string {
	if len(facts) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("Постоянная память. Это ты знаешь о своих пользователях из прошлых разговоров. " +
		"Учитывай это в ответах, но не пересказывай без просьбы. Если узнаёшь что-то новое и важное " +
		"надолго, сохрани через memory_save; если факт устарел или его просят забыть, удали через " +
		"memory_forget по номеру.\n")
	for _, f := range facts {
		fmt.Fprintf(&b, "[%d] %s\n", f.ID, f.Text)
	}
	return strings.TrimSpace(b.String())
}
