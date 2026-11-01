// Copyright 2026 The Marusya-Gemini Authors.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//	http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package openaichat

import (
	"strings"
	"testing"
)

func TestFastPathRegexPhrases(t *testing.T) {
	cases := []string{
		"Включи сцену Спокойной ночи",
		"Поставь яркость торшера 50",
		"Запусти сцену Утро",
		"Без звука",
	}
	for _, c := range cases {
		if !commandRE.MatchString(strings.ToLower(c)) {
			t.Errorf("commandRE does not match %q", c)
		}
	}
}

func TestDateRE(t *testing.T) {
	for _, c := range []string{"какое сегодня число", "какая дата", "какой сегодня день", "какой день недели", "какое число"} {
		if !dateRE.MatchString(c) {
			t.Errorf("dateRE does not match %q", c)
		}
	}
	for _, c := range []string{"какая погода", "включи свет", "сколько времени"} {
		if dateRE.MatchString(c) {
			t.Errorf("dateRE matches %q", c)
		}
	}
}

func TestBrightnessRE(t *testing.T) {
	m := brightnessDeviceRE.FindStringSubmatch("поставь яркость торшера 50")
	if m == nil || m[1] != "торшера" || m[2] != "50" {
		t.Fatalf("brightnessDeviceRE = %v", m)
	}
	m = brightnessRoomRE.FindStringSubmatch("яркость в зале 40")
	if m == nil || m[1] != "зале" || m[2] != "40" {
		t.Fatalf("brightnessRoomRE = %v", m)
	}
	m = brightnessPlaceRE.FindStringSubmatch("яркость 30")
	if m == nil || m[1] != "30" || m[2] != "" {
		t.Fatalf("brightnessPlaceRE без комнаты = %v", m)
	}
}

func TestSceneRE(t *testing.T) {
	m := sceneRE.FindStringSubmatch("включи сцену Спокойной ночи")
	if m == nil || m[2] != "Спокойной ночи" {
		t.Fatalf("sceneRE = %v", m)
	}
	if sceneRE.MatchString("включи свет") {
		t.Fatal("sceneRE matches «включи свет»")
	}
}

func TestVolumeDelta(t *testing.T) {
	cases := map[string]int{
		"сделай громче":         10,
		"сделай громче на 20":   20,
		"тише на 5":             5,
		"прибавь на 0":          10,
		"громче на 100":         50,
		"убавь на 30 процентов": 30,
	}
	for text, want := range cases {
		if got := volumeDelta(text); got != want {
			t.Errorf("volumeDelta(%q) = %d, want %d", text, got, want)
		}
	}
}

func TestStripWakeWord(t *testing.T) {
	if got := stripWakeWord("маруся, который час"); got != "который час" {
		t.Errorf("stripWakeWord = %q", got)
	}
	if got := stripWakeWord("алиса, который час"); got != "алиса, который час" {
		t.Errorf("stripWakeWord must keep «алиса», got %q", got)
	}
}

func TestDateReply(t *testing.T) {
	got := dateReply(map[string]any{"date": "2026-11-01", "weekday": "Saturday"})
	if got != "Сегодня суббота, 1 ноября." {
		t.Errorf("dateReply = %q", got)
	}
	if got := dateReply(map[string]any{"date": "bad"}); got != "Не получилось узнать дату." {
		t.Errorf("dateReply(bad) = %q", got)
	}
}
