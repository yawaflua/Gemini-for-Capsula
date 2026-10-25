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
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/yawaflua/marusya-gemini/internal/models"
	"github.com/yawaflua/marusya-gemini/internal/provider"
	"github.com/yawaflua/marusya-gemini/internal/skills"
	"github.com/yawaflua/marusya-gemini/internal/tools"
	"github.com/yawaflua/marusya-gemini/internal/transport"
	"github.com/yawaflua/marusya-gemini/internal/wake"
)

const (
	helloTimeout  = 10 * time.Second
	writeTimeout  = 10 * time.Second
	pingInterval  = 20 * time.Second
	connectTimout = 15 * time.Second
)

// deviceSession bridges one device stream to a lazily opened provider
// session.
//
// The provider session is opened on the first turn and kept open for
// Config.IdleTimeout after the last turn, so the model remembers the context
// of the conversation between quick follow-up questions.
type deviceSession struct {
	id        string
	conn      *transport.Conn
	srv       *Server
	cfg       *Config
	log       *slog.Logger
	ctx       context.Context
	hello     *models.Hello
	vad       bool // the provider detects the end of speech
	remote    string
	connected time.Time

	token string // the Bearer token the device connected with

	mu         sync.Mutex
	ps         provider.Session
	retiring   bool          // ps received GoAway and must be replaced on next turn
	opened     time.Time     // when ps was opened
	connecting chan struct{} // closed when a pending connection attempt ends
	turnID     string
	inTurn     bool   // user audio is being streamed
	awaiting   bool   // the user finished speaking, a reply is expected
	suppress   bool   // the turn was cancelled; drop model output until it completes
	source     string // who started the current turn, SourceVoice or SourcePanel
	ending     bool   // conversation.end succeeded; forget the context after this turn
	output     bool   // the model produced audio, text or a tool call this turn
	retryText  string // a panel request to resend once if it gets an empty reply
	turns      int
	lastTurn   time.Time
	idle       *time.Timer
	pending    map[string]chan *models.CommandResult
	wake       *wake.Stream // spotter stream for the wake word, see runWake
	wakeState  string
	wakeKick   chan struct{}
	alarmKick  chan struct{} // the alarms changed, see runAlarms
	ask        *pendingAsk   // an announcement being delivered, see ask.go

	// heard and said accumulate the transcripts of the current turn, which
	// are logged as one line each when it completes.
	heard, said strings.Builder
}

func newDeviceSession(id string, conn *transport.Conn, srv *Server, remote string, log *slog.Logger) *deviceSession {
	return &deviceSession{
		id:        id,
		conn:      conn,
		srv:       srv,
		cfg:       &srv.cfg,
		log:       log,
		remote:    remote,
		connected: time.Now(),
		pending:   make(map[string]chan *models.CommandResult),
		wakeKick:  make(chan struct{}, 1),
		alarmKick: make(chan struct{}, 1),
		wakeState: wakeOff,
	}
}

// DeviceInfo describes a connected device in the web panel.
type DeviceInfo struct {
	Session      string           `json:"session"`
	DeviceID     string           `json:"device_id"`
	Remote       string           `json:"remote"`
	Connected    time.Time        `json:"connected"`
	Language     string           `json:"language,omitempty"`
	Location     string           `json:"location,omitempty"`
	TimeZone     string           `json:"timezone,omitempty"`
	Capabilities []models.Action  `json:"capabilities"`
	Features     []models.Feature `json:"features"`
	// State is "idle", "listening" (user audio is streamed) or "thinking"
	// (a reply is expected).
	State        string    `json:"state"`
	ModelSession bool      `json:"model_session"`
	Turns        int       `json:"turns"`
	LastTurn     time.Time `json:"last_turn,omitzero"`
	CanSay       bool      `json:"can_say"`
	// Effective is what the device runs with: its profile over its hello
	// over the global settings.
	Effective Effective `json:"effective"`
	// Wake is the state of server-side wake word spotting: "off", "on" or
	// "unavailable"; empty if the server cannot do it for this device.
	Wake string `json:"wake,omitempty"`
}

