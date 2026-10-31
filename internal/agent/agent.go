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

// Package agent implements the on-device assistant: it waits for the wake
// word, streams the user's request to the server, plays back the reply and
// executes device commands.
package agent

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/yawaflua/marusya-gemini/internal/audio"
	"github.com/yawaflua/marusya-gemini/internal/controller"
	"github.com/yawaflua/marusya-gemini/internal/indicator"
	"github.com/yawaflua/marusya-gemini/internal/models"
	"github.com/yawaflua/marusya-gemini/internal/transport"
	"github.com/yawaflua/marusya-gemini/internal/vad"
	"github.com/yawaflua/marusya-gemini/internal/wakeword"
)

// Config configures an [Agent].
type Config struct {
	// DeviceID identifies the speaker on the server.
	DeviceID string
	// ServerURL is the ws(s):// URL of the server streaming endpoint.
	ServerURL string
	// Token authenticates the device.
	Token string
	// Language is a BCP-47 code, e.g. "ru-RU".
	Language string
	// Location is where the speaker is, e.g. "Ashdod" (for time/weather).
	Location string
	// TimeZone is the IANA zone of the speaker, e.g. "Asia/Jerusalem".
	TimeZone string

	// VAD tunes the end-of-utterance detection.
	VAD vad.Config
	// PreRoll is how much audio before the detected speech onset is sent.
	PreRoll time.Duration
	// ResponseTimeout aborts a turn if the server does not answer.
	ResponseTimeout time.Duration

	// Earcons enables listen/error sounds.
	Earcons bool
	// FollowUp keeps listening after a reply so the user can continue the
	// conversation without repeating the wake word.
	FollowUp bool
	// FollowUpTimeout is how long to wait for a follow-up utterance.
	FollowUpTimeout time.Duration
	// AnswerTimeout is how long to wait for the answer to a question the
	// server announced (see models.TypeAnnounce).
	AnswerTimeout time.Duration
	// DuckMedia pauses the media player while the assistant is active.
	DuckMedia bool
	// PluginsDir is the directory of Deps.Plugins. With it the agent
	// installs plugins from the plugin catalog through the server (see
	// models.TypeCatalogPlugins); the plugins must implement Rescan().
	PluginsDir string
	// OnConnected, if set, runs in its own goroutine after every
	// (re)connection to the server, with a context ending on disconnect.
	OnConnected func(ctx context.Context)
}

func (c *Config) setDefaults() {
	if c.VAD == (vad.Config{}) {
		c.VAD = vad.DefaultConfig()
	}
	if c.PreRoll == 0 {
		c.PreRoll = 300 * time.Millisecond
	}
	if c.ResponseTimeout == 0 {
		c.ResponseTimeout = 30 * time.Second
	}
	if c.FollowUpTimeout == 0 {
		c.FollowUpTimeout = 4 * time.Second
	}
	if c.AnswerTimeout == 0 {
		c.AnswerTimeout = 10 * time.Second
	}
}

// Deps are the hardware abstractions the agent drives.
type Deps struct {
	Capture    audio.Capturer
	Player     audio.Player
	Detector   wakeword.Detector
	Dispatcher *controller.Dispatcher
	// Indicator shows the assistant state; optional.
	Indicator indicator.Indicator
	// Logs are device log records to forward to the server; optional
	// (see logging.Forwarder).
	Logs <-chan models.Log
	// Plugins run on the speaker; optional.
	Plugins Plugins
}

// Agent is the device-side assistant.
type Agent struct {
	cfg        Config
	log        *slog.Logger
	capture    audio.Capturer
	player     audio.Player
	detector   wakeword.Detector
	dispatcher *controller.Dispatcher
	indicator  indicator.Indicator
	logs       <-chan models.Log
	plugins    Plugins
	conn       atomic.Pointer[transport.Conn] // the current connection

	frames chan []byte
	wakes  chan wakeword.Detection
	muted  atomic.Bool
	busy   atomic.Bool // not idle: listening, thinking or speaking
	alarm  *alarmClock
	mutes  chan bool

	catMu  sync.Mutex   // syncs of the catalog plugins
	catGen atomic.Int64 // the newest list of catalog plugins
}

// Busy reports whether the assistant is using the speaker (listening,
// thinking or speaking).
func (a *Agent) Busy() bool { return a.busy.Load() }

// SetMicMuted mutes or unmutes the microphone. While muted the wake word
// detector only receives silence, wakes are ignored and a request being
// recorded is cancelled.
func (a *Agent) SetMicMuted(muted bool) {
	a.muted.Store(muted)
	a.log.Info("microphone", "muted", muted)
	select {
	case a.mutes <- muted:
	default:
	}
}

// New creates an agent.
func New(cfg Config, log *slog.Logger, deps Deps) *Agent {
	cfg.setDefaults()
	if log == nil {
		log = slog.Default()
	}
	if deps.Indicator == nil {
		deps.Indicator = indicator.Nop{}
	}
	a := &Agent{
		cfg:        cfg,
		log:        log,
		capture:    deps.Capture,
		player:     deps.Player,
		detector:   deps.Detector,
		dispatcher: deps.Dispatcher,
		indicator:  deps.Indicator,
		logs:       deps.Logs,
		plugins:    deps.Plugins,
		frames:     make(chan []byte, 64),
		wakes:      make(chan wakeword.Detection, 4),
		mutes:      make(chan bool, 4),
	}
	a.alarm = newAlarmClock(a)
	return a
}

