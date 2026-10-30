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

// Package pipeline turns a text chat model into a [provider.Session]: the
// user's audio is transcribed by a [speech.Recognizer], the model answers
// (calling device tools as needed) and the reply is spoken sentence by
// sentence by a [speech.Synthesizer] while it is still being generated.
//
// Model backends (provider/claude, provider/openaichat) only implement
// [Conversation].
package pipeline

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/yawaflua/marusya-gemini/internal/provider"
	"github.com/yawaflua/marusya-gemini/internal/speech"
)

// maxTurnAudio caps buffered user audio (about 60 s at 16 kHz).
const maxTurnAudio = 2 << 20

// FailedReply is spoken when the model cannot be reached.
const FailedReply = "Извини, не получилось ответить: сервис недоступен."

// Conversation is the model side of one session. It keeps the history.
type Conversation interface {
	// Turn answers text, streaming the reply into out. Turns never run
	// concurrently. A turn whose ctx is cancelled is abandoned and must not
	// change the history.
	Turn(ctx context.Context, text string, out *Output) error
}

// Config configures a [Session].
type Config struct {
	Recognizer  speech.Recognizer
	Synthesizer speech.Synthesizer
	Options     provider.SessionOptions
	// Name prefixes log messages, e.g. "claude".
	Name   string
	Logger *slog.Logger
}

// Session is a [provider.Session] driven by a [Conversation]. User audio is
// buffered between StartTurn and EndTurn; the turn then runs in its own
// goroutine and can be abandoned by the next StartTurn.
type Session struct {
	cfg    Config
	conv   Conversation
	events chan provider.Event
	done   chan struct{}

	mu        sync.Mutex
	audio     bytes.Buffer
	recording bool
	cancel    context.CancelFunc // aborts the running turn
	results   chan []provider.ToolResult
	running   sync.WaitGroup // turn goroutines, waited for by Close

	// turnMu serializes turns: a new turn waits for the abandoned one to
	// exit, so the conversation is never used by two turns at once.
	turnMu sync.Mutex

	closeOnce sync.Once
}

var (
	_ provider.Session    = (*Session)(nil)
	_ provider.TextSender = (*Session)(nil)
)

// New returns a session.
func New(cfg Config, conv Conversation) *Session {
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	return &Session{
		cfg:     cfg,
		conv:    conv,
		events:  make(chan provider.Event, 256),
		done:    make(chan struct{}),
		results: make(chan []provider.ToolResult, 1),
	}
}

func (s *Session) closed() bool {
	select {
	case <-s.done:
		return true
	default:
		return false
	}
}

// StartTurn implements [provider.Session]. It abandons a reply in progress.
func (s *Session) StartTurn() error {
	if s.closed() {
		return provider.ErrClosed
	}
	s.mu.Lock()
	if s.cancel != nil {
		s.cancel()
		s.cancel = nil
	}
	s.audio.Reset()
	s.recording = true
	s.mu.Unlock()
	return nil
}