func (d *deviceSession) info() DeviceInfo {
	d.mu.Lock()
	defer d.mu.Unlock()
	state := "idle"
	switch {
	case d.ask != nil:
		state = "announcing"
	case d.inTurn:
		state = "listening"
	case d.awaiting:
		state = "thinking"
	}
	caps := d.hello.Capabilities
	if len(caps) == 0 {
		for _, t := range tools.All() {
			caps = append(caps, t.Action)
		}
	}
	wakeState := ""
	if d.serverWake() {
		wakeState = d.wakeState
	}
	return DeviceInfo{
		Effective:    d.effective(),
		Wake:         wakeState,
		Session:      d.id,
		DeviceID:     d.hello.DeviceID,
		Remote:       d.remote,
		Connected:    d.connected,
		Language:     d.hello.Language,
		Location:     d.hello.Location,
		TimeZone:     d.hello.TimeZone,
		Capabilities: caps,
		Features:     d.hello.Features,
		State:        state,
		ModelSession: d.ps != nil,
		Turns:        d.turns,
		LastTurn:     d.lastTurn,
		CanSay:       provider.AcceptsText(d.cfg.Provider) && slices.Contains(d.hello.Features, models.FeatureRemoteTurn),
	}
}

func (d *deviceSession) run(ctx context.Context) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	d.ctx = ctx
	defer d.shutdown()

	if err := d.handshake(ctx); err != nil {
		return err
	}
	d.srv.register(d)
	defer d.srv.unregister(d)
	go d.conn.KeepAlive(ctx, pingInterval)
	if d.serverWake() {
		go d.runWake(ctx)
	}
	if d.supportsFeature(models.FeatureAlarms) {
		go d.runAlarms()
	}

	for {
		f, err := d.conn.Read(ctx)
		if err != nil {
			if transport.IsNormalClosure(err) {
				return nil
			}
			return err
		}
		if f.Message == nil {
			d.onAudio(f.Audio)
			continue
		}
		d.onMessage(f.Message)
	}
}

func (d *deviceSession) handshake(ctx context.Context) error {
	hctx, cancel := context.WithTimeout(ctx, helloTimeout)
	defer cancel()
	f, err := d.conn.Read(hctx)
	if err != nil {
		return fmt.Errorf("read hello: %w", err)
	}
	if f.Message == nil || f.Message.Type != models.TypeHello {
		return errors.New("expected hello")
	}
	h := f.Message.Hello
	if h.ProtocolVersion != models.ProtocolVersion {
		err := fmt.Errorf("unsupported protocol version %d, want %d", h.ProtocolVersion, models.ProtocolVersion)
		d.sendError("", models.ErrorCodeInvalidArgument, err)
		return err
	}
	if err := h.InputFormat.Validate(); err != nil {
		d.sendError("", models.ErrorCodeInvalidArgument, err)
		return err
	}
	p, _ := d.srv.Profile(h.DeviceID)
	if !d.tokenOK(h.DeviceID, p) {
		err := fmt.Errorf("device %q is not allowed with this token", h.DeviceID)
		d.sendError("", models.ErrorCodeInvalidArgument, err)
		return err
	}
	d.hello = h
	d.vad = d.cfg.ServerVAD && provider.SupportsServerVAD(d.cfg.Provider)
	d.log = d.log.With("device", h.DeviceID)
	d.log.Info("device connected", "capabilities", len(h.Capabilities), "language", h.Language,
		"location", h.Location, "timezone", h.TimeZone)
	defer d.prewarm()
	return d.send(&models.Message{
		Type: models.TypeReady,
		Ready: &models.Ready{
			SessionID:    d.id,
			OutputFormat: d.cfg.Provider.OutputFormat(),
			ServerVAD:    d.vad,
		},
	})
}

func (d *deviceSession) shutdown() {
	d.mu.Lock()
	ps := d.ps
	d.ps = nil
	if d.idle != nil {
		d.idle.Stop()
	}
	for id, ch := range d.pending {
		close(ch)
		delete(d.pending, id)
	}
	d.mu.Unlock()
	if ps != nil {
		ps.Close()
	}
}

func (d *deviceSession) send(m *models.Message) error {
	ctx, cancel := context.WithTimeout(d.ctx, writeTimeout)
	defer cancel()
	return d.conn.Send(ctx, m)
}

