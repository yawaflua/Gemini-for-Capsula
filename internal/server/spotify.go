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
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

// spotifyCreds are the Spotify app's credentials set in the web panel,
// kept in spotify.json next to SettingsFile. They override the ones the
// server was started with (MARUSYA_SPOTIFY_CLIENT_ID, ...).
type spotifyCreds struct {
	ClientID     string `json:"client_id"`
	ClientSecret string `json:"client_secret"`
	Market       string `json:"market,omitempty"`
}

// spotifyInfo is what the panel sees: never the secret.
type spotifyInfo struct {
	ClientID  string `json:"client_id"`
	SecretSet bool   `json:"secret_set"`
	Market    string `json:"market"`
	// Panel tells whether the credentials come from the panel rather than
	// the server's environment; Env whether the environment has any.
	Panel bool `json:"panel"`
	Env   bool `json:"env"`
	// Saved tells whether changes survive a restart.
	Saved bool `json:"saved"`
}

var marketRE = regexp.MustCompile(`^[A-Z]{2}$`)

// spotifyFile is where the panel's credentials are kept, "" without
// SettingsFile.
func (s *Server) spotifyFile() string {
	if s.cfg.SettingsFile == "" {
		return ""
	}
	return filepath.Join(filepath.Dir(s.cfg.SettingsFile), "spotify.json")
}

// initSpotify remembers the credentials from the environment and applies
// the ones saved from the panel.
func (s *Server) initSpotify() {
	sp := s.cfg.Spotify
	if sp == nil {
		return
	}
	id, secret, market := sp.Credentials()
	s.spotifyEnv = spotifyCreds{ClientID: id, ClientSecret: secret, Market: market}
	path := s.spotifyFile()
	if path == "" {
		return
	}
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return
	}
	var c spotifyCreds
	if err == nil {
		err = json.Unmarshal(b, &c)
	}
	if err != nil {
		s.cfg.Logger.Error("load Spotify credentials; using the environment's", "file", path, "err", err)
		return
	}
	if c.ClientID != "" && c.ClientSecret != "" {
		sp.SetCredentials(c.ClientID, c.ClientSecret, c.Market)
		s.spotifyPanel = true
		s.cfg.Logger.Info("Spotify credentials from the web panel", "market", c.Market)
	}
}

func (s *Server) spotifyInfo() *spotifyInfo {
	if s.cfg.Spotify == nil {
		return nil
	}
	id, secret, market := s.cfg.Spotify.Credentials()
	s.spmu.Lock()
	defer s.spmu.Unlock()
	return &spotifyInfo{
		ClientID:  id,
		SecretSet: secret != "",
		Market:    market,
		Panel:     s.spotifyPanel,
		Env:       s.spotifyEnv.ClientID != "",
		Saved:     s.spotifyFile() != "",
	}
}

// handleSetSpotify sets the credentials after Spotify accepted them. An
// empty secret keeps the current one.
func (s *Server) handleSetSpotify(w http.ResponseWriter, r *http.Request) {
	var c spotifyCreds
	if !readJSON(w, r, &c) {
		return
	}
	c.ClientID = strings.TrimSpace(c.ClientID)
	c.ClientSecret = strings.TrimSpace(c.ClientSecret)
	c.Market = strings.ToUpper(strings.TrimSpace(c.Market))
	if c.ClientSecret == "" {
		_, c.ClientSecret, _ = s.cfg.Spotify.Credentials()
	}
	switch {
	case c.ClientID == "" || c.ClientSecret == "":
		writeError(w, http.StatusBadRequest, errors.New("нужны Client ID и Client Secret приложения Spotify"))
		return
	case c.Market != "" && !marketRE.MatchString(c.Market):
		writeError(w, http.StatusBadRequest, fmt.Errorf("страна %q: нужен двухбуквенный код, например RU или IL", c.Market))
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()
	if err := s.cfg.Spotify.Check(ctx, c.ClientID, c.ClientSecret); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	s.spmu.Lock()
	if path := s.spotifyFile(); path != "" {
		if err := writeSecretJSON(path, c); err != nil {
			s.spmu.Unlock()
			writeError(w, http.StatusInternalServerError, fmt.Errorf("не удалось сохранить: %w", err))
			return
		}
	}
	s.cfg.Spotify.SetCredentials(c.ClientID, c.ClientSecret, c.Market)
	s.spotifyPanel = true
	s.spmu.Unlock()
	s.cfg.Logger.Info("Spotify credentials set in the web panel", "market", c.Market)
	writeJSON(w, http.StatusOK, s.spotifyInfo())
}

// handleResetSpotify goes back to the environment's credentials.
func (s *Server) handleResetSpotify(w http.ResponseWriter, _ *http.Request) {
	s.spmu.Lock()
	if path := s.spotifyFile(); path != "" {
		if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			s.spmu.Unlock()
			writeError(w, http.StatusInternalServerError, err)
			return
		}
	}
	e := s.spotifyEnv
	s.cfg.Spotify.SetCredentials(e.ClientID, e.ClientSecret, e.Market)
	s.spotifyPanel = false
	s.spmu.Unlock()
	writeJSON(w, http.StatusOK, s.spotifyInfo())
}

// writeSecretJSON writes v atomically, readable by the owner only.
func writeSecretJSON(path string, v any) error {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".spotify-*")
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
