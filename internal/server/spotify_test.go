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
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/yawaflua/marusya-gemini/internal/skills/spotify"
)

// spotifyTokens accepts the app "good" with the secret "s3".
func spotifyTokens(t *testing.T) string {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id, secret, _ := r.BasicAuth()
		w.Header().Set("Content-Type", "application/json")
		if id != "good" || secret != "s3" {
			w.WriteHeader(http.StatusBadRequest)
			w.Write([]byte(`{"error":"invalid_client"}`))
			return
		}
		w.Write([]byte(`{"access_token":"tok","token_type":"Bearer","expires_in":3600}`))
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}

func newSpotifyServer(t *testing.T, dir string, sp *spotify.Client) *Server {
	t.Helper()
	return New(Config{
		Provider:     &toolProvider{},
		Skills:       sp.Skills(),
		Spotify:      sp,
		SettingsFile: filepath.Join(dir, "settings.json"),
		Logger:       slog.New(slog.DiscardHandler),
	})
}

func callSpotify(t *testing.T, h http.HandlerFunc, method, body string) (int, spotifyInfo, string) {
	t.Helper()
	w := httptest.NewRecorder()
	h(w, httptest.NewRequest(method, "/panel/api/spotify", strings.NewReader(body)))
	var info spotifyInfo
	json.Unmarshal(w.Body.Bytes(), &info)
	return w.Code, info, w.Body.String()
}

func TestSpotifyCredentialsFromPanel(t *testing.T) {
	dir := t.TempDir()
	token := spotifyTokens(t)
	sp := &spotify.Client{Token: token}
	s := newSpotifyServer(t, dir, sp)
	if _, ok := s.cfg.Skills.Lookup("spotify_play"); ok {
		t.Fatal("spotify_play offered without credentials")
	}
	if info := s.spotifyInfo(); info == nil || info.Env || info.Panel || !info.Saved {
		t.Fatalf("info %+v", info)
	}

	if code, _, body := callSpotify(t, s.handleSetSpotify, "PUT", `{"client_id":"good","client_secret":"wrong"}`); code != http.StatusBadRequest || !strings.Contains(body, "не принял") {
		t.Errorf("wrong secret: %d %s", code, body)
	}
	if code, _, _ := callSpotify(t, s.handleSetSpotify, "PUT", `{"client_id":"good","client_secret":"s3","market":"Russia"}`); code != http.StatusBadRequest {
		t.Errorf("bad market accepted: %d", code)
	}
	code, info, body := callSpotify(t, s.handleSetSpotify, "PUT", `{"client_id":" good ","client_secret":"s3","market":"ru"}`)
	if code != http.StatusOK || info.ClientID != "good" || !info.SecretSet || info.Market != "RU" || !info.Panel {
		t.Fatalf("set: %d %s", code, body)
	}
	if strings.Contains(body, "s3") {
		t.Error("the secret went back to the panel")
	}
	if _, ok := s.cfg.Skills.Lookup("spotify_play"); !ok {
		t.Error("spotify_play not offered with credentials")
	}
	st, err := os.Stat(filepath.Join(dir, "spotify.json"))
	if err != nil || st.Mode().Perm() != 0o600 {
		t.Errorf("spotify.json: %v, %v", st, err)
	}
	// An empty secret keeps the saved one.
	if code, _, body := callSpotify(t, s.handleSetSpotify, "PUT", `{"client_id":"good","market":""}`); code != http.StatusOK {
		t.Errorf("keep secret: %d %s", code, body)
	}

	// A restart picks the panel's credentials over the environment's.
	sp2 := &spotify.Client{Token: token, ClientID: "env", ClientSecret: "envsecret"}
	s2 := newSpotifyServer(t, dir, sp2)
	if id, secret, _ := sp2.Credentials(); id != "good" || secret != "s3" {
		t.Errorf("after restart %q %q", id, secret)
	}
	if info := s2.spotifyInfo(); !info.Panel || !info.Env {
		t.Errorf("info after restart %+v", info)
	}
	// Reset goes back to the environment's.
	if code, info, _ := callSpotify(t, s2.handleResetSpotify, "DELETE", ""); code != http.StatusOK || info.ClientID != "env" || info.Panel {
		t.Errorf("reset: %d %+v", code, info)
	}
	if _, err := os.Stat(filepath.Join(dir, "spotify.json")); !os.IsNotExist(err) {
		t.Errorf("spotify.json after reset: %v", err)
	}
}