func (d *deviceSession) sendError(turnID string, code models.ErrorCode, err error) {
	d.send(&models.Message{
		Type:   models.TypeError,
		TurnID: turnID,
		Error:  &models.Error{Code: code, Message: err.Error()},
	})
}

func (d *deviceSession) onMessage(m *models.Message) {
	switch m.Type {
	case models.TypeTurnStart:
		if !d.askTurnStart(m) {
			d.startTurn(m.TurnID)
		}
	case models.TypeTurnEnd:
		if !d.askTurnEnd(m, false) {
			d.endTurn(false)
		}
	case models.TypeTurnCancel:
		if !d.askTurnEnd(m, true) {
			d.endTurn(true)
		}
	case models.TypeCommandResult:
		d.resolve(m.CommandResult)
	case models.TypeLog:
		d.deviceLog(m.Log)
	default:
		d.log.Warn("unexpected message", "type", m.Type)
	}
}

// deviceLog prints a record forwarded by the device.
func (d *deviceSession) deviceLog(l *models.Log) {
	var level slog.Level
	if err := level.UnmarshalText([]byte(l.Level)); err != nil {
		level = slog.LevelWarn
	}
	args := []any{"device_time", l.Time.Format(time.RFC3339)}
	for k, v := range l.Attrs {
		args = append(args, slog.String("device."+k, v))
	}
	d.log.Log(d.ctx, level, "device: "+l.Message, args...)
}

func (d *deviceSession) onAudio(pcm []byte) {
	if d.askAudio(pcm) {
		return // the answer to an announcement
	}
	d.mu.Lock()
	ps, in := d.ps, d.inTurn
	d.mu.Unlock()
	if !in {
		d.feedWake(pcm) // idle audio, streamed for the wake word
		return
	}
	if ps == nil {
		return
	}
	if err := ps.SendAudio(pcm); err != nil {
		d.log.Warn("send audio to provider", "err", err)
	}
}

// session returns the open provider session, connecting a new one if
// needed. Concurrent callers share one connection attempt.
func (d *deviceSession) session() (provider.Session, error) {
	for {
		d.mu.Lock()
		if d.ps != nil && !d.retiring {
			ps := d.ps
			d.mu.Unlock()
			return ps, nil
		}
		if ch := d.connecting; ch != nil {
			d.mu.Unlock()
			select {
			case <-ch:
				continue
			case <-d.ctx.Done():
				return nil, d.ctx.Err()
			}
		}
		old := d.ps
		d.ps, d.retiring = nil, false
		ch := make(chan struct{})
		d.connecting = ch
		d.mu.Unlock()
		if old != nil {
			old.Close()
		}

		ps, err := d.connect()
		d.mu.Lock()
		d.connecting = nil
		if err == nil && d.ctx.Err() != nil {
			// The device left while connecting; shutdown already ran.
			ps.Close()
			ps, err = nil, d.ctx.Err()
		}
		if err == nil {
			d.ps, d.opened = ps, time.Now()
		}
		d.mu.Unlock()
		close(ch)
		if err != nil {
			return nil, err
		}
		go d.pump(ps)
		return ps, nil
	}
}

func (d *deviceSession) connect() (provider.Session, error) {
	set := d.srv.Settings()
	e := d.effective()
	lang := e.Language
	ctx, cancel := context.WithTimeout(d.ctx, connectTimout)
	defer cancel()
	start := time.Now()
	ps, err := d.cfg.Provider.Connect(ctx, provider.SessionOptions{
		SystemInstruction: set.SystemInstruction + d.deviceContext() + languagesNote(lang, e.ExtraLanguages),
		Language:          lang,
		ExtraLanguages:    extraLanguages(lang, e.ExtraLanguages),
		InputFormat:       d.hello.InputFormat,
		Tools:             append(tools.For(d.hello.Capabilities), d.cfg.Skills.Tools()...),
		ServerVAD:         d.vad,
	})
	if err != nil {
		return nil, err
	}
	d.log.Info("provider session opened", "provider", d.cfg.Provider.Name(), "latency", time.Since(start))
	d.srv.events.devicesChanged()
	return ps, nil
}

