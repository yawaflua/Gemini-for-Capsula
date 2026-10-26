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

// Command marusya-server accepts audio streams from speakers and relays them
// to the Gemini Live API.
//
// Usage:
//
//	GEMINI_API_KEY=... marusya-server -listen :8080 -auth_token secret
package main

import (
	"cmp"
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"
	_ "time/tzdata" // time zones for the time skill on minimal images

	"github.com/yawaflua/marusya-gemini/internal/config"
	"github.com/yawaflua/marusya-gemini/internal/logging"
	"github.com/yawaflua/marusya-gemini/internal/media/ytdlp"
	"github.com/yawaflua/marusya-gemini/internal/openmeteo"
	"github.com/yawaflua/marusya-gemini/internal/provider"
	"github.com/yawaflua/marusya-gemini/internal/provider/claude"
	"github.com/yawaflua/marusya-gemini/internal/provider/echo"
	"github.com/yawaflua/marusya-gemini/internal/provider/gemini"
	"github.com/yawaflua/marusya-gemini/internal/server"
	"github.com/yawaflua/marusya-gemini/internal/skills"
	"github.com/yawaflua/marusya-gemini/internal/skills/clock"
	"github.com/yawaflua/marusya-gemini/internal/skills/homeassistant"
	"github.com/yawaflua/marusya-gemini/internal/skills/spotify"
	"github.com/yawaflua/marusya-gemini/internal/skills/weather"
	"github.com/yawaflua/marusya-gemini/internal/speech/geminispeech"
)

// version is set at build time with -ldflags "-X main.version=...".
var version = "dev"

