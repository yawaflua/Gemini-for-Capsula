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

package agent

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/yawaflua/marusya-gemini/internal/audio"
	"github.com/yawaflua/marusya-gemini/internal/indicator"
	"github.com/yawaflua/marusya-gemini/internal/models"
	"github.com/yawaflua/marusya-gemini/internal/transport"
	"github.com/yawaflua/marusya-gemini/internal/vad"
	"github.com/yawaflua/marusya-gemini/internal/wakeword"
)

// state is the phase of the conversation.
type state int

const (
	// stateIdle waits for the wake word.
	stateIdle state = iota
	// stateListening records the user's request.
	stateListening
	// stateThinking waits for the server's reply.
	stateThinking
	// stateSpeaking plays the server's reply.
	stateSpeaking
)

func (s state) String() string {
	return [...]string{"idle", "listening", "thinking", "speaking"}[s]
}

func (s state) indicator() indicator.State {
	switch s {
	case stateListening:
		return indicator.Listening
	case stateThinking:
		return indicator.Thinking
	case stateSpeaking:
		return indicator.Speaking
	}
	return indicator.Idle
}

const sendTimeout = 5 * time.Second

// session is the per-connection state machine. All fields are owned by the
// run goroutine.
type session struct {
	a         *Agent
	conn      *transport.Conn
	outFormat models.AudioFormat
	inFormat  models.AudioFormat
	ep        *vad.Endpointer
	serverVAD bool // the server detects the end of speech (listen_end)

	state     state
	since     time.Time
	turnSeq   int
	turnID    string
	speech    bool      // turn_start was sent for the current turn
	remote    bool      // the current turn was started by the server (remote_turn)
	ending    bool      // the model ended the conversation: no follow-up
	announce  bool      // the current turn plays an announcement (TypeAnnounce)
	askListen bool      // ...after which an answer is expected
	replyTo   string    // the turn being recorded answers this announcement
	wakeFeed  bool      // stream idle microphone audio for the server's wake word spotter
	deafUntil time.Time // ignore the mic while our own earcon plays
	preroll   [][]byte
	failed    bool // the current turn produced an error
	replied   bool // the current turn produced audio
	ducked    bool // media was paused by us and must be resumed
	heard     strings.Builder
	said      strings.Builder

	stream   audio.Stream // active reply playback
	draining audio.Stream // reply playback finishing in the background
	drained  chan audio.Stream
}

func newSession(a *Agent, conn *transport.Conn, ready *models.Ready) *session {
	return &session{
		a:         a,
		conn:      conn,
		outFormat: ready.OutputFormat,
		serverVAD: ready.ServerVAD,
		inFormat:  a.capture.Format(),
		ep:        vad.New(a.cfg.VAD),
		drained:   make(chan audio.Stream, 1),
	}
}

func (s *session) run(ctx context.Context) error {
	inbound := make(chan transport.Frame, 256)
	readErr := make(chan error, 1)
	go func() {
		for {
			f, err := s.conn.Read(ctx)
			if err != nil {
				readErr <- err
				return
			}
			select {
			case inbound <- f:
			case <-ctx.Done():
				return
			}
		}
	}()
	defer s.reset(ctx)

	tick := time.NewTicker(500 * time.Millisecond)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case err := <-readErr:
			return err
		case f := <-s.a.frames:
			s.onFrame(ctx, f)
		case d := <-s.a.wakes:
			// Any activation silences a ringing alarm, also with the
			// microphone muted, and does nothing else.
			if s.a.alarm.stop() {
				s.a.log.Info("alarm silenced", "source", d.Source)
				continue
			}
			if s.a.muted.Load() {
				s.a.log.Info("wake ignored: microphone muted", "source", d.Source)
				continue
			}
			s.onWake(ctx, d)
		case m := <-s.a.mutes:
			if m && s.state == stateListening {
				s.cancelListening(ctx, "microphone muted")
			}
		case f := <-inbound:
			if f.Message == nil {
				s.onAudio(ctx, f.Audio)
			} else {
				s.onMessage(ctx, f.Message)
			}
		case st := <-s.drained:
			s.onDrained(ctx, st)
		case l := <-s.a.logs:
			// Failures are not logged: that would feed the queue again.
			sctx, cancel := context.WithTimeout(ctx, sendTimeout)
			s.conn.Send(sctx, &models.Message{Type: models.TypeLog, Log: &l})
			cancel()
		case <-tick.C:
			s.checkTimeouts(ctx)
		}
	}
}