// prewarm opens a provider session in the background so the next turn does
// not wait for the connection (1-3 s for Gemini Live). Failures are only
// logged: the next turn will try again.
func (d *deviceSession) prewarm() {
	if d.ctx.Err() != nil {
		return
	}
	go func() {
		if _, err := d.session(); err != nil && d.ctx.Err() == nil {
			d.log.Warn("prewarm provider session", "err", err)
		}
	}()
}

// tokenOK reports whether the connection token fits the device with
// profile p: a profile token binds the device ID and the token together;
// the shared token admits any device without a token of its own, and the
// owner's devices with one.
func (d *deviceSession) tokenOK(deviceID string, p Profile) bool {
	if id, ok := d.srv.deviceForToken(d.token); ok {
		return id == deviceID
	}
	if p.Token == "" {
		return true // authorized() admitted the shared token
	}
	return d.cfg.AuthToken != "" && d.srv.authorizedToken(d.token)
}

// Effective is the configuration a device runs with: its [Profile] over
// what it reported in its hello over the global [Settings].
type Effective struct {
	Language       string `json:"language"`
	ExtraLanguages string `json:"extra_languages,omitempty"`
	Location       string `json:"location,omitempty"`
	TimeZone       string `json:"timezone,omitempty"`
}

func (d *deviceSession) effective() Effective {
	set := d.srv.Settings()
	e := Effective{
		Language:       set.Language,
		ExtraLanguages: set.ExtraLanguages,
		Location:       set.DefaultLocation,
		TimeZone:       set.DefaultTimeZone,
	}
	over := func(dst *string, v string) {
		if v != "" {
			*dst = v
		}
	}
	over(&e.Language, d.hello.Language)
	over(&e.Location, d.hello.Location)
	over(&e.TimeZone, d.hello.TimeZone)
	if p, ok := d.srv.Profile(d.hello.DeviceID); ok {
		over(&e.Language, p.Language)
		over(&e.ExtraLanguages, p.ExtraLanguages)
		over(&e.Location, p.Location)
		over(&e.TimeZone, p.TimeZone)
	}
	return e
}

// device describes the speaker to server-side skills.
func (d *deviceSession) device() skills.Device {
	e := d.effective()
	dev := skills.Device{Location: e.Location, TimeZone: e.TimeZone, Language: e.Language}
	if d.hello != nil {
		dev.ID, dev.Features = d.hello.DeviceID, d.hello.Features
	}
	return dev
}

// deviceContext tells the model where the speaker is and what time it is.
func (d *deviceSession) deviceContext() string {
	dev := d.device()
	var b strings.Builder
	b.WriteString("\n\nКонтекст устройства:")
	if dev.Location != "" {
		fmt.Fprintf(&b, "\n- Колонка находится здесь: %s. Если пользователь не назвал место, вопросы о погоде, времени и местных событиях относятся к нему.", dev.Location)
	}
	loc := time.UTC
	if dev.TimeZone != "" {
		if l, err := time.LoadLocation(dev.TimeZone); err == nil {
			loc = l
		}
	}
	fmt.Fprintf(&b, "\n- Часовой пояс: %s. Дата на момент начала разговора: %s.", loc, time.Now().In(loc).Format("2006-01-02, Monday"))
	return b.String()
}

func (d *deviceSession) startTurn(turnID string) {
	d.log.Debug("turn start", "turn", turnID)
	ps, err := d.session()
	if err == nil {
		d.mu.Lock()
		d.turnID, d.inTurn, d.awaiting, d.suppress = turnID, true, false, false
		d.source, d.ending, d.output, d.retryText = SourceVoice, false, false, ""
		d.heard.Reset()
		d.said.Reset()
		if d.idle != nil {
			d.idle.Stop()
		}
		d.mu.Unlock()
		err = ps.StartTurn()
		d.srv.events.devicesChanged()
	}
	if err != nil {
		d.log.Error("start turn", "err", err)
		d.mu.Lock()
		d.inTurn = false
		d.mu.Unlock()
		d.sendError(turnID, models.ErrorCodeUnavailable, err)
		d.send(&models.Message{Type: models.TypeTurnComplete, TurnID: turnID})
	}
}