// SendAudio implements [provider.Session].
func (s *Session) SendAudio(pcm []byte) error {
	if s.closed() {
		return provider.ErrClosed
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.recording && s.audio.Len()+len(pcm) <= maxTurnAudio {
		s.audio.Write(pcm)
	}
	return nil
}

// EndTurn implements [provider.Session]: it starts answering.
func (s *Session) EndTurn() error {
	if s.closed() {
		return provider.ErrClosed
	}
	s.mu.Lock()
	if !s.recording || s.closed() {
		s.mu.Unlock()
		return nil
	}
	s.recording = false
	pcm := bytes.Clone(s.audio.Bytes())
	s.audio.Reset()
	s.startLocked(pcm, "")
	return nil
}

// SendText implements [provider.TextSender]: it answers text as if it had
// been said, skipping speech recognition. A reply in progress is abandoned.
func (s *Session) SendText(text string) error {
	if s.closed() {
		return provider.ErrClosed
	}
	s.mu.Lock()
	if s.closed() {
		s.mu.Unlock()
		return provider.ErrClosed
	}
	if s.cancel != nil {
		s.cancel()
	}
	s.recording = false
	s.audio.Reset()
	s.startLocked(nil, text)
	return nil
}

// startLocked runs a turn in its own goroutine. s.mu must be held; it is
// released.
func (s *Session) startLocked(pcm []byte, text string) {
	ctx, cancel := context.WithCancel(context.Background())
	s.cancel = cancel
	s.running.Add(1)
	s.mu.Unlock()

	go func() {
		defer s.running.Done()
		defer cancel()
		s.turnMu.Lock()
		defer s.turnMu.Unlock()
		s.turn(ctx, pcm, text)
	}()
}

// SendToolResults implements [provider.Session].
func (s *Session) SendToolResults(r []provider.ToolResult) error {
	select {
	case s.results <- r:
		return nil
	case <-s.done:
		return provider.ErrClosed
	}
}

// Events implements [provider.Session].
func (s *Session) Events() <-chan provider.Event { return s.events }

// Err implements [provider.Session].
func (s *Session) Err() error { return nil }

// Close implements [provider.Session].
func (s *Session) Close() error {
	s.closeOnce.Do(func() {
		s.mu.Lock()
		if s.cancel != nil {
			s.cancel()
		}
		close(s.done)
		s.mu.Unlock()
		go func() {
			s.running.Wait()
			close(s.events)
		}()
	})
	return nil
}

func (s *Session) emit(ctx context.Context, e provider.Event) bool {
	select {
	case s.events <- e:
		return true
	case <-ctx.Done():
		return false
	case <-s.done:
		return false
	}
}

// turn answers one utterance, or typed text if it is not empty. Errors are
// spoken as a short apology; the turn always ends with EventTurnComplete
// unless it was abandoned.
func (s *Session) turn(ctx context.Context, pcm []byte, text string) {
	if ctx.Err() != nil || s.closed() {
		return
	}
	defer s.emit(ctx, provider.Event{Kind: provider.EventTurnComplete})
	log := s.cfg.Logger

	if text == "" {
		start := time.Now()
		var err error
		text, err = s.cfg.Recognizer.Transcribe(ctx, pcm, s.cfg.Options.InputFormat, s.cfg.Options.Language)
		if err != nil {
			if ctx.Err() == nil {
				log.Warn(s.cfg.Name+": speech recognition failed", "err", err)
			}
			return
		}
		log.Debug(s.cfg.Name+": transcribed", "latency", time.Since(start), "text", text)
		if text == "" {
			return // nothing intelligible was said; the device goes idle
		}
		if !s.emit(ctx, provider.Event{Kind: provider.EventInputTranscript, Text: text}) {
			return
		}
	}

	out := newOutput(ctx, s)
	defer out.close()
	if err := s.conv.Turn(ctx, text, out); err != nil && ctx.Err() == nil {
		log.Warn(s.cfg.Name+": request failed", "err", err)
		out.Say(FailedReply)
	}
	out.Flush()
}

// Output receives the reply of a turn. Text is turned into speech sentence
// by sentence; synthesis runs in a goroutine so the next sentence is
// generated while the previous one is being spoken.
type Output struct {
	ctx   context.Context
	s     *Session
	buf   strings.Builder
	queue chan string
	done  chan struct{}
	spoke bool
}

func newOutput(ctx context.Context, s *Session) *Output {
	o := &Output{ctx: ctx, s: s, queue: make(chan string, 32), done: make(chan struct{})}
	go o.run()
	return o
}

func (o *Output) run() {
	defer close(o.done)
	syn := o.s.cfg.Synthesizer
	for text := range o.queue {
		if o.ctx.Err() != nil {
			continue
		}
		pcm, err := syn.Synthesize(o.ctx, text, o.s.cfg.Options.Language)
		if err != nil {
			if !errors.Is(err, context.Canceled) {
				o.s.cfg.Logger.Warn(o.s.cfg.Name+": speech synthesis failed", "err", err, "text", text)
			}
			continue
		}
		o.s.emit(o.ctx, provider.Event{Kind: provider.EventOutputTranscript, Text: text + " "})
		// Chunks of ~100 ms keep the device buffer smooth.
		chunk := syn.Format().FrameSize(100 * time.Millisecond)
		for len(pcm) > 0 {
			n := min(chunk, len(pcm))
			if !o.s.emit(o.ctx, provider.Event{Kind: provider.EventAudio, Audio: pcm[:n]}) {
				break
			}
			pcm = pcm[n:]
		}
	}
}

// Write adds streamed reply text and queues every complete sentence.
func (o *Output) Write(t string) {
	o.buf.WriteString(t)
	for {
		s := o.buf.String()
		i := sentenceEnd(s)
		if i < 0 {
			return
		}
		o.Say(s[:i])
		o.buf.Reset()
		o.buf.WriteString(s[i:])
	}
}

// Flush queues whatever text is buffered. Text before a tool call ("сейчас
// посмотрю") should be flushed right away rather than waiting for a
// sentence end.
func (o *Output) Flush() {
	s := o.buf.String()
	o.buf.Reset()
	o.Say(s)
}

// Say queues text to be spoken as is.
func (o *Output) Say(text string) {
	text = strings.TrimSpace(text)
	if text == "" {
		return
	}
	o.spoke = true
	select {
	case o.queue <- text:
	case <-o.ctx.Done():
	}
}

// Spoke reports whether anything was said in this turn.
func (o *Output) Spoke() bool { return o.spoke }

// CallTools has the server run the model's function calls and waits for
// the results, ordered like calls. ok is false if the turn was abandoned.
func (o *Output) CallTools(calls []provider.ToolCall) (results []provider.ToolResult, ok bool) {
	if len(calls) == 0 {
		return nil, true
	}
	s := o.s
	// Drop stale results of an abandoned turn.
	select {
	case <-s.results:
	default:
	}
	if !s.emit(o.ctx, provider.Event{Kind: provider.EventToolCall, ToolCalls: calls}) {
		return nil, false
	}
	var got []provider.ToolResult
	select {
	case got = <-s.results:
	case <-o.ctx.Done():
		return nil, false
	case <-s.done:
		return nil, false
	}
	byID := map[string]provider.ToolResult{}
	for _, r := range got {
		byID[r.ID] = r
	}
	results = make([]provider.ToolResult, len(calls))
	for i, c := range calls {
		r, ok := byID[c.ID]
		if !ok {
			r = provider.ToolResult{ID: c.ID, Name: c.Name, Err: "no result"}
		}
		results[i] = r
	}
	return results, true
}

// close waits until everything queued has been spoken.
func (o *Output) close() {
	close(o.queue)
	<-o.done
}

// sentenceEnd returns the index just after the first sentence boundary in s
// that leaves a reasonably long sentence, or -1.
func sentenceEnd(s string) int {
	const minLen = 24
	for i, r := range s {
		end := i + utf8.RuneLen(r)
		switch r {
		case '\n':
			if strings.TrimSpace(s[:i]) != "" {
				return end
			}
		case '.', '!', '?', '…', ';':
			if end < len(s) && (s[end] == ' ' || s[end] == '\n') && utf8.RuneCountInString(s[:end]) >= minLen {
				return end
			}
		}
	}
	return -1
}
