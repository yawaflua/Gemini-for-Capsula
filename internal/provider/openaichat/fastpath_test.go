package openaichat

import (
	"strings"
	"testing"
)

func TestFastPathRegex(t *testing.T) {
	cases := []string{
		"Сделай громче",
		"Поставь громкость 40",
		"Следующий трек",
		"Поставь на паузу",
		"Включи звук",
		"Выключи музыку",
	}
	for _, c := range cases {
		if !commandRE.MatchString(strings.ToLower(c)) {
			t.Errorf("commandRE does not match %q", c)
		}
	}
	if !volumeUpRE.MatchString(strings.ToLower("Сделай громче")) {
		t.Error("volumeUpRE does not match")
	}
	if !volumeSetRE.MatchString(strings.ToLower("Поставь громкость 40")) {
		t.Error("volumeSetRE does not match")
	}
	if !mediaNextRE.MatchString(strings.ToLower("Следующий трек")) {
		t.Error("mediaNextRE does not match")
	}
	if !mediaPauseRE.MatchString(strings.ToLower("Поставь на паузу")) {
		t.Error("mediaPauseRE does not match")
	}
}
