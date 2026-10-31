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
	"embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/yawaflua/marusya-gemini/internal/alarms"
	"github.com/yawaflua/marusya-gemini/internal/models"
	"github.com/yawaflua/marusya-gemini/internal/provider"
	"github.com/yawaflua/marusya-gemini/internal/speech/geminispeech"
	"github.com/yawaflua/marusya-gemini/internal/tools"
)

//go:embed panel
var panelFiles embed.FS

const (
	panelCookie     = "marusya_panel"
	panelSessionTTL = 30 * 24 * time.Hour
	maxPanelLogins  = 100
	maxSayText      = 1000
	// csrfHeader must accompany every state-changing request. Browsers do
	// not let other sites set custom headers without a CORS preflight,
	// which the panel never allows.
	csrfHeader = "X-Marusya-Panel"
)

// panelAuth keeps the logins of the web panel in memory: a restart logs
// everyone out.
type panelAuth struct {
	password []byte
	failMu   sync.Mutex // serializes failed attempts, about one per second

	mu       sync.Mutex
	sessions map[string]time.Time // token -> expiry
}

func newPanelAuth(password string) *panelAuth {
	return &panelAuth{password: []byte(password), sessions: make(map[string]time.Time)}
}

func (a *panelAuth) login(password string) (string, bool) {
	if subtle.ConstantTimeCompare([]byte(password), a.password) != 1 {
		a.failMu.Lock()
		time.Sleep(time.Second)
		a.failMu.Unlock()
		return "", false
	}
	b := make([]byte, 32)
	rand.Read(b)
	token := hex.EncodeToString(b)
	now := time.Now()
	a.mu.Lock()
	defer a.mu.Unlock()
	for t, exp := range a.sessions {
		if now.After(exp) {
			delete(a.sessions, t)
		}
	}
	if len(a.sessions) >= maxPanelLogins {
		// Evict the login that expires first.
		var oldest string
		for t, exp := range a.sessions {
			if oldest == "" || exp.Before(a.sessions[oldest]) {
				oldest = t
			}
		}
		delete(a.sessions, oldest)
	}
	a.sessions[token] = now.Add(panelSessionTTL)
	return token, true
}

func (a *panelAuth) valid(r *http.Request) bool {
	c, err := r.Cookie(panelCookie)
	if err != nil {
		return false
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	exp, ok := a.sessions[c.Value]
	if ok && time.Now().After(exp) {
		delete(a.sessions, c.Value)
		return false
	}
	return ok
}

func (a *panelAuth) logout(r *http.Request) {
	if c, err := r.Cookie(panelCookie); err == nil {
		a.mu.Lock()
		delete(a.sessions, c.Value)
		a.mu.Unlock()
	}
}

func (s *Server) routePanel() {
	static, err := fs.Sub(panelFiles, "panel")
	if err != nil {
		panic(err)
	}
	files := http.StripPrefix("/panel/", http.FileServerFS(static))
	s.mux.Handle("GET /panel/", securityHeaders(files))
	s.mux.Handle("GET /panel", http.RedirectHandler("/panel/", http.StatusFound))
	s.mux.Handle("GET /{$}", http.RedirectHandler("/panel/", http.StatusFound))

	s.mux.HandleFunc("POST /panel/api/login", s.handleLogin)
	s.mux.HandleFunc("POST /panel/api/logout", s.panelAPI(s.handleLogout))
	s.mux.HandleFunc("GET /panel/api/state", s.panelAPI(s.handleState))
	s.mux.HandleFunc("GET /panel/api/events", s.panelAPI(s.handleEvents))
	s.mux.HandleFunc("POST /panel/api/devices/{session}/command", s.panelAPI(s.handleCommand))
	s.mux.HandleFunc("POST /panel/api/devices/{session}/say", s.panelAPI(s.handleSay))
	s.mux.HandleFunc("POST /panel/api/devices/{session}/forget", s.panelAPI(s.handleForget))
	s.mux.HandleFunc("PUT /panel/api/settings", s.panelAPI(s.handleSettings))
	s.mux.HandleFunc("GET /panel/api/profiles", s.panelAPI(s.handleProfiles))
	s.mux.HandleFunc("PUT /panel/api/profiles/{device}", s.panelAPI(s.handleSetProfile))
	s.mux.HandleFunc("DELETE /panel/api/profiles/{device}", s.panelAPI(s.handleDeleteProfile))
	s.mux.HandleFunc("GET /panel/api/alarms/{device}", s.panelAPI(s.handleAlarms))
	s.mux.HandleFunc("POST /panel/api/alarms/{device}", s.panelAPI(s.handleAddAlarm))
	s.mux.HandleFunc("DELETE /panel/api/alarms/{device}/{id}", s.panelAPI(s.handleDeleteAlarm))
	s.mux.HandleFunc("DELETE /panel/api/settings", s.panelAPI(s.handleResetSettings))
	s.registerProviderAPI()
	s.registerPluginAPI()
	s.registerCatalogAPI()
	if s.cfg.Spotify != nil {
		s.mux.HandleFunc("PUT /panel/api/spotify", s.panelAPI(s.handleSetSpotify))
		s.mux.HandleFunc("DELETE /panel/api/spotify", s.panelAPI(s.handleResetSpotify))
	}
	if s.cfg.AgentDir != "" {
		s.mux.HandleFunc("POST /panel/api/install", s.panelAPI(s.handleInstall))
		s.mux.HandleFunc("POST /panel/api/install/spotify", s.panelAPI(s.handleSpotifyLogin))
	}
}

func securityHeaders(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hd := w.Header()
		hd.Set("Content-Security-Policy", "default-src 'self'; img-src 'self' data:; frame-ancestors 'none'; form-action 'self'")
		hd.Set("X-Content-Type-Options", "nosniff")
		hd.Set("Referrer-Policy", "no-referrer")
		hd.Set("Cache-Control", "no-cache")
		h.ServeHTTP(w, r)
	})
}