func (s *session) setState(st state) {
	if s.state != st {
		s.a.log.Debug("state", "from", s.state, "to", st, "turn", s.turnID)
	}
	s.state, s.since = st, time.Now()
	s.a.busy.Store(st != stateIdle)
	ind := st.indicator()
	if s.announce && (st == stateThinking || st == stateSpeaking) {
		ind = indicator.Announcing
	}
	s.a.indicator.Show(ind)
}

func (s *session) send(ctx context.Context, m *models.Message) {
	ctx, cancel := context.WithTimeout(ctx, sendTimeout)
	defer cancel()
	if err := s.conn.Send(ctx, m); err != nil {
		s.a.log.Warn("send", "type", m.Type, "err", err)
	}
}

func (s *session) sendAudio(ctx context.Context, pcm []byte) {
	ctx, cancel := context.WithTimeout(ctx, sendTimeout)
	defer cancel()
	if err := s.conn.SendAudio(ctx, pcm); err != nil {
		s.a.log.Warn("send audio", "err", err)
	}
}

// onWake handles a wake word detection.
func (s *session) onWake(ctx context.Context, d wakeword.Detection) {
	s.a.log.Info("wake", "source", d.Source, "keyword", d.Keyword, "state", s.state)
	switch s.state {
	case stateListening:
		// A second press stops the request being recorded. A keyword
		// spotter firing again is most likely hearing the same phrase.
		if d.Source != "exec" && d.Source != "server" {
			s.cancelListening(ctx, "activation pressed again")
			s.a.playEarcon(ctx, audio.EarconDone)
		}
		return
	case stateThinking, stateSpeaking:
		if s.announce && s.askListen {
			// Pressed while a question is read out: answer right away.
			s.stopPlayback()
			s.listenForAnswer(ctx, s.turnID)
			return
		}
		// Barge-in: drop the current reply. Starting a new turn makes the
		// model abandon the previous one.
		s.stopPlayback()
		if s.speech {
			s.send(ctx, &models.Message{Type: models.TypeTurnCancel, TurnID: s.turnID})
		}
	}
	s.startListening(ctx, 0)
}

// startListening records a request; timeout bounds the wait for speech
// (zero: the endpointer's default).
func (s *session) startListening(ctx context.Context, timeout time.Duration) {
	s.turnSeq++
	s.turnID = fmt.Sprintf("%s-%d-%d", s.a.cfg.DeviceID, time.Now().Unix(), s.turnSeq)
	s.speech, s.failed, s.replied, s.remote, s.ending = false, false, false, false, false
	s.announce, s.askListen, s.replyTo = false, false, ""
	s.preroll = s.preroll[:0]
	s.heard.Reset()
	s.said.Reset()

	s.ep.Reset(timeout)
	s.duck(ctx)

	earcon := s.a.playEarcon(ctx, audio.EarconListen)
	if earcon > 0 {
		s.deafUntil = time.Now().Add(earcon + 80*time.Millisecond)
	}
	s.setState(stateListening)
}