func (d *deviceSession) endTurn(cancelled bool) {
	if cancelled {
		d.cancelTurn()
		return
	}
	d.mu.Lock()
	ps, was := d.ps, d.inTurn
	d.inTurn = false
	d.awaiting = !cancelled
	if cancelled {
		d.suppress = true
	}
	d.mu.Unlock()
	if ps == nil || !was {
		return
	}
	d.srv.events.devicesChanged()
	d.log.Debug("turn end", "cancelled", cancelled)
	if err := ps.EndTurn(); err != nil {
		d.log.Warn("end turn", "err", err)
	}
}

// cancelTurn abandons the current turn. If the provider still owes a reply,
// its session is dropped: otherwise the late reply (or just its completion)
// of the abandoned turn would be taken for the answer to the next one. The
// next turn opens a fresh session.
func (d *deviceSession) cancelTurn() {
	d.mu.Lock()
	ps := d.ps
	pending := ps != nil && (d.inTurn || d.awaiting)
	d.inTurn, d.awaiting = false, false
	if pending {
		d.ps, d.retiring, d.suppress = nil, false, false
	}
	d.mu.Unlock()
	d.srv.events.devicesChanged()
	if pending {
		d.log.Info("turn cancelled while a reply was pending; resetting the model session")
		ps.Close()
		d.prewarm()
	}
}

// speechEnded handles the end of the user's speech detected by the
// provider: it stops forwarding audio and tells the device to stop
// streaming.
func (d *deviceSession) speechEnded(turnID string) {
	d.mu.Lock()
	was := d.inTurn && d.vad
	if was {
		d.inTurn, d.awaiting = false, true
	}
	d.mu.Unlock()
	if !was {
		return
	}
	d.log.Debug("speech end detected by provider", "turn", turnID)
	d.send(&models.Message{Type: models.TypeListenEnd, TurnID: turnID})
}

// pump forwards provider events to the device until ps ends.
func (d *deviceSession) pump(ps provider.Session) {
	for e := range ps.Events() {
		d.mu.Lock()
		turnID, suppress, current := d.turnID, d.suppress, d.ps == ps
		d.mu.Unlock()
		if !current {
			continue // a dropped session draining
		}

		switch e.Kind {
		case provider.EventSpeechEnd, provider.EventAudio, provider.EventOutputTranscript, provider.EventToolCall:
			// The model only answers after the user stopped speaking.
			d.speechEnded(turnID)
		}
		switch e.Kind {
		case provider.EventAudio, provider.EventOutputTranscript, provider.EventToolCall:
			d.mu.Lock()
			d.output = true
			d.mu.Unlock()
		case provider.EventTurnComplete:
			if d.retryEmpty(ps) {
				continue
			}
		}

		var err error
		switch e.Kind {
		case provider.EventAudio:
			if !suppress {
				ctx, cancel := context.WithTimeout(d.ctx, writeTimeout)
				err = d.conn.SendAudio(ctx, e.Audio)
				cancel()
			}
		case provider.EventInputTranscript, provider.EventOutputTranscript:
			role := models.RoleUser
			if e.Kind == provider.EventOutputTranscript {
				role = models.RoleModel
			}
			d.log.Debug("transcript", "role", role, "text", e.Text)
			d.mu.Lock()
			if role == models.RoleUser {
				d.heard.WriteString(e.Text)
			} else {
				d.said.WriteString(e.Text)
			}
			d.mu.Unlock()
			if !suppress {
				err = d.send(&models.Message{
					Type:       models.TypeTranscript,
					TurnID:     turnID,
					Transcript: &models.Transcript{Role: role, Text: e.Text},
				})
			}
		case provider.EventToolCall:
			go d.runTools(ps, turnID, e.ToolCalls, suppress)
		case provider.EventToolCallCancel:
			d.cancel(e.CancelledIDs)
		case provider.EventInterrupted:
			err = d.send(&models.Message{Type: models.TypeInterrupted, TurnID: turnID})
		case provider.EventTurnComplete:
			d.mu.Lock()
			d.awaiting, d.suppress = false, false
			heard, said := strings.TrimSpace(d.heard.String()), strings.TrimSpace(d.said.String())
			d.heard.Reset()
			d.said.Reset()
			d.resetIdleLocked()
			source := d.source
			ending := d.ending && d.ps == ps
			if ending {
				// The user said goodbye: the next activation starts afresh.
				d.ending, d.ps = false, nil
			}
			if heard != "" || said != "" {
				d.turns++
				d.lastTurn = time.Now()
			}
			d.mu.Unlock()
			if heard != "" || said != "" {
				d.log.Info("turn", "turn", turnID, "user", heard, "model", said, "cancelled", suppress)
				d.srv.events.publish(Event{Kind: EventTurn, Session: d.id, Device: d.hello.DeviceID,
					Source: source, User: heard, Model: said, Cancelled: suppress})
			}
			d.srv.events.devicesChanged()
			if !suppress {
				err = d.send(&models.Message{Type: models.TypeTurnComplete, TurnID: turnID})
			}
			if ending {
				d.log.Info("conversation ended by the user; dropping the model session")
				ps.Close()
				d.prewarm()
			}
		case provider.EventGoAway:
			d.log.Info("provider requested reconnect")
			d.mu.Lock()
			if d.ps == ps {
				d.retiring = true
			}
			busy := d.inTurn || d.awaiting
			d.mu.Unlock()
			if !busy {
				d.prewarm() // replaces the retiring session
			}
		}
		if err != nil {
			d.log.Warn("forward event", "kind", e.Kind, "err", err)
		}
	}

	d.mu.Lock()
	current := d.ps == ps
	pendingReply := current && (d.inTurn || d.awaiting)
	turnID := d.turnID
	lived := time.Since(d.opened)
	if current {
		d.ps, d.inTurn, d.awaiting = nil, false, false
	}
	d.mu.Unlock()
	// The backend closed the session (connection lifetime, idle). Reopen it
	// unless it dies right away, which would mean a persistent failure.
	if current && lived > 30*time.Second {
		defer d.prewarm()
	}

	if current {
		d.srv.events.devicesChanged()
	}
	if err := ps.Err(); err != nil {
		d.log.Warn("provider session failed", "err", err)
		d.srv.events.publish(Event{Kind: EventError, Session: d.id, Device: d.hello.DeviceID,
			Error: "сессия модели: " + err.Error()})
		if pendingReply {
			d.sendError(turnID, models.ErrorCodeUnavailable, err)
		}
	}
	if pendingReply {
		d.send(&models.Message{Type: models.TypeTurnComplete, TurnID: turnID})
	}
}

