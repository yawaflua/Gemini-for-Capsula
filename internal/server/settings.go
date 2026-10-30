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
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/yawaflua/marusya-gemini/internal/provider"
	"github.com/yawaflua/marusya-gemini/internal/wake"
)

// Settings are the parameters that can be changed from the web panel while
// the server runs. They apply to model sessions opened afterwards; idle
// sessions are reopened right away.
type Settings struct {
	SystemInstruction string `json:"system_instruction,omitempty"`
	Language          string `json:"language,omitempty"`
	// ExtraLanguages are other languages the users speak, comma separated
	// BCP-47 codes; replies follow the user, speech synthesis uses Language.
	ExtraLanguages  string `json:"extra_languages,omitempty"`
	DefaultLocation string `json:"default_location,omitempty"`
	DefaultTimeZone string `json:"default_timezone,omitempty"`
	// Model and Voice are empty when the provider cannot change them.
	Model string `json:"model,omitempty"`
	Voice string `json:"voice,omitempty"`
	// WakeWords is a comma separated list of keywords spotted on the server.
	WakeWords string `json:"wake_words,omitempty"`
}

var (
	languageRE = regexp.MustCompile(`^[A-Za-z]{2,3}(-[A-Za-z0-9]{2,8})*$`)
	nameRE     = regexp.MustCompile(`^[A-Za-z0-9._/-]{1,128}$`)
)

const (
	maxInstruction = 20000
	maxLocation    = 200
)

// validate normalizes s in place and checks every field.
func (s *Settings) validate(tunable bool) error {
	s.SystemInstruction = strings.TrimSpace(s.SystemInstruction)
	s.Language = strings.TrimSpace(s.Language)
	extra, err := parseLanguages(s.ExtraLanguages, s.Language)
	if err != nil {
		return err
	}
	s.ExtraLanguages = strings.Join(extra, ", ")
	s.DefaultLocation = strings.TrimSpace(s.DefaultLocation)
	s.DefaultTimeZone = strings.TrimSpace(s.DefaultTimeZone)
	s.Model = strings.TrimSpace(s.Model)
	s.Voice = strings.TrimSpace(s.Voice)
	kws, err := wake.ParseKeywords(s.WakeWords)
	if err != nil {
		return err
	}
	s.WakeWords = strings.Join(kws, ", ")

	if utf8.RuneCountInString(s.SystemInstruction) > maxInstruction {
		return fmt.Errorf("системный промпт длиннее %d символов", maxInstruction)
	}
	if !languageRE.MatchString(s.Language) {
		return fmt.Errorf("язык %q не похож на код BCP-47, например ru-RU", s.Language)
	}
	if utf8.RuneCountInString(s.DefaultLocation) > maxLocation {
		return errors.New("слишком длинное название места")
	}
	if s.DefaultTimeZone != "" {
		if _, err := time.LoadLocation(s.DefaultTimeZone); err != nil {
			return fmt.Errorf("неизвестный часовой пояс %q, нужен вид Europe/Moscow", s.DefaultTimeZone)
		}
	}
	if !tunable {
		s.Model, s.Voice = "", ""
		return nil
	}
	if !nameRE.MatchString(s.Model) {
		return fmt.Errorf("недопустимое имя модели %q", s.Model)
	}
	if !nameRE.MatchString(s.Voice) {
		return fmt.Errorf("недопустимое имя голоса %q", s.Voice)
	}
	return nil
}

// overrides returns the fields of s that differ from def. Only they are
// saved, so a default changed in a later release still applies to the
// fields that were never edited.
func (s Settings) overrides(def Settings) Settings {
	var o Settings
	if s.SystemInstruction != def.SystemInstruction {
		o.SystemInstruction = s.SystemInstruction
	}
	if s.Language != def.Language {
		o.Language = s.Language
	}
	if s.ExtraLanguages != def.ExtraLanguages {
		o.ExtraLanguages = s.ExtraLanguages
	}
	if s.DefaultLocation != def.DefaultLocation {
		o.DefaultLocation = s.DefaultLocation
	}
	if s.DefaultTimeZone != def.DefaultTimeZone {
		o.DefaultTimeZone = s.DefaultTimeZone
	}
	if s.Model != def.Model {
		o.Model = s.Model
	}
	if s.Voice != def.Voice {
		o.Voice = s.Voice
	}
	if s.WakeWords != def.WakeWords {
		o.WakeWords = s.WakeWords
	}
	return o
}