// Run starts audio capture and wake word detection and keeps a connection to
// the server until ctx is done.
func (a *Agent) Run(ctx context.Context) error {
	if err := a.capture.Format().Validate(); err != nil {
		return err
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	mic := make(chan []byte, 64)
	wakeAudio := make(chan []byte, 64)
	go func() {
		if err := a.capture.Run(ctx, mic); err != nil {
			a.log.Error("audio capture failed", "err", err)
			cancel()
		}
	}()
	go a.fanout(ctx, mic, wakeAudio)
	a.alarm.start(ctx)
	go func() {
		if err := a.detector.Run(ctx, wakeAudio, a.wakes); err != nil {
			a.log.Error("wake word detector failed", "err", err)
			cancel()
		}
	}()

	backoff := time.Second
	for {
		start := time.Now()
		err := a.serve(ctx)
		if ctx.Err() != nil {
			return nil
		}
		if time.Since(start) > time.Minute {
			backoff = time.Second
		}
		a.log.Warn("disconnected from server", "err", err, "retry_in", backoff)
		a.indicator.Show(indicator.Disconnected)
		if !a.waitReconnect(ctx, backoff) {
			return nil
		}
		backoff = min(backoff*2, 30*time.Second)
	}
}

// fanout copies microphone frames to the wake word detector and the session.
func (a *Agent) fanout(ctx context.Context, mic <-chan []byte, wake chan<- []byte) {
	feedWake := a.detector.WantsAudio()
	var silence []byte
	for {
		select {
		case <-ctx.Done():
			return
		case f := <-mic:
			if a.muted.Load() {
				if len(silence) != len(f) {
					silence = make([]byte, len(f))
				}
				f = silence
			}
			if feedWake {
				select {
				case wake <- f:
				default:
				}
			}
			select {
			case a.frames <- f:
			default:
			}
		}
	}
}

// waitReconnect sleeps for d, answering wake words with an error sound.
func (a *Agent) waitReconnect(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return false
		case <-t.C:
			return true
		case <-a.frames:
		case <-a.mutes:
		case <-a.wakes:
			if a.alarm.stop() {
				continue
			}
			if a.muted.Load() {
				continue
			}
			a.playEarcon(ctx, audio.EarconError)
			a.indicator.Show(indicator.Disconnected)
		}
	}
}

// serve runs one connection to the server.
func (a *Agent) serve(ctx context.Context) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	dctx, dcancel := context.WithTimeout(ctx, 10*time.Second)
	conn, err := transport.Dial(dctx, a.cfg.ServerURL, a.cfg.Token)
	dcancel()
	if err != nil {
		return err
	}
	defer conn.Close("agent shutdown")
	a.conn.Store(conn)
	defer a.conn.CompareAndSwap(conn, nil)

	ready, err := a.handshake(ctx, conn)
	if err != nil {
		return err
	}
	a.log.Info("connected to server", "session", ready.SessionID, "output_rate", ready.OutputFormat.SampleRateHertz, "server_vad", ready.ServerVAD)
	a.indicator.Show(indicator.Idle)
	if a.cfg.OnConnected != nil {
		go a.cfg.OnConnected(ctx)
	}

	go conn.KeepAlive(ctx, 20*time.Second)
	s := newSession(a, conn, ready)
	return s.run(ctx)
}

func (a *Agent) handshake(ctx context.Context, conn *transport.Conn) (*models.Ready, error) {
	hctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	err := conn.Send(hctx, &models.Message{
		Type: models.TypeHello,
		Hello: &models.Hello{
			ProtocolVersion: models.ProtocolVersion,
			DeviceID:        a.cfg.DeviceID,
			InputFormat:     a.capture.Format(),
			Capabilities:    append(a.dispatcher.Capabilities(), models.ActionConversationEnd),
			Language:        a.cfg.Language,
			Location:        a.cfg.Location,
			TimeZone:        a.cfg.TimeZone,
			Features:        a.features(),
			PluginTools:     a.pluginTools(),
		},
	})
	if err != nil {
		return nil, err
	}
	f, err := conn.Read(hctx)
	if err != nil {
		return nil, fmt.Errorf("read ready: %w", err)
	}
	switch {
	case f.Message == nil:
		return nil, errors.New("unexpected audio during handshake")
	case f.Message.Type == models.TypeError:
		return nil, f.Message.Error
	case f.Message.Type != models.TypeReady:
		return nil, fmt.Errorf("unexpected %s during handshake", f.Message.Type)
	}
	if err := f.Message.Ready.OutputFormat.Validate(); err != nil {
		return nil, err
	}
	return f.Message.Ready, nil
}

// playEarcon plays e asynchronously and returns its duration.
func (a *Agent) playEarcon(ctx context.Context, e audio.Earcon) time.Duration {
	if !a.cfg.Earcons {
		return 0
	}
	f := models.DefaultOutputFormat
	pcm := e.Render(f)
	st, err := a.player.Open(ctx, f)
	if err != nil {
		a.log.Warn("play earcon", "err", err)
		return 0
	}
	st.Write(pcm)
	go st.Drain()
	return f.Duration(len(pcm))
}