// resetIdleLocked (re)arms the idle timer. d.mu must be held.
func (d *deviceSession) resetIdleLocked() {
	if d.idle != nil {
		d.idle.Stop()
	}
	d.idle = time.AfterFunc(d.cfg.IdleTimeout, d.expire)
}

func (d *deviceSession) expire() {
	d.mu.Lock()
	if d.inTurn || d.awaiting || d.ps == nil {
		d.mu.Unlock()
		return
	}
	ps := d.ps
	d.ps = nil
	d.mu.Unlock()
	d.log.Info("closing idle provider session")
	ps.Close()
	d.srv.events.devicesChanged()
	d.prewarm() // fresh context, ready for the next question
}

func (d *deviceSession) runTools(ps provider.Session, turnID string, calls []provider.ToolCall, suppressed bool) {
	results := make([]provider.ToolResult, 0, len(calls))
	for _, c := range calls {
		if r, ok := d.execute(turnID, c, suppressed); ok {
			results = append(results, r)
		}
	}
	if len(results) == 0 {
		return
	}
	if err := ps.SendToolResults(results); err != nil {
		d.log.Warn("send tool results", "err", err)
	}
}

// execute runs a single call, either as a server-side skill or on the
// device. It reports false if the call was cancelled by the provider and must
// not be answered.
func (d *deviceSession) execute(turnID string, c provider.ToolCall, suppressed bool) (provider.ToolResult, bool) {
	res := provider.ToolResult{ID: c.ID, Name: c.Name}
	if sk, ok := d.cfg.Skills.Lookup(c.Name); ok && !suppressed {
		ctx, cancel := context.WithTimeout(d.ctx, d.cfg.CommandTimeout)
		defer cancel()
		start := time.Now()
		out, err := sk.Run(ctx, d.device(), c.Args)
		ev := Event{Kind: EventSkill, Session: d.id, Device: d.hello.DeviceID, Name: c.Name,
			Args: c.Args, LatencyMS: time.Since(start).Milliseconds()}
		if err != nil {
			res.Err = err.Error()
			ev.Error = res.Err
		} else {
			res.Output = out
			ev.Result = out
		}
		d.log.Info("skill", "name", c.Name, "args", c.Args, "latency", time.Since(start), "err", err)
		d.srv.events.publish(ev)
		return res, true
	}
	tool, ok := tools.Lookup(c.Name)
	switch {
	case !ok:
		res.Err = fmt.Sprintf("unknown function %q", c.Name)
		return res, true
	case suppressed:
		res.Err = "the user cancelled the request"
		return res, true
	}

	r, err := d.command(d.ctx, turnID, c.ID, tool.Action, c.Args, SourceVoice)
	switch {
	case errors.Is(err, errCommandCancelled):
		return res, false
	case err != nil:
		res.Err = err.Error()
	case !r.OK:
		res.Err = r.Error
	default:
		res.Output = r.Data
		if tool.Action == models.ActionConversationEnd {
			d.mu.Lock()
			d.ending = true
			d.mu.Unlock()
		}
	}
	return res, true
}