// panelAPI requires a login, and the CSRF header for state-changing
// requests.
func (s *Server) panelAPI(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		if r.Method != http.MethodGet && r.Header.Get(csrfHeader) != "1" {
			writeError(w, http.StatusForbidden, errors.New("missing "+csrfHeader+" header"))
			return
		}
		if !s.panel.valid(r) {
			writeError(w, http.StatusUnauthorized, errors.New("нужно войти"))
			return
		}
		h(w, r)
	}
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, code int, err error) {
	writeJSON(w, code, map[string]string{"error": err.Error()})
}

func readJSON(w http.ResponseWriter, r *http.Request, v any) bool {
	r.Body = http.MaxBytesReader(w, r.Body, 64<<10)
	if err := json.NewDecoder(r.Body).Decode(v); err != nil {
		writeError(w, http.StatusBadRequest, fmt.Errorf("неверный запрос: %w", err))
		return false
	}
	return true
}

func (s *Server) handleLogin(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	if r.Header.Get(csrfHeader) != "1" {
		writeError(w, http.StatusForbidden, errors.New("missing "+csrfHeader+" header"))
		return
	}
	var req struct {
		Password string `json:"password"`
	}
	if !readJSON(w, r, &req) {
		return
	}
	token, ok := s.panel.login(req.Password)
	if !ok {
		s.cfg.Logger.Warn("panel: wrong password", "remote", r.RemoteAddr)
		writeError(w, http.StatusUnauthorized, errors.New("неверный пароль"))
		return
	}
	s.cfg.Logger.Info("panel: login", "remote", r.RemoteAddr)
	http.SetCookie(w, &http.Cookie{
		Name:     panelCookie,
		Value:    token,
		Path:     "/panel/",
		MaxAge:   int(panelSessionTTL / time.Second),
		HttpOnly: true,
		Secure:   r.TLS != nil,
		SameSite: http.SameSiteStrictMode,
	})
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	s.panel.logout(r)
	http.SetCookie(w, &http.Cookie{Name: panelCookie, Value: "", Path: "/panel/", MaxAge: -1, HttpOnly: true, SameSite: http.SameSiteStrictMode})
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// panelState is everything the panel shows on load.
type panelState struct {
	Version  string       `json:"version"`
	Provider string       `json:"provider"`
	Started  time.Time    `json:"started"`
	Tunable  bool         `json:"tunable"`
	Text     bool         `json:"text"`
	Saved    bool         `json:"saved"`   // settings survive a restart
	Wake     bool         `json:"wake"`    // the server spots wake words
	Install  bool         `json:"install"` // speakers can be set up over SSH
	Spotify  *spotifyInfo `json:"spotify,omitempty"`
	Settings Settings     `json:"settings"`
	Defaults Settings     `json:"defaults"`
	Voices   []string     `json:"voices"`
	Actions  []panelTool  `json:"actions"`
	Devices  []DeviceInfo `json:"devices"`
	History  []Event      `json:"history"`
	// Profiles are the per-speaker settings, by device ID.
	Profiles map[string]Profile `json:"profiles"`
}

type panelTool struct {
	Action      models.Action `json:"action"`
	Description string        `json:"description"`
}

func (s *Server) handleState(w http.ResponseWriter, _ *http.Request) {
	_, tunable := provider.AsTuner(s.cfg.Provider)
	var actions []panelTool
	for _, t := range tools.All() {
		actions = append(actions, panelTool{Action: t.Action, Description: t.Description})
	}
	writeJSON(w, http.StatusOK, panelState{
		Version:  s.cfg.Version,
		Provider: s.cfg.Provider.Name(),
		Started:  s.started,
		Tunable:  tunable,
		Text:     provider.AcceptsText(s.cfg.Provider),
		Saved:    s.cfg.SettingsFile != "",
		Wake:     s.cfg.WakeURL != "",
		Install:  s.cfg.AgentDir != "",
		Spotify:  s.spotifyInfo(),
		Settings: s.Settings(),
		Defaults: s.defaults,
		Voices:   panelVoices(s.cfg.Provider),
		Actions:  actions,
		Devices:  s.Devices(),
		History:  s.events.history(),
		Profiles: s.Profiles(),
	})
}

// handleEvents streams live updates as server-sent events: "history" for a
// new [Event] and "devices" with the full device list.
func (s *Server) handleEvents(w http.ResponseWriter, r *http.Request) {
	fl, ok := w.(http.Flusher)
	if !ok {
		writeError(w, http.StatusInternalServerError, errors.New("streaming unsupported"))
		return
	}
	sub := s.events.subscribe()
	defer s.events.unsubscribe(sub)

	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("X-Accel-Buffering", "no") // nginx
	w.WriteHeader(http.StatusOK)
	fmt.Fprint(w, "retry: 3000\n\n")
	fl.Flush()

	send := func(event string, v any) bool {
		b, err := json.Marshal(v)
		if err != nil {
			return true
		}
		if _, err := fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event, b); err != nil {
			return false
		}
		fl.Flush()
		return true
	}
	ping := time.NewTicker(25 * time.Second)
	defer ping.Stop()
	for {
		var ok bool
		select {
		case <-r.Context().Done():
			return
		case e := <-sub.events:
			ok = send("history", e)
		case <-sub.devices:
			ok = send("devices", s.Devices())
		case <-ping.C:
			_, err := fmt.Fprint(w, ": ping\n\n")
			fl.Flush()
			ok = err == nil
		}
		if !ok {
			return
		}
	}
}