var (
	listen     = flag.String("listen", config.Env("MARUSYA_LISTEN", ":8080"), "HTTP listen address.")
	tlsCert    = flag.String("tls_cert", "", "TLS certificate file. Enables HTTPS/WSS together with -tls_key.")
	tlsKey     = flag.String("tls_key", "", "TLS private key file.")
	authToken  = flag.String("auth_token", config.Env("MARUSYA_AUTH_TOKEN", ""), "Bearer token devices must present. Empty disables auth.")
	panelPass  = flag.String("panel_password", config.Env("MARUSYA_PANEL_PASSWORD", ""), "Password of the web panel at /panel/. Empty disables the panel.")
	wakeURL    = flag.String("wake_url", config.Env("MARUSYA_WAKE_URL", ""), "WebSocket URL of the wake word spotter (deploy/wake), e.g. ws://marusya-wake:2700. Empty leaves the wake word to the speakers.")
	wakeWords  = flag.String("wake_words", config.Env("MARUSYA_WAKE_WORDS", ""), `Comma separated wake words spotted on the server, e.g. "антон, אנטון". Changeable in the web panel.`)
	haURL      = flag.String("ha_url", config.Env("MARUSYA_HA_URL", ""), "Home Assistant URL, e.g. http://homeassistant.local:8123: enables the home_find and home_control skills.")
	haToken    = flag.String("ha_token", config.Env("MARUSYA_HA_TOKEN", ""), "Home Assistant long-lived access token.")
	haDomains  = flag.String("ha_domains", config.Env("MARUSYA_HA_DOMAINS", ""), "Comma separated entity domains that may be controlled by voice; empty: lights, switches, covers, climate, media players, scenes, scripts... (no locks or alarms).")
	spID       = flag.String("spotify_client_id", config.Env("MARUSYA_SPOTIFY_CLIENT_ID", ""), "Client ID of a Spotify app (developer.spotify.com): enables the spotify_play skill. The server only searches; speakers play in spotifyd, signed in to their owner's Premium account.")
	spSecret   = flag.String("spotify_client_secret", config.Env("MARUSYA_SPOTIFY_CLIENT_SECRET", ""), "Client secret of the Spotify app.")
	spMarket   = flag.String("spotify_market", config.Env("MARUSYA_SPOTIFY_MARKET", ""), "ISO country code, e.g. IL: find only music playable there. Empty searches every country.")
	profiles   = flag.String("profiles_file", config.Env("MARUSYA_PROFILES_FILE", ""), "JSON file with per-speaker profiles (region, languages, own token), editable in the web panel. Empty keeps them in memory.")
	mediaOn    = flag.Bool("media", config.EnvBool("MARUSYA_MEDIA", true), "Let speakers play YouTube and other sites (GET /v1/media). Needs yt-dlp and ffmpeg on the server.")
	ytdlpArgs  = flag.String("ytdlp_args", config.Env("MARUSYA_YTDLP_ARGS", ""), `Extra yt-dlp arguments, e.g. "--js-runtimes node" or "--cookies /data/cookies.txt".`)
	alarmsFile = flag.String("alarms_file", config.Env("MARUSYA_ALARMS_FILE", ""), "JSON file with the alarms and timers of the speakers. Empty keeps them in memory.")
	settings   = flag.String("settings_file", config.Env("MARUSYA_SETTINGS_FILE", ""), "File that keeps the settings changed in the web panel across restarts. Empty keeps them in memory.")

	providerName = flag.String("provider", config.Env("MARUSYA_PROVIDER", "gemini"), `Model backend: "gemini" (Live API, speech to speech), "claude" (Claude + Gemini speech recognition/TTS) or "echo" (loopback, for testing).`)
	model        = flag.String("gemini_model", config.Env("GEMINI_MODEL", gemini.DefaultModel), "Gemini Live API model.")
	voice        = flag.String("gemini_voice", config.Env("GEMINI_VOICE", "Kore"), "Prebuilt voice name (Kore, Puck, Charon, Aoede, Fenrir, ...).")
	apiKey       = flag.String("gemini_api_key", "", "Gemini API key. Defaults to $GEMINI_API_KEY / $GOOGLE_API_KEY.")
	vertex       = flag.Bool("vertex", config.EnvBool("GOOGLE_GENAI_USE_VERTEXAI", false), "Use Vertex AI instead of the Gemini Developer API.")
	project      = flag.String("gcp_project", config.Env("GOOGLE_CLOUD_PROJECT", ""), "Vertex AI project.")
	location     = flag.String("gcp_location", config.Env("GOOGLE_CLOUD_LOCATION", "us-central1"), "Vertex AI location.")
	claudeModel  = flag.String("claude_model", config.Env("MARUSYA_CLAUDE_MODEL", claude.DefaultModel), "Claude model for -provider claude.")
	claudeKey    = flag.String("claude_api_key", "", "Claude API key. Defaults to $ANTHROPIC_API_KEY (or an 'ant auth login' profile).")
	claudeEffort = flag.String("claude_effort", config.Env("MARUSYA_CLAUDE_EFFORT", "low"), "Claude effort: low (fastest), medium, high, ...")
	sttModel     = flag.String("stt_model", config.Env("MARUSYA_STT_MODEL", geminispeech.DefaultSTTModel), "Gemini model that transcribes speech for -provider claude.")
	ttsModel     = flag.String("tts_model", config.Env("MARUSYA_TTS_MODEL", geminispeech.DefaultTTSModel), "Gemini TTS model that speaks replies for -provider claude.")
	askTTS       = flag.String("ask_tts", config.Env("MARUSYA_ASK_TTS", "live"), `How announcements (/v1/ask) are spoken: "live" (a Live API model reads the text, Live quota) or "tts" (the -tts_model, small daily quota on the free tier).`)
	googleSearch = flag.Bool("google_search", config.EnvBool("GEMINI_GOOGLE_SEARCH", true), "Ground answers with Google Search.")

	language       = flag.String("language", config.Env("MARUSYA_LANGUAGE", "ru-RU"), "Default speech language (BCP-47).")
	extraLanguages = flag.String("extra_languages", config.Env("MARUSYA_EXTRA_LANGUAGES", ""), `Other languages users speak, comma separated BCP-47 codes, e.g. "he-IL" (recognition hints).`)
	promptFile     = flag.String("system_prompt_file", "", "File with a custom system instruction.")
	idleTimeout    = flag.Duration("idle_timeout", config.EnvDuration("MARUSYA_IDLE_TIMEOUT", 2*time.Minute), "Keep the model session (and conversation context) open this long after the last turn.")
	cmdTimeout     = flag.Duration("command_timeout", 30*time.Second, "How long a device may take to execute a command.")
	agentDir       = flag.String("agent_dir", config.Env("MARUSYA_AGENT_DIR", ""), `Directory with agent binaries (marusya-agent-linux-<arch>) served at /v1/agent/<arch>. Empty disables.`)
	defLocation    = flag.String("default_location", config.Env("MARUSYA_DEFAULT_LOCATION", ""), `Speaker location for devices that do not report one, e.g. "Ашдод".`)
	defTimeZone    = flag.String("default_timezone", config.Env("MARUSYA_DEFAULT_TIMEZONE", localZone()), "IANA time zone for devices that do not report one. Defaults to this machine's zone.")
	vadMode        = flag.String("vad", config.Env("MARUSYA_VAD", "server"), `End-of-speech detection: "server" (the model, robust) or "device" (the speaker's energy endpointer).`)
	enSkills       = flag.Bool("skills", config.EnvBool("MARUSYA_SKILLS", true), "Enable server-side skills: time_now and weather_forecast (Open-Meteo, no key needed).")
	healthcheck    = flag.Bool("healthcheck", false, "Probe /healthz of the server at -listen and exit 0 if it is healthy (for container health checks).")
	logLevel       = flag.String("log_level", config.Env("MARUSYA_LOG_LEVEL", "info"), "Log level: debug, info, warn, error.")
	logJSON        = flag.Bool("log_json", false, "Emit JSON logs.")
)