// errCommandCancelled means the command was abandoned: the model cancelled
// the call or the device disconnected.
var errCommandCancelled = errors.New("command cancelled")

// command asks the device to execute action and waits for the result. An
// empty id is generated.
func (d *deviceSession) command(ctx context.Context, turnID, id string, action models.Action, args map[string]any, source string) (*models.CommandResult, error) {
	if id == "" {
		id = newID()
	}
	ch := make(chan *models.CommandResult, 1)
	d.mu.Lock()
	d.pending[id] = ch
	d.mu.Unlock()
	defer func() {
		d.mu.Lock()
		delete(d.pending, id)
		d.mu.Unlock()
	}()

	start := time.Now()
	// The panel polls the state of the speaker; only actions are history.
	quiet := source == SourcePanel && (action == models.ActionVolumeGet ||
		action == models.ActionMediaStatus || action == models.ActionBluetoothDevices)
	publish := func(ev Event) {
		if !quiet {
			d.srv.events.publish(ev)
		}
	}
	ev := Event{Kind: EventCommand, Session: d.id, Device: d.hello.DeviceID, Source: source, Name: string(action), Args: args}
	cmd := &models.Command{ID: id, Action: action, Args: args}
	d.log.Info("command", "action", cmd.Action, "args", cmd.Args, "source", source)
	if err := d.send(&models.Message{Type: models.TypeCommand, TurnID: turnID, Command: cmd}); err != nil {
		err = fmt.Errorf("device unreachable: %w", err)
		ev.Error = err.Error()
		publish(ev)
		return nil, err
	}

	t := time.NewTimer(d.cfg.CommandTimeout)
	defer t.Stop()
	select {
	case r, ok := <-ch:
		if !ok {
			return nil, errCommandCancelled
		}
		d.log.Info("command result", "action", cmd.Action, "ok", r.OK, "err", r.Error)
		ev.Result, ev.Error, ev.LatencyMS = r.Data, r.Error, time.Since(start).Milliseconds()
		publish(ev)
		return r, nil
	case <-t.C:
		err := errors.New("the device did not respond in time")
		ev.Error = err.Error()
		publish(ev)
		return nil, err
	case <-ctx.Done():
		return nil, errCommandCancelled
	case <-d.ctx.Done():
		return nil, errCommandCancelled
	}
}

func (d *deviceSession) resolve(r *models.CommandResult) {
	d.mu.Lock()
	ch, ok := d.pending[r.ID]
	d.mu.Unlock()
	if !ok {
		d.log.Warn("result for unknown command", "id", r.ID)
		return
	}
	select {
	case ch <- r:
	default:
	}
}

func (d *deviceSession) cancel(ids []string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	for _, id := range ids {
		if ch, ok := d.pending[id]; ok {
			close(ch)
			delete(d.pending, id)
		}
	}
}

var (
	errBusy        = errors.New("колонка сейчас занята разговором, попробуйте через пару секунд")
	errNoRemote    = errors.New("агент на колонке устарел: перезагрузите колонку, чтобы она скачала новый")
	errNoText      = errors.New("этот провайдер не принимает текстовые запросы")
	errUnsupported = errors.New("колонка не поддерживает это действие")
)

