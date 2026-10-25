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
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"
)

// Profile is the server-side configuration of one speaker, keyed by its
// device ID. Non-empty fields override what the speaker reports in its hello
// and the global [Settings], so speakers sold to different people, in
// different countries, are configured centrally.
type Profile struct {
	// Name is a label for the owner, e.g. "Иван, Хайфа".
	Name string `json:"name,omitempty"`
	// Language is the main language: speech synthesis and the default reply
	// language.
	Language string `json:"language,omitempty"`
	// ExtraLanguages are other languages the owner speaks, comma separated.
	ExtraLanguages string `json:"extra_languages,omitempty"`
	Location       string `json:"location,omitempty"`
	TimeZone       string `json:"timezone,omitempty"`
	// Token, if set, authenticates this speaker instead of the shared token:
	// a speaker presenting it can only connect under this device ID, and
	// this device ID cannot connect with another speaker's token.
	Token string `json:"token,omitempty"`
}

var deviceIDRE = regexp.MustCompile(`^[A-Za-z0-9._:-]{1,64}$`)

const maxName = 100

func (p *Profile) validate() error {
	p.Name = strings.TrimSpace(p.Name)
	p.Language = strings.TrimSpace(p.Language)
	p.Location = strings.TrimSpace(p.Location)
	p.TimeZone = strings.TrimSpace(p.TimeZone)
	p.Token = strings.TrimSpace(p.Token)
	if utf8.RuneCountInString(p.Name) > maxName {
		return errors.New("слишком длинное имя")
	}
	if p.Language != "" && !languageRE.MatchString(p.Language) {
		return fmt.Errorf("язык %q не похож на код BCP-47, например ru-RU", p.Language)
	}
	extra, err := parseLanguages(p.ExtraLanguages, p.Language)
	if err != nil {
		return err
	}
	p.ExtraLanguages = strings.Join(extra, ", ")
	if utf8.RuneCountInString(p.Location) > maxLocation {
		return errors.New("слишком длинное название места")
	}
	if p.TimeZone != "" {
		if _, err := time.LoadLocation(p.TimeZone); err != nil {
			return fmt.Errorf("неизвестный часовой пояс %q, нужен вид Europe/Moscow", p.TimeZone)
		}
	}
	if p.Token != "" && len(p.Token) < 16 {
		return errors.New("токен колонки должен быть не короче 16 символов")
	}
	return nil
}

// NewDeviceToken returns a random token for a [Profile].
func NewDeviceToken() string {
	b := make([]byte, 18)
	rand.Read(b)
	return hex.EncodeToString(b)
}

// Profiles returns a copy of all speaker profiles.
func (s *Server) Profiles() map[string]Profile {
	s.pmu.RLock()
	defer s.pmu.RUnlock()
	return maps.Clone(s.profiles)
}

// Profile returns the profile of a device.
func (s *Server) Profile(deviceID string) (Profile, bool) {
	s.pmu.RLock()
	defer s.pmu.RUnlock()
	p, ok := s.profiles[deviceID]
	return p, ok
}

// SetProfile validates, stores and applies a speaker profile. A profile
// with every field empty is deleted.
func (s *Server) SetProfile(deviceID string, p Profile) (Profile, error) {
	if !deviceIDRE.MatchString(deviceID) {
		return Profile{}, fmt.Errorf("недопустимый ID колонки %q", deviceID)
	}
	if err := p.validate(); err != nil {
		return Profile{}, err
	}
	s.pmu.Lock()
	for id, o := range s.profiles {
		if id != deviceID && p.Token != "" && o.Token == p.Token {
			s.pmu.Unlock()
			return Profile{}, fmt.Errorf("этот токен уже у колонки %s", id)
		}
	}
	next := maps.Clone(s.profiles)
	if next == nil {
		next = map[string]Profile{}
	}
	if p == (Profile{}) {
		delete(next, deviceID)
	} else {
		next[deviceID] = p
	}
	if s.cfg.ProfilesFile != "" {
		if err := saveProfiles(s.cfg.ProfilesFile, next); err != nil {
			s.pmu.Unlock()
			return Profile{}, fmt.Errorf("не удалось сохранить профили: %w", err)
		}
	}
	s.profiles = next
	s.pmu.Unlock()

	s.cfg.Logger.Info("device profile changed", "device", deviceID, "language", p.Language,
		"extra_languages", p.ExtraLanguages, "location", p.Location, "timezone", p.TimeZone, "own_token", p.Token != "")
	for _, d := range s.sessions() {
		if d.hello != nil && d.hello.DeviceID == deviceID {
			if !d.tokenOK(deviceID, p) {
				d.log.Info("disconnecting: the device token changed")
				d.conn.Close("token changed")
				continue
			}
			d.refresh()
			d.kickAlarms()
		}
	}
	s.events.devicesChanged()
	return p, nil
}

// deviceForToken returns the device whose profile token is tok.
func (s *Server) deviceForToken(tok string) (string, bool) {
	if tok == "" {
		return "", false
	}
	s.pmu.RLock()
	defer s.pmu.RUnlock()
	for id, p := range s.profiles {
		if p.Token != "" && subtle.ConstantTimeCompare([]byte(p.Token), []byte(tok)) == 1 {
			return id, true
		}
	}
	return "", false
}

func loadProfiles(path string) (map[string]Profile, error) {
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return map[string]Profile{}, nil
	}
	if err != nil {
		return nil, err
	}
	var ps map[string]Profile
	if err := json.Unmarshal(b, &ps); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	for id, p := range ps {
		if err := p.validate(); err != nil {
			return nil, fmt.Errorf("%s: колонка %s: %w", path, id, err)
		}
		ps[id] = p
	}
	return ps, nil
}

func saveProfiles(path string, ps map[string]Profile) error {
	b, err := json.MarshalIndent(ps, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".devices-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	// Profiles hold device tokens.
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(append(b, '\n')); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}