func main() {
	flag.Parse()
	if *healthcheck {
		os.Exit(checkHealth())
	}
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "marusya-server:", err)
		os.Exit(1)
	}
}

func run() error {
	log, err := logging.New(*logLevel, *logJSON)
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	sp := newSpeech(ctx, log)
	p, err := newProvider(ctx, sp, log)
	if err != nil {
		return err
	}

	var prompt string
	if *promptFile != "" {
		b, err := os.ReadFile(*promptFile)
		if err != nil {
			return err
		}
		prompt = string(b)
	}
	if *authToken == "" {
		log.Warn("authentication is disabled; set -auth_token for any non-local deployment")
	}

	if *vadMode != "server" && *vadMode != "device" {
		fmt.Fprintf(os.Stderr, "invalid -vad %q\n", *vadMode)
		os.Exit(2)
	}
	var set skills.Set
	if *enSkills {
		geo := &openmeteo.Client{}
		set = skills.Set{&clock.Skill{Geo: geo}, &weather.Skill{Geo: geo}}
	}
	if *haURL != "" {
		ha := &homeassistant.Client{URL: *haURL, Token: *haToken}
		if *haDomains != "" {
			ha.Domains = strings.FieldsFunc(*haDomains, func(r rune) bool { return r == ',' || r == ' ' })
		}
		set = append(set, ha.Skills()...)
		log.Info("Home Assistant skills enabled", "url", *haURL, "domains", cmp.Or(*haDomains, strings.Join(homeassistant.DefaultDomains, ",")))
	}
	if *spID != "" {
		sp := &spotify.Client{ClientID: *spID, ClientSecret: *spSecret, Market: *spMarket}
		set = append(set, sp.Skills()...)
		log.Info("Spotify skill enabled", "market", cmp.Or(*spMarket, "any"))
	}

	cfg := server.Config{
		Provider:          p,
		AuthToken:         *authToken,
		SystemInstruction: prompt,
		Language:          *language,
		ExtraLanguages:    *extraLanguages,
		IdleTimeout:       *idleTimeout,
		CommandTimeout:    *cmdTimeout,
		AgentDir:          *agentDir,
		Skills:            set,
		DefaultLocation:   *defLocation,
		DefaultTimeZone:   *defTimeZone,
		ServerVAD:         *vadMode == "server",
		PanelPassword:     *panelPass,
		WakeURL:           *wakeURL,
		WakeWords:         *wakeWords,
		SettingsFile:      *settings,
		ProfilesFile:      *profiles,
		AlarmsFile:        *alarmsFile,
		Version:           version,
		Logger:            log,
	}
	if sp != nil {
		cfg.Synthesizer, cfg.Recognizer = sp, sp
		switch *askTTS {
		case "live":
			cfg.Synthesizer = sp.Live(*model)
		case "tts":
		default:
			fmt.Fprintf(os.Stderr, "invalid -ask_tts %q\n", *askTTS)
			os.Exit(2)
		}
	}
	if *mediaOn {
		src := &ytdlp.Source{Args: strings.Fields(*ytdlpArgs), Logger: log}
		if err := src.Available(); err != nil {
			log.Warn("media playback disabled", "err", err)
		} else {
			cfg.Media = src
			log.Info("media playback enabled", "ytdlp_args", *ytdlpArgs)
		}
	}
	srv := server.New(cfg)
	hs := &http.Server{
		Addr:              *listen,
		Handler:           srv,
		ReadHeaderTimeout: 10 * time.Second,
		BaseContext:       func(net.Listener) context.Context { return ctx },
	}

	errc := make(chan error, 1)
	go func() {
		log.Info("listening", "version", version, "addr", *listen, "provider", p.Name())
		if *panelPass != "" {
			log.Info("web panel enabled", "path", "/panel/")
		} else {
			log.Info("web panel disabled; set MARUSYA_PANEL_PASSWORD to enable it")
		}
		if *tlsCert != "" {
			errc <- hs.ListenAndServeTLS(*tlsCert, *tlsKey)
		} else {
			errc <- hs.ListenAndServe()
		}
	}()

	select {
	case err := <-errc:
		return err
	case <-ctx.Done():
	}
	log.Info("shutting down")
	sctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := hs.Shutdown(sctx); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

// newSpeech returns Gemini speech recognition and synthesis, used by the
// claude provider and by announcements (POST /v1/ask); nil without a Gemini
// Developer API key.
func newSpeech(ctx context.Context, log *slog.Logger) *geminispeech.Speech {
	if *vertex {
		log.Info("announcements (/v1/ask) are disabled: Gemini speech needs a Developer API key, not Vertex AI")
		return nil
	}
	sp, err := geminispeech.New(ctx, geminispeech.Config{
		APIKey: *apiKey, STTModel: *sttModel, TTSModel: *ttsModel, Voice: *voice,
	})
	if err != nil {
		log.Warn("Gemini speech unavailable; announcements (/v1/ask) are disabled", "err", err)
		return nil
	}
	return sp
}

func newProvider(ctx context.Context, sp *geminispeech.Speech, log *slog.Logger) (provider.Provider, error) {
	switch *providerName {
	case "gemini":
		return gemini.New(ctx, gemini.Config{
			APIKey:       *apiKey,
			VertexAI:     *vertex,
			Project:      *project,
			Location:     *location,
			Model:        *model,
			Voice:        *voice,
			GoogleSearch: *googleSearch,
		})
	case "claude":
		if sp == nil {
			return nil, errors.New("-provider claude needs Gemini speech (a Gemini Developer API key)")
		}
		return claude.New(claude.Config{
			APIKey:      *claudeKey,
			Model:       *claudeModel,
			Effort:      *claudeEffort,
			WebSearch:   *googleSearch,
			Recognizer:  sp,
			Synthesizer: sp,
			Logger:      log,
		})
	case "echo":
		return echo.Provider{}, nil
	}
	return nil, fmt.Errorf("unknown provider %q", *providerName)
}

// localZone returns the IANA name of this machine's time zone, if known.
func localZone() string {
	if tz := os.Getenv("TZ"); tz != "" {
		return strings.TrimPrefix(tz, ":")
	}
	if p, err := filepath.EvalSymlinks("/etc/localtime"); err == nil {
		if i := strings.LastIndex(p, "zoneinfo/"); i >= 0 {
			return p[i+len("zoneinfo/"):]
		}
	}
	return ""
}

// checkHealth probes the local server's /healthz endpoint.
func checkHealth() int {
	_, port, err := net.SplitHostPort(*listen)
	if err != nil {
		fmt.Fprintln(os.Stderr, "healthcheck:", err)
		return 1
	}
	scheme := "http"
	if *tlsCert != "" {
		scheme = "https"
	}
	c := &http.Client{Timeout: 3 * time.Second}
	resp, err := c.Get(scheme + "://127.0.0.1:" + port + "/healthz")
	if err != nil {
		fmt.Fprintln(os.Stderr, "healthcheck:", err)
		return 1
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		fmt.Fprintln(os.Stderr, "healthcheck:", resp.Status)
		return 1
	}
	return 0
}