// say asks the model text on behalf of the user, as if it had been spoken
// to the speaker; the reply is played by the speaker.
func (d *deviceSession) say(text string) error {
	if !slices.Contains(d.hello.Features, models.FeatureRemoteTurn) {
		return errNoRemote
	}
	d.mu.Lock()
	busy := d.inTurn || d.awaiting || d.ask != nil
	d.mu.Unlock()
	if busy {
		return errBusy
	}
	ps, err := d.session()
	if err != nil {
		return err
	}
	ts, ok := ps.(provider.TextSender)
	if !ok {
		return errNoText
	}

	turnID := "panel-" + newID()
	d.mu.Lock()
	if d.inTurn || d.awaiting || d.ask != nil {
		d.mu.Unlock()
		return errBusy
	}
	d.turnID, d.inTurn, d.awaiting, d.suppress = turnID, false, true, false
	d.source, d.ending, d.output, d.retryText = SourcePanel, false, false, text
	d.heard.Reset()
	d.heard.WriteString(text)
	d.said.Reset()
	if d.idle != nil {
		d.idle.Stop()
	}
	d.mu.Unlock()
	d.srv.events.devicesChanged()

	d.log.Info("text request from the panel", "turn", turnID, "text", text)
	err = d.send(&models.Message{Type: models.TypeRemoteTurn, TurnID: turnID})
	if err == nil {
		err = ts.SendText(text)
	}
	if err != nil {
		d.mu.Lock()
		if d.turnID == turnID {
			d.awaiting = false
			d.resetIdleLocked()
		}
		d.mu.Unlock()
		d.send(&models.Message{Type: models.TypeTurnComplete, TurnID: turnID})
		d.srv.events.devicesChanged()
		return err
	}
	return nil
}

// refresh replaces the model session so that new settings take effect. A
// session in use is replaced at the next turn.
func (d *deviceSession) refresh() {
	d.mu.Lock()
	if d.ps == nil {
		d.mu.Unlock()
		return
	}
	d.retiring = true
	busy := d.inTurn || d.awaiting
	d.mu.Unlock()
	if !busy {
		d.prewarm()
	}
}

// forget drops the conversation context of an idle device.
func (d *deviceSession) forget() error {
	d.mu.Lock()
	if d.inTurn || d.awaiting {
		d.mu.Unlock()
		return errBusy
	}
	ps := d.ps
	d.ps, d.retiring = nil, false
	d.mu.Unlock()
	if ps != nil {
		d.log.Info("conversation reset from the panel")
		ps.Close()
	}
	d.srv.events.devicesChanged()
	d.prewarm()
	return nil
}

// supports reports whether the device can execute action.
func (d *deviceSession) supports(action models.Action) bool {
	if _, ok := tools.Lookup(tools.NameOf(action)); !ok {
		return false
	}
	return len(d.hello.Capabilities) == 0 || slices.Contains(d.hello.Capabilities, action)
}

// retryEmpty resends a text request from the panel once if the model
// completed the turn without any output. It reports whether it did, in
// which case the completion must be ignored.
func (d *deviceSession) retryEmpty(ps provider.Session) bool {
	d.mu.Lock()
	text := d.retryText
	retry := text != "" && !d.output && !d.suppress && d.awaiting && d.ps == ps
	d.retryText = ""
	d.mu.Unlock()
	if !retry {
		return false
	}
	ts, ok := ps.(provider.TextSender)
	if !ok {
		return false
	}
	d.log.Warn("empty reply to a text request; sending it again", "text", text)
	return ts.SendText(text) == nil
}

// extraLanguages returns the configured extra languages other than lang.
func extraLanguages(lang, extra string) []string {
	l, _ := parseLanguages(extra, lang) // validated when saved
	return l
}

// languagesNote tells the model which languages to expect, so that, e.g., a
// Hebrew phrase is not taken for a garbled Russian one.
func languagesNote(lang, extra string) string {
	l := extraLanguages(lang, extra)
	if len(l) == 0 {
		return ""
	}
	return "\n\nПользователи говорят на языках: " + strings.Join(append([]string{lang}, l...), ", ") +
		". Понимай любой из них и отвечай на том языке, на котором к тебе обратились."
}
