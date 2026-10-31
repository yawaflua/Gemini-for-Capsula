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
	"path/filepath"
	"strings"
	"testing"
)

func TestStore(t *testing.T) {
	path := filepath.Join(t.TempDir(), "memory.json")
	s, err := Open(path, 3, 30)
	if err != nil {
		t.Fatal(err)
	}
	if Instructions(s.Facts()) != "" {
		t.Error("instructions of an empty store")
	}
	a, err := s.Add("  Пользователя   зовут Дима ", "kitchen")
	if err != nil || a.ID != 1 || a.Text != "Пользователя зовут Дима" || a.Device != "kitchen" {
		t.Fatalf("add = %+v, %v", a, err)
	}
	if again, _ := s.Add("пользователя зовут дима", ""); again.ID != 1 || len(s.Facts()) != 1 {
		t.Errorf("duplicate = %+v, %d facts", again, len(s.Facts()))
	}
	if _, err := s.Add(strings.Repeat("я", 31), ""); err == nil {
		t.Error("too long fact accepted")
	}
	s.Add("Кота зовут Барсик", "")
	s.Add("Дима любит джаз", "")
	if _, err := s.Add("Четвёртый факт", ""); err == nil {
		t.Error("full store accepted a fact")
	}
	if got := s.Find("дима"); len(got) != 2 {
		t.Errorf("find = %+v", got)
	}
	if _, err := s.Remove(2); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Remove(2); err == nil {
		t.Error("removed a missing fact")
	}

	// The file keeps the facts and the numbering.
	s, err = Open(path, 3, 30)
	if err != nil {
		t.Fatal(err)
	}
	f, _ := s.Add("Кошку зовут Мурка", "")
	if f.ID != 4 || len(s.Facts()) != 3 {
		t.Errorf("after reopen: %+v, %+v", f, s.Facts())
	}
	in := Instructions(s.Facts())
	if !strings.Contains(in, "[1] Пользователя зовут Дима\n[3] Дима любит джаз\n[4] Кошку зовут Мурка") {
		t.Errorf("instructions = %q", in)
	}
	if err := s.Clear(); err != nil || len(s.Facts()) != 0 {
		t.Errorf("clear: %v, %+v", err, s.Facts())
	}
}
