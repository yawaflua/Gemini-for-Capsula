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

package server

import (
	"strings"
	"testing"
)

func TestExtraLanguages(t *testing.T) {
	s := Settings{Language: "ru-RU", ExtraLanguages: " he-IL, ru-ru ,HE-il, en-US ,"}
	if err := s.validate(false); err != nil {
		t.Fatal(err)
	}
	if s.ExtraLanguages != "he-IL, en-US" {
		t.Errorf("normalized to %q", s.ExtraLanguages)
	}
	if got := extraLanguages("ru-RU", s.ExtraLanguages); strings.Join(got, "|") != "he-IL|en-US" {
		t.Errorf("extraLanguages = %v", got)
	}
	if n := languagesNote("ru-RU", s.ExtraLanguages); !strings.Contains(n, "ru-RU, he-IL, en-US") {
		t.Errorf("note = %q", n)
	}
	if languagesNote("ru-RU", "") != "" {
		t.Error("no note without extra languages")
	}
	bad := Settings{Language: "ru-RU", ExtraLanguages: "иврит"}
	if err := bad.validate(false); err == nil {
		t.Error("a language name instead of a code must be rejected")
	}
}
