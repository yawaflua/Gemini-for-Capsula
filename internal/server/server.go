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

// Package server accepts device streams and bridges each of them to a
// [provider.Session].
package server

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"log/slog"
	"net/http"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/yawaflua/marusya-gemini/internal/alarms"
	"github.com/yawaflua/marusya-gemini/internal/media"
	"github.com/yawaflua/marusya-gemini/internal/plugin"
	"github.com/yawaflua/marusya-gemini/internal/provider"
	"github.com/yawaflua/marusya-gemini/internal/provider/registry"
	"github.com/yawaflua/marusya-gemini/internal/skills"
	"github.com/yawaflua/marusya-gemini/internal/skills/alarm"
	"github.com/yawaflua/marusya-gemini/internal/skills/spotify"
	"github.com/yawaflua/marusya-gemini/internal/speech"
	"github.com/yawaflua/marusya-gemini/internal/transport"
)

// DefaultSystemInstruction is used when no custom prompt is configured.
const DefaultSystemInstruction = `Ты — голосовой ассистент, работающий внутри умной колонки.
Отвечай кратко, по-разговорному, без markdown, списков и эмодзи: твой ответ будет озвучен.
Если пользователь просит изменить громкость, управлять музыкой, Bluetooth или ночником — вызывай соответствующие функции.
Музыку по названию включай через spotify_play, если такая функция есть, иначе через media_play_search (ищет на YouTube); видео, подкасты и всё, что просят с ютуба, — через media_play_search, ссылку — через media_play_url. После включения коротко назови, что играет.
Если вместо речи пришёл шум, неразборчивые звуки или пометка вроде <noise>, ничего не включай и не вызывай функций: промолчи или коротко переспроси.
До вызова функции ничего не говори; после результата подтверди действие один раз, коротко — не повторяй фразу. Отвечай на языке пользователя.
На вопрос о времени вызывай time_now и отвечай в формате «ЧЧ:ММ в <место>», например «21:05 в Ашдоде».
О погоде спрашивай weather_forecast. Для новостей, курсов, фактов и всего, что меняется со временем, используй поиск Google.
Если пользователь прощается, просит замолчать или отстать, грубо посылает или явно закончил разговор, вызови conversation_end, а после него скажи максимум пару слов, один раз.`

// Config configures a [Server].
type Config struct {
	// Provider is the model backend. Required.
	Provider provider.Provider
	// AuthToken, if set, must be presented by devices as a Bearer token.
	AuthToken string
	// SystemInstruction is the model persona.
	SystemInstruction string
	// Language is the default BCP-47 language, overridable by the device.
	Language string
	// ExtraLanguages are other languages users speak, e.g. "he-IL": comma
	// separated BCP-47 codes, used as speech recognition hints.
	ExtraLanguages string
	// IdleTimeout closes the model session after this period without turns.
	// Conversation history is kept while the session is open, which allows
	// follow-up questions.
	IdleTimeout time.Duration
	// CommandTimeout bounds how long a device has to execute a command.
	CommandTimeout time.Duration
	// AgentDir, if set, serves agent binaries named
	// "marusya-agent-linux-<arch>" from this directory at
	// GET /v1/agent/{arch}, so speakers with a full read-only flash can
	// download the agent into RAM on every boot.
	AgentDir string
	// Spotify, if set, is the app behind the spotify_play skill (in
	// Skills too); its credentials can be changed in the web panel.
	Spotify *spotify.Client
	// HTTPClient makes the server's own downloads (the AirPlay bundle for
	// speakers set up from the panel). Nil uses http.DefaultClient.
	HTTPClient *http.Client
	// DefaultLocation and DefaultTimeZone describe where a speaker is when
	// it does not say so itself (see models.Hello).
	DefaultLocation string
	DefaultTimeZone string
	// Skills are functions executed on the server (time, weather, ...).
	Skills skills.Set
	// ServerVAD lets the provider detect the end of speech instead of the
	// device.
	ServerVAD bool
	// PanelPassword enables the web panel at /panel/ and protects it. Empty
	// disables the panel.
	PanelPassword string
	// SettingsFile, if set, keeps the settings changed in the web panel
	// across restarts. Values in it override the ones above.
	SettingsFile string
	// ProfilesFile keeps per-speaker profiles (region, languages, token),
	// see [Profile]. Empty keeps them in memory only.
	ProfilesFile string
	// WakeURL is the WebSocket URL of the wake word spotter (deploy/wake).
	// Empty leaves wake word detection to the devices.
	WakeURL string
	// WakeWords is a comma separated list of wake words spotted on the
	// server, e.g. "антон, אנטון". Changeable in the web panel.
	WakeWords string
	// Synthesizer and Recognizer speak and transcribe announcements of the
	// ask API (POST /v1/ask); nil disables it.
	Synthesizer speech.Synthesizer
	Recognizer  speech.Recognizer
	// Media finds and decodes music and videos for speakers
	// (GET /v1/media); nil disables it.
	Media media.Source
	// AlarmsFile keeps the alarms and timers of the speakers; empty keeps
	// them in memory only.
	AlarmsFile string
	// Providers are the model providers configured in the web panel. If
	// set, Provider defaults to Providers.Provider(); nil leaves the
	// provider to the configuration.
	Providers *registry.Registry
	// ProvidersFile is where Providers are kept, shown in the panel.
	ProvidersFile string
	// PluginsDir holds the server plugins, one directory each with a
	// plugin.json (see pkg/pluginsdk); empty disables plugins. Call
	// [Server.StartPlugins] to run them.
	PluginsDir string
	// Version is shown in the web panel.
	Version string
	// Logger receives structured logs.
	Logger *slog.Logger
}