func (s *Server) panelDevice(w http.ResponseWriter, r *http.Request) (*deviceSession, bool) {
	d, ok := s.device(r.PathValue("session"))
	if !ok {
		writeError(w, http.StatusNotFound, errors.New("колонка отключилась"))
	}
	return d, ok
}

func (s *Server) handleCommand(w http.ResponseWriter, r *http.Request) {
	d, ok := s.panelDevice(w, r)
	if !ok {
		return
	}
	var req struct {
		Action models.Action  `json:"action"`
		Args   map[string]any `json:"args"`
	}
	if !readJSON(w, r, &req) {
		return
	}
	if !d.supports(req.Action) {
		writeError(w, http.StatusBadRequest, errUnsupported)
		return
	}
	res, err := d.command(r.Context(), "", "", req.Action, req.Args, SourcePanel)
	if err != nil {
		writeError(w, http.StatusBadGateway, err)
		return
	}
	if !res.OK {
		writeError(w, http.StatusUnprocessableEntity, errors.New(res.Error))
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "data": res.Data})
}

func (s *Server) handleSay(w http.ResponseWriter, r *http.Request) {
	d, ok := s.panelDevice(w, r)
	if !ok {
		return
	}
	var req struct {
		Text string `json:"text"`
	}
	if !readJSON(w, r, &req) {
		return
	}
	text := strings.TrimSpace(req.Text)
	if text == "" || utf8.RuneCountInString(text) > maxSayText {
		writeError(w, http.StatusBadRequest, fmt.Errorf("нужен текст от 1 до %d символов", maxSayText))
		return
	}
	if err := d.say(text); err != nil {
		code := http.StatusBadGateway
		switch {
		case errors.Is(err, errBusy):
			code = http.StatusConflict
		case errors.Is(err, errNoRemote), errors.Is(err, errNoText):
			code = http.StatusNotImplemented
		}
		writeError(w, code, err)
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]bool{"ok": true})
}