// onFrame handles 20 ms of microphone audio.
func (s *session) onFrame(ctx context.Context, f []byte) {
	if s.state == stateIdle {
		s.ep.Observe(f)
		if s.wakeFeed && !s.a.muted.Load() {
			s.sendAudio(ctx, f)
		}
		return
	}
	if s.state != stateListening || time.Now().Before(s.deafUntil) {
		return
	}

	dec := s.ep.Process(f, s.inFormat.Duration(len(f)))
	switch dec {
	case vad.Waiting:
		s.pushPreroll(f)
	case vad.SpeechStarted:
		s.speech = true
		s.send(ctx, &models.Message{Type: models.TypeTurnStart, TurnID: s.turnID, ReplyTo: s.replyTo})
		for _, p := range s.preroll {
			s.sendAudio(ctx, p)
		}
		s.preroll = s.preroll[:0]
		s.sendAudio(ctx, f)
	case vad.Speaking:
		s.sendAudio(ctx, f)
	case vad.EndOfUtterance:
		s.sendAudio(ctx, f)
		s.send(ctx, &models.Message{Type: models.TypeTurnEnd, TurnID: s.turnID})
		s.a.log.Info("utterance captured", "turn", s.turnID, "by", "device", "length", time.Since(s.since).Round(100*time.Millisecond))
		s.setState(stateThinking)
	case vad.NoSpeech:
		s.a.log.Info("no speech detected")
		s.declineAnswer(ctx)
		s.a.playEarcon(ctx, audio.EarconDone)
		s.goIdle(ctx)
	}
}

// listenForAnswer records the answer to the announcement announceID.
func (s *session) listenForAnswer(ctx context.Context, announceID string) {
	if s.a.muted.Load() {
		s.a.log.Info("not listening for an answer: microphone muted", "announce", announceID)
		s.send(ctx, &models.Message{Type: models.TypeTurnCancel, ReplyTo: announceID})
		s.goIdle(ctx)
		return
	}
	s.startListening(ctx, s.a.cfg.AnswerTimeout)
	s.replyTo = announceID
}

// declineAnswer tells the server that the announcement being answered gets
// no answer.
func (s *session) declineAnswer(ctx context.Context) {
	if s.replyTo != "" && !s.speech {
		s.send(ctx, &models.Message{Type: models.TypeTurnCancel, ReplyTo: s.replyTo})
	}
}

// cancelListening abandons the request being recorded.
func (s *session) cancelListening(ctx context.Context, reason string) {
	s.a.log.Info("listening cancelled", "reason", reason, "turn", s.turnID)
	s.declineAnswer(ctx)
	if s.speech {
		s.send(ctx, &models.Message{Type: models.TypeTurnCancel, TurnID: s.turnID})
	}
	s.goIdle(ctx)
}

func (s *session) pushPreroll(f []byte) {
	limit := s.inFormat.FrameSize(s.a.cfg.PreRoll)
	s.preroll = append(s.preroll, f)
	total := 0
	for i := len(s.preroll) - 1; i >= 0; i-- {
		total += len(s.preroll[i])
		if total > limit {
			s.preroll = append(s.preroll[:0], s.preroll[i+1:]...)
			return
		}
	}
}

// onAudio handles a chunk of the spoken reply.
func (s *session) onAudio(ctx context.Context, pcm []byte) {
	if s.state != stateThinking && s.state != stateSpeaking {
		return // stale audio of an abandoned turn
	}
	if s.stream == nil {
		st, err := s.a.player.Open(ctx, s.outFormat)
		if err != nil {
			s.a.log.Error("open playback", "err", err)
			return
		}
		s.stream = st
		s.setState(stateSpeaking)
	}
	s.replied = true
	if err := s.stream.Write(pcm); err != nil {
		s.a.log.Warn("playback write", "err", err)
	}
}