// applyOverrides returns def with the non-empty fields of o applied.
func applyOverrides(def, o Settings) Settings {
	s := def
	for _, f := range []struct{ dst, src *string }{
		{&s.SystemInstruction, &o.SystemInstruction},
		{&s.Language, &o.Language},
		{&s.ExtraLanguages, &o.ExtraLanguages},
		{&s.DefaultLocation, &o.DefaultLocation},
		{&s.DefaultTimeZone, &o.DefaultTimeZone},
		{&s.Model, &o.Model},
		{&s.Voice, &o.Voice},
		{&s.WakeWords, &o.WakeWords},
	} {
		if *f.src != "" {
			*f.dst = *f.src
		}
	}
	return s
}

// defaultSettings describes the configuration the server was started with.
func (c *Config) defaultSettings() Settings {
	s := Settings{
		SystemInstruction: c.SystemInstruction,
		Language:          c.Language,
		ExtraLanguages:    c.ExtraLanguages,
		DefaultLocation:   c.DefaultLocation,
		DefaultTimeZone:   c.DefaultTimeZone,
		WakeWords:         c.WakeWords,
	}
	if kws, err := wake.ParseKeywords(s.WakeWords); err == nil {
		s.WakeWords = strings.Join(kws, ", ")
	}
	if t, ok := provider.AsTuner(c.Provider); ok {
		tn := t.Tuning()
		s.Model, s.Voice = tn.Model, tn.Voice
	}
	return s
}

func loadOverrides(path string) (Settings, error) {
	var o Settings
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return o, nil
	}
	if err != nil {
		return o, err
	}
	if err := json.Unmarshal(b, &o); err != nil {
		return o, fmt.Errorf("%s: %w", path, err)
	}
	return o, nil
}

// saveOverrides writes o atomically; an empty o removes the file.
func saveOverrides(path string, o Settings) error {
	if o == (Settings{}) {
		if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		return nil
	}
	b, err := json.MarshalIndent(o, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".settings-*")
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
	return os.Rename(tmp.Name(), path)
}

// Settings returns the current settings.
func (s *Server) Settings() Settings {
	s.smu.RLock()
	defer s.smu.RUnlock()
	return s.settings
}

// UpdateSettings validates and applies new settings, saves them to
// Config.SettingsFile and reopens idle model sessions so they take effect.
func (s *Server) UpdateSettings(n Settings) (Settings, error) {
	tuner, tunable := provider.AsTuner(s.cfg.Provider)
	if n.SystemInstruction == "" {
		n.SystemInstruction = s.defaults.SystemInstruction
	}
	if err := n.validate(tunable); err != nil {
		return Settings{}, err
	}
	if s.cfg.SettingsFile != "" {
		if err := saveOverrides(s.cfg.SettingsFile, n.overrides(s.defaults)); err != nil {
			return Settings{}, fmt.Errorf("не удалось сохранить настройки: %w", err)
		}
	}
	s.smu.Lock()
	s.settings = n
	s.smu.Unlock()
	if tunable {
		tuner.Tune(provider.Tuning{Model: n.Model, Voice: n.Voice})
		if s.cfg.Providers != nil {
			s.cfg.Providers.SetTuning(provider.Tuning{Model: n.Model, Voice: n.Voice})
		}
	}
	s.cfg.Logger.Info("settings changed", "language", n.Language, "location", n.DefaultLocation,
		"timezone", n.DefaultTimeZone, "model", n.Model, "voice", n.Voice)
	s.events.publish(Event{Kind: EventSettings})
	for _, d := range s.sessions() {
		d.refresh()
		d.kickWake()
		d.kickAlarms()
	}
	return n, nil
}

// ResetSettings restores the configuration the server was started with.
func (s *Server) ResetSettings() (Settings, error) {
	return s.UpdateSettings(s.defaults)
}

// maxExtraLanguages bounds Settings.ExtraLanguages.
const maxExtraLanguages = 4

// parseLanguages splits a comma separated list of BCP-47 codes, dropping
// duplicates and the primary language.
func parseLanguages(list, primary string) ([]string, error) {
	var out []string
	seen := map[string]bool{strings.ToLower(primary): true}
	for _, l := range strings.Split(list, ",") {
		l = strings.TrimSpace(l)
		if l == "" || seen[strings.ToLower(l)] {
			continue
		}
		if !languageRE.MatchString(l) {
			return nil, fmt.Errorf("язык %q не похож на код BCP-47, например he-IL", l)
		}
		seen[strings.ToLower(l)] = true
		out = append(out, l)
	}
	if len(out) > maxExtraLanguages {
		return nil, fmt.Errorf("не больше %d дополнительных языков", maxExtraLanguages)
	}
	return out, nil
}