func (s *Server) handleForget(w http.ResponseWriter, r *http.Request) {
	d, ok := s.panelDevice(w, r)
	if !ok {
		return
	}
	if err := d.forget(); err != nil {
		writeError(w, http.StatusConflict, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (s *Server) handleSettings(w http.ResponseWriter, r *http.Request) {
	var req Settings
	if !readJSON(w, r, &req) {
		return
	}
	set, err := s.UpdateSettings(req)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	writeJSON(w, http.StatusOK, set)
}

func (s *Server) handleResetSettings(w http.ResponseWriter, _ *http.Request) {
	set, err := s.ResetSettings()
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, set)
}

func (s *Server) handleProfiles(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, s.Profiles())
}

// handleSetProfile stores a speaker profile. A token of "new" generates one.
func (s *Server) handleSetProfile(w http.ResponseWriter, r *http.Request) {
	var p Profile
	if !readJSON(w, r, &p) {
		return
	}
	if p.Token == "new" {
		p.Token = NewDeviceToken()
	}
	p, err := s.SetProfile(r.PathValue("device"), p)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	writeJSON(w, http.StatusOK, p)
}

func (s *Server) handleDeleteProfile(w http.ResponseWriter, r *http.Request) {
	if _, err := s.SetProfile(r.PathValue("device"), Profile{}); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (s *Server) handleAlarms(w http.ResponseWriter, r *http.Request) {
	list := s.alarms.List(r.PathValue("device"))
	if list == nil {
		list = []alarms.Alarm{}
	}
	writeJSON(w, http.StatusOK, list)
}

func (s *Server) handleAddAlarm(w http.ResponseWriter, r *http.Request) {
	var a alarms.Alarm
	if !readJSON(w, r, &a) {
		return
	}
	device := r.PathValue("device")
	if !deviceIDRE.MatchString(device) {
		writeError(w, http.StatusBadRequest, errors.New("bad device id"))
		return
	}
	if !a.Repeats() && a.Date == "" {
		// The next time this time of day comes, where the speaker is.
		loc := time.Local
		if p, ok := s.Profile(device); ok && p.TimeZone != "" {
			if l, err := time.LoadLocation(p.TimeZone); err == nil {
				loc = l
			}
		} else if tz := s.Settings().DefaultTimeZone; tz != "" {
			if l, err := time.LoadLocation(tz); err == nil {
				loc = l
			}
		}
		for _, d := range s.sessions() {
			if d.hello != nil && d.hello.DeviceID == device {
				loc = d.location()
			}
		}
		now := time.Now().In(loc)
		if t, err := time.ParseInLocation("15:04", a.Time, loc); err == nil {
			at := time.Date(now.Year(), now.Month(), now.Day(), t.Hour(), t.Minute(), 0, 0, loc)
			if !at.After(now) {
				at = at.AddDate(0, 0, 1)
			}
			a.Date = at.Format(time.DateOnly)
		}
	}
	a, err := s.alarms.Add(device, a)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	s.cfg.Logger.Info("alarm added in the panel", "device", device, "time", a.Time, "date", a.Date, "days", a.Days, "volume", a.Volume)
	writeJSON(w, http.StatusOK, a)
}

func (s *Server) handleDeleteAlarm(w http.ResponseWriter, r *http.Request) {
	n, err := s.alarms.Delete(r.PathValue("device"), r.PathValue("id"))
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]int{"deleted": n})
}

// panelVoices returns the voices offered as suggestions: those of the
// provider, or the prebuilt Gemini voices if it does not know them.
func panelVoices(p provider.Provider) []string {
	if v := provider.Voices(p); len(v) > 0 {
		return v
	}
	return geminispeech.PrebuiltVoices
}