func (s *session) onMessage(ctx context.Context, m *models.Message) {
	switch m.Type {
	case models.TypeTranscript:
		s.a.log.Debug("transcript", "role", m.Transcript.Role, "text", m.Transcript.Text)
		if m.TurnID == s.turnID {
			if m.Transcript.Role == models.RoleUser {
				s.heard.WriteString(m.Transcript.Text)
			} else {
				s.said.WriteString(m.Transcript.Text)
			}
		}

	case models.TypeListenEnd:
		// The server detected the end of speech before our endpointer.
		if m.TurnID == s.turnID && s.state == stateListening && s.speech {
			s.a.log.Info("utterance captured", "turn", s.turnID, "by", "server", "length", time.Since(s.since).Round(100*time.Millisecond))
			s.setState(stateThinking)
		}

	case models.TypeRemoteTurn:
		s.startRemote(ctx, m.TurnID)

	case models.TypeAnnounce:
		s.startAnnounce(ctx, m.TurnID, m.Announce)

	case models.TypeAlarms:
		s.a.alarm.set(m.Alarms.Rings)

	case models.TypeWakeStream:
		if s.wakeFeed != m.WakeStream.On {
			s.a.log.Info("server wake word", "streaming", m.WakeStream.On)
		}
		s.wakeFeed = m.WakeStream.On

	case models.TypeWake:
		if s.a.muted.Load() {
			return
		}
		s.onWake(ctx, wakeword.Detection{Source: "server", Keyword: m.Wake.Keyword, At: time.Now()})

	case models.TypeCommand:
		cmd := m.Command
		if cmd.Action == models.ActionConversationEnd {
			// Let the goodbye play, then go idle instead of listening on.
			s.a.log.Info("conversation ended by the user", "turn", s.turnID)
			s.ending = true
			s.send(ctx, &models.Message{Type: models.TypeCommandResult, TurnID: m.TurnID, CommandResult: cmd.Success(nil)})
			return
		}
		if strings.HasPrefix(string(cmd.Action), "media.") {
			// The user is managing playback explicitly; do not resume
			// media behind their back at the end of the turn.
			s.ducked = false
		}
		go func() {
			res := s.a.dispatcher.Execute(ctx, cmd)
			level := slog.LevelInfo
			if cmd.Action == models.ActionSensorsGet && res.OK {
				level = slog.LevelDebug // the web panel polls it
			}
			s.a.log.Log(ctx, level, "command executed", "action", cmd.Action, "ok", res.OK, "err", res.Error)
			s.send(ctx, &models.Message{Type: models.TypeCommandResult, TurnID: m.TurnID, CommandResult: res})
		}()

	case models.TypeInterrupted:
		s.stopPlayback()

	case models.TypeError:
		s.a.log.Warn("server error", "code", m.Error.Code, "message", m.Error.Message)
		if m.TurnID == s.turnID {
			s.failed = true
		}

	case models.TypeTurnComplete:
		if m.TurnID != s.turnID || (s.state != stateThinking && s.state != stateSpeaking) {
			return
		}
		if heard, said := strings.TrimSpace(s.heard.String()), strings.TrimSpace(s.said.String()); heard != "" || said != "" {
			s.a.log.Info("turn", "user", heard, "model", said)
		}
		s.finishTurn(ctx)

	default:
		s.a.log.Debug("ignoring message", "type", m.Type)
	}
}

// startRemote prepares to play the reply to a request typed in the server's
// web panel. A conversation in progress has priority: the reply is then
// dropped like stale audio.
func (s *session) startRemote(ctx context.Context, turnID string) {
	if s.state != stateIdle {
		s.a.log.Info("remote turn ignored: busy", "turn", turnID, "state", s.state)
		return
	}
	s.a.log.Info("remote turn", "turn", turnID)
	s.turnID = turnID
	s.speech, s.failed, s.replied, s.remote, s.ending = false, false, false, true, false
	s.heard.Reset()
	s.said.Reset()
	s.duck(ctx)
	s.setState(stateThinking)
}