func (c *Config) setDefaults() {
	if c.Provider == nil && c.Providers != nil {
		c.Provider = c.Providers.Provider()
	}
	if c.SystemInstruction == "" {
		c.SystemInstruction = DefaultSystemInstruction
	}
	if c.IdleTimeout == 0 {
		c.IdleTimeout = 2 * time.Minute
	}
	if c.CommandTimeout == 0 {
		c.CommandTimeout = 30 * time.Second
	}
	if c.Logger == nil {
		c.Logger = slog.Default()
	}
}

// Server is an HTTP handler serving the device streaming endpoint and the
// web panel.
type Server struct {
	cfg     Config
	mux     *http.ServeMux
	started time.Time
	events  *hub
	panel   *panelAuth
	alarms  *alarms.Store

	smu      sync.RWMutex
	settings Settings
	defaults Settings // as started, see ResetSettings

	dmu     sync.Mutex
	devices map[string]*deviceSession // by session ID

	pmu      sync.RWMutex
	profiles map[string]Profile // by device ID

	plugins     *plugin.Manager // nil without PluginsDir
	plmu        sync.Mutex
	pluginTools string // names of the plugin tools, see pluginsChanged

	logins spotifyLogins // Spotify sign-ins started from the panel

	spmu         sync.Mutex
	spotifyEnv   spotifyCreds // as started, see handleResetSpotify
	spotifyPanel bool         // the credentials were set in the panel
}

// New creates a server.
func New(cfg Config) *Server {
	cfg.setDefaults()
	s := &Server{
		cfg:     cfg,
		mux:     http.NewServeMux(),
		started: time.Now(),
		events:  newHub(),
		devices: make(map[string]*deviceSession),
	}
	s.profiles = map[string]Profile{}
	if cfg.ProfilesFile != "" {
		ps, err := loadProfiles(cfg.ProfilesFile)
		if err != nil {
			cfg.Logger.Error("load device profiles", "err", err)
		} else {
			s.profiles = ps
		}
	}
	store, err := alarms.Open(cfg.AlarmsFile)
	if err != nil {
		cfg.Logger.Error("load alarms", "err", err)
		store, _ = alarms.Open("")
	}
	s.alarms = store
	store.OnChange = s.alarmsChanged
	s.cfg.Skills = append(slices.Clone(cfg.Skills), alarm.Skills(store)...)
	s.plugins = s.newPlugins()
	s.defaults = cfg.defaultSettings()
	s.settings = s.defaults
	if cfg.SettingsFile != "" {
		o, err := loadOverrides(cfg.SettingsFile)
		if err != nil {
			cfg.Logger.Error("load settings; using defaults", "err", err)
		}
		s.settings = applyOverrides(s.defaults, o)
		if t, ok := provider.AsTuner(cfg.Provider); ok {
			t.Tune(provider.Tuning{Model: s.settings.Model, Voice: s.settings.Voice})
		}
	}
	s.initSpotify()
	s.mux.HandleFunc("GET "+transport.Path, s.handleStream)
	if cfg.AgentDir != "" {
		s.mux.HandleFunc("GET /v1/agent/{arch}", s.handleAgent)
	}
	s.mux.HandleFunc("POST /v1/ask", s.handleAsk)
	s.mux.HandleFunc("GET "+media.Path, s.handleMedia)
	s.mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.Write([]byte("ok\n"))
	})
	if cfg.PanelPassword != "" {
		s.panel = newPanelAuth(cfg.PanelPassword)
		s.routePanel()
	}
	return s
}

// ServeHTTP implements [http.Handler].
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) { s.mux.ServeHTTP(w, r) }

// authorized reports whether r carries the shared token (or auth is off).
func (s *Server) authorized(r *http.Request) bool {
	if s.cfg.AuthToken == "" {
		return true
	}
	return subtle.ConstantTimeCompare([]byte(bearerToken(r)), []byte(s.cfg.AuthToken)) == 1
}

// authorizedDevice reports whether r may act as a speaker: the shared token
// or the token of a speaker profile.
func (s *Server) authorizedDevice(r *http.Request) bool {
	if s.authorized(r) {
		return true
	}
	_, ok := s.deviceForToken(bearerToken(r))
	return ok
}

// authorizedToken reports whether tok is the shared token.
func (s *Server) authorizedToken(tok string) bool {
	return s.cfg.AuthToken != "" && subtle.ConstantTimeCompare([]byte(tok), []byte(s.cfg.AuthToken)) == 1
}

func bearerToken(r *http.Request) string {
	t, _ := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	return t
}

var archRE = regexp.MustCompile(`^[a-z0-9]+$`)

// handleAgent serves the agent binary for the requested architecture.
func (s *Server) handleAgent(w http.ResponseWriter, r *http.Request) {
	if !s.authorizedDevice(r) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	arch := r.PathValue("arch")
	if !archRE.MatchString(arch) {
		http.Error(w, "invalid architecture", http.StatusBadRequest)
		return
	}
	path := filepath.Join(s.cfg.AgentDir, "marusya-agent-linux-"+arch)
	s.cfg.Logger.Info("serving agent binary", "arch", arch, "remote", r.RemoteAddr)
	w.Header().Set("Content-Type", "application/octet-stream")
	http.ServeFile(w, r, path)
}

// handleMedia streams the sound of a video or stream as PCM, see
// [media.Serve]. Query: "url" (a page or stream URL) or "q" (a search).
func (s *Server) handleMedia(w http.ResponseWriter, r *http.Request) {
	if !s.authorizedDevice(r) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	if s.cfg.Media == nil {
		http.Error(w, "media playback is not set up on the server (install yt-dlp and ffmpeg)", http.StatusNotImplemented)
		return
	}
	req := media.Request{URL: r.URL.Query().Get("url"), Query: strings.TrimSpace(r.URL.Query().Get("q"))}
	if err := req.Validate(); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	log := s.cfg.Logger.With("remote", r.RemoteAddr, "request", req.String())
	t, err := s.cfg.Media.Open(r.Context(), req)
	if err != nil {
		log.Warn("media: open failed", "err", err)
		code := http.StatusBadGateway
		if errors.Is(err, media.ErrNotFound) {
			code = http.StatusNotFound
		}
		http.Error(w, err.Error(), code)
		return
	}
	defer t.Audio.Close()
	start := time.Now()
	err = media.Serve(w, t)
	log.Info("media: stream ended", "title", t.Title, "played", time.Since(start).Round(time.Second), "err", err)
}

func (s *Server) handleStream(w http.ResponseWriter, r *http.Request) {
	if !s.authorizedDevice(r) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	conn, err := transport.Accept(w, r)
	if err != nil {
		s.cfg.Logger.Warn("accept failed", "remote", r.RemoteAddr, "err", err)
		return
	}
	id := newID()
	log := s.cfg.Logger.With("session", id, "remote", r.RemoteAddr)
	ds := newDeviceSession(id, conn, s, r.RemoteAddr, log)
	ds.token = bearerToken(r)
	if err := ds.run(r.Context()); err != nil {
		log.Info("device session ended", "err", err)
		conn.Close("session ended")
		return
	}
	log.Info("device session ended")
	conn.Close("bye")
}

func newID() string {
	b := make([]byte, 8)
	rand.Read(b)
	return hex.EncodeToString(b)
}

// register adds a device that completed the handshake.
func (s *Server) register(d *deviceSession) {
	s.dmu.Lock()
	s.devices[d.id] = d
	s.dmu.Unlock()
	s.events.publish(Event{Kind: EventConnect, Session: d.id, Device: d.hello.DeviceID})
	s.events.devicesChanged()
}

func (s *Server) unregister(d *deviceSession) {
	s.dmu.Lock()
	delete(s.devices, d.id)
	s.dmu.Unlock()
	s.events.publish(Event{Kind: EventDisconnect, Session: d.id, Device: d.hello.DeviceID})
	s.events.devicesChanged()
}

// device returns the connected device with the given session ID.
func (s *Server) device(id string) (*deviceSession, bool) {
	s.dmu.Lock()
	defer s.dmu.Unlock()
	d, ok := s.devices[id]
	return d, ok
}

// sessions returns the connected devices, the oldest connection first.
func (s *Server) sessions() []*deviceSession {
	s.dmu.Lock()
	out := make([]*deviceSession, 0, len(s.devices))
	for _, d := range s.devices {
		out = append(out, d)
	}
	s.dmu.Unlock()
	sort.Slice(out, func(i, j int) bool { return out[i].connected.Before(out[j].connected) })
	return out
}

// Devices describes the connected devices.
func (s *Server) Devices() []DeviceInfo {
	ds := s.sessions()
	out := make([]DeviceInfo, len(ds))
	for i, d := range ds {
		out[i] = d.info()
	}
	return out
}