// startAnnounce prepares to play a message the server sends on its own. A
// conversation in progress has priority: the device declines and the
// server retries later.
func (s *session) startAnnounce(ctx context.Context, turnID string, a *models.Announce) {
	if s.state != stateIdle {
		s.a.log.Info("announcement declined: busy", "turn", turnID, "state", s.state)
		s.send(ctx, &models.Message{Type: models.TypeTurnCancel, TurnID: turnID})
		return
	}
	s.a.log.Info("announcement", "turn", turnID, "text", a.Text, "listen", a.Listen)
	s.turnID = turnID
	s.speech, s.failed, s.replied, s.remote, s.ending = false, false, false, true, false
	s.announce, s.askListen, s.replyTo = true, a.Listen, ""
	s.heard.Reset()
	s.said.Reset()
	s.duck(ctx)
	s.setState(stateThinking)
	// The chime goes into the reply stream, so the message follows it
	// instead of playing over it.
	if s.a.cfg.Earcons {
		st, err := s.a.player.Open(ctx, s.outFormat)
		if err != nil {
			s.a.log.Error("open playback", "err", err)
			return
		}
		s.stream = st
		st.Write(audio.EarconNotify.Render(s.outFormat))
		s.setState(stateSpeaking)
	}
}

// finishTurn waits for the reply to finish playing, then continues.
func (s *session) finishTurn(ctx context.Context) {
	if s.stream == nil {
		s.afterReply(ctx)
		return
	}
	st := s.stream
	s.stream, s.draining = nil, st
	go func() {
		st.Drain()
		select {
		case s.drained <- st:
		case <-ctx.Done():
		}
	}()
}

func (s *session) onDrained(ctx context.Context, st audio.Stream) {
	if st != s.draining {
		return // aborted by barge-in
	}
	s.draining = nil
	s.afterReply(ctx)
}

func (s *session) afterReply(ctx context.Context) {
	switch {
	case s.announce && s.askListen && !s.failed:
		s.listenForAnswer(ctx, s.turnID)
		return
	case s.replyTo != "":
		// The answer was passed on.
		s.replyTo = ""
		s.goIdle(ctx)
		s.a.playEarcon(ctx, audio.EarconDone)
		return
	}
	if s.a.cfg.FollowUp && s.replied && !s.failed && !s.remote && !s.ending {
		s.startListening(ctx, s.a.cfg.FollowUpTimeout)
		return
	}
	s.goIdle(ctx)
	if s.failed {
		s.a.playEarcon(ctx, audio.EarconError)
		s.a.indicator.Show(indicator.Error)
	}
}

func (s *session) checkTimeouts(ctx context.Context) {
	if s.state == stateThinking && time.Since(s.since) > s.a.cfg.ResponseTimeout {
		s.a.log.Warn("server did not reply in time", "turn", s.turnID)
		s.send(ctx, &models.Message{Type: models.TypeTurnCancel, TurnID: s.turnID})
		s.goIdle(ctx)
		s.a.playEarcon(ctx, audio.EarconError)
		s.a.indicator.Show(indicator.Error)
	}
}

func (s *session) stopPlayback() {
	for _, st := range []audio.Stream{s.stream, s.draining} {
		if st != nil {
			st.Abort()
		}
	}
	s.stream, s.draining = nil, nil
}

func (s *session) goIdle(ctx context.Context) {
	s.announce, s.askListen = false, false
	s.stopPlayback()
	s.unduck(ctx)
	s.setState(stateIdle)
}

// reset cleans up when the connection ends.
func (s *session) reset(ctx context.Context) {
	s.stopPlayback()
	// ctx may already be cancelled; use a short independent one.
	uctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Second)
	defer cancel()
	s.unduck(uctx)
}

func (s *session) duck(ctx context.Context) {
	m := s.a.dispatcher.Media
	if !s.a.cfg.DuckMedia || m == nil || s.ducked {
		return
	}
	st, err := m.Status(ctx)
	if err != nil || !st.Playing {
		return
	}
	if err := m.Control(ctx, models.MediaPause); err == nil {
		s.ducked = true
	}
}

func (s *session) unduck(ctx context.Context) {
	if !s.ducked {
		return
	}
	s.ducked = false
	if err := s.a.dispatcher.Media.Control(ctx, models.MediaPlay); err != nil {
		s.a.log.Warn("resume media", "err", err)
	}
}
