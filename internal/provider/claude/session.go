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

package claude

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/anthropics/anthropic-sdk-go"

	"github.com/yawaflua/marusya-gemini/internal/provider"
)

const (
	// maxIterations bounds the tool-call loop of one turn.
	maxIterations = 8
	// maxTurnAudio caps buffered user audio (about 60 s at 16 kHz).
	maxTurnAudio = 2 << 20
	// refusalReply is spoken when Claude declines to answer.
	refusalReply = "Извини, с этим я помочь не могу."
)

// session is one conversation. User audio is buffered between StartTurn and
// EndTurn; the turn then runs in its own goroutine and can be abandoned by
// the next StartTurn.
type session struct {
	p      *Provider
	opts   provider.SessionOptions
	params anthropic.MessageNewParams
	events chan provider.Event
	done   chan struct{}

	mu        sync.Mutex
	audio     bytes.Buffer
	recording bool
	cancel    context.CancelFunc // aborts the running turn
	results   chan []provider.ToolResult
	running   sync.WaitGroup // turn goroutines, waited for by Close

	// turnMu serializes turns: a new turn waits for the abandoned one to
	// exit, so history is never used by two turns at once.
	turnMu  sync.Mutex
	history []anthropic.MessageParam

	closeOnce sync.Once
}

var (
	_ provider.Session    = (*session)(nil)
	_ provider.TextSender = (*session)(nil)
)

func newSession(p *Provider, opts provider.SessionOptions) *session {
	return &session{
		p:       p,
		opts:    opts,
		params:  p.params(opts),
		events:  make(chan provider.Event, 256),
		done:    make(chan struct{}),
		results: make(chan []provider.ToolResult, 1),
	}
}

func (s *session) closed() bool {
	select {
	case <-s.done:
		return true
	default:
		return false
	}
}

// StartTurn implements [provider.Session]. It abandons a reply in progress.
func (s *session) StartTurn() error {
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
func (s *session) SendAudio(pcm []byte) error {
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
func (s *session) EndTurn() error {
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
func (s *session) SendText(text string) error {
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
func (s *session) startLocked(pcm []byte, text string) {
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
func (s *session) SendToolResults(r []provider.ToolResult) error {
	select {
	case s.results <- r:
		return nil
	case <-s.done:
		return provider.ErrClosed
	}
}

// Events implements [provider.Session].
func (s *session) Events() <-chan provider.Event { return s.events }

// Err implements [provider.Session].
func (s *session) Err() error { return nil }

// Close implements [provider.Session].
func (s *session) Close() error {
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

func (s *session) emit(ctx context.Context, e provider.Event) bool {
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
func (s *session) turn(ctx context.Context, pcm []byte, text string) {
	if ctx.Err() != nil || s.closed() {
		return
	}
	defer s.emit(ctx, provider.Event{Kind: provider.EventTurnComplete})

	if text == "" {
		start := time.Now()
		var err error
		text, err = s.p.cfg.Recognizer.Transcribe(ctx, pcm, s.opts.InputFormat, s.opts.Language)
		if err != nil {
			if ctx.Err() == nil {
				s.p.log.Warn("claude: speech recognition failed", "err", err)
			}
			return
		}
		s.p.log.Debug("claude: transcribed", "latency", time.Since(start), "text", text)
		if text == "" {
			return // nothing intelligible was said; the device goes idle
		}
		if !s.emit(ctx, provider.Event{Kind: provider.EventInputTranscript, Text: text}) {
			return
		}
	}

	sp := newSpeaker(ctx, s)
	defer sp.close()

	// Messages of this turn join the history only if it completes, so an
	// abandoned turn never leaves an unanswered tool call behind.
	msgs := append(s.history[:len(s.history):len(s.history)],
		anthropic.NewUserMessage(anthropic.NewTextBlock(text)))
	for range maxIterations {
		msg, err := s.generate(ctx, msgs, sp)
		if err != nil {
			if ctx.Err() == nil {
				s.p.log.Warn("claude: request failed", "err", err)
				sp.say("Извини, не получилось ответить: сервис недоступен.")
			}
			return
		}
		msgs = append(msgs, msg.ToParam())

		switch msg.StopReason {
		case anthropic.StopReasonToolUse:
			results, ok := s.runTools(ctx, msg)
			if !ok {
				return
			}
			msgs = append(msgs, anthropic.NewUserMessage(results...))
			continue
		case anthropic.StopReasonPauseTurn:
			continue // a long server tool (web search) paused; resume it
		case anthropic.StopReasonRefusal:
			s.p.log.Info("claude: refused", "category", msg.StopDetails.Category)
			if !sp.spoke {
				sp.say(refusalReply)
			}
		}
		break
	}
	sp.flush()
	if ctx.Err() == nil {
		s.history = msgs
	}
}

// generate streams one response, speaking text as it arrives.
func (s *session) generate(ctx context.Context, msgs []anthropic.MessageParam, sp *speaker) (*anthropic.Message, error) {
	params := s.params
	params.Messages = msgs
	stream := s.p.client.Messages.NewStreaming(ctx, params)
	msg := &anthropic.Message{}
	for stream.Next() {
		ev := stream.Current()
		if err := msg.Accumulate(ev); err != nil {
			return nil, err
		}
		if d, ok := ev.AsAny().(anthropic.ContentBlockDeltaEvent); ok {
			if t, ok := d.Delta.AsAny().(anthropic.TextDelta); ok {
				sp.write(t.Text)
			}
		}
	}
	if err := stream.Err(); err != nil {
		return nil, err
	}
	// Text before a tool call or web search ("сейчас посмотрю") is spoken
	// right away rather than waiting for a sentence end.
	sp.flush()
	return msg, nil
}

// runTools forwards Claude's client tool calls and waits for the results.
func (s *session) runTools(ctx context.Context, msg *anthropic.Message) ([]anthropic.ContentBlockParamUnion, bool) {
	var calls []provider.ToolCall
	for _, b := range msg.Content {
		tu, ok := b.AsAny().(anthropic.ToolUseBlock)
		if !ok {
			continue
		}
		args := map[string]any{}
		if raw := tu.JSON.Input.Raw(); raw != "" {
			if err := json.Unmarshal([]byte(raw), &args); err != nil {
				args = map[string]any{}
			}
		}
		calls = append(calls, provider.ToolCall{ID: tu.ID, Name: tu.Name, Args: args})
	}
	if len(calls) == 0 {
		return nil, false
	}
	// Drop stale results of an abandoned turn.
	select {
	case <-s.results:
	default:
	}
	if !s.emit(ctx, provider.Event{Kind: provider.EventToolCall, ToolCalls: calls}) {
		return nil, false
	}
	var got []provider.ToolResult
	select {
	case got = <-s.results:
	case <-ctx.Done():
		return nil, false
	case <-s.done:
		return nil, false
	}
	byID := map[string]provider.ToolResult{}
	for _, r := range got {
		byID[r.ID] = r
	}
	// Every tool_use needs a tool_result, in one user message.
	out := make([]anthropic.ContentBlockParamUnion, 0, len(calls))
	for _, c := range calls {
		r, ok := byID[c.ID]
		switch {
		case !ok:
			out = append(out, anthropic.NewToolResultBlock(c.ID, "no result", true))
		case r.Err != "":
			out = append(out, anthropic.NewToolResultBlock(c.ID, r.Err, true))
		default:
			b, _ := json.Marshal(r.Output)
			if r.Output == nil {
				b = []byte(`{"output":"ok"}`)
			}
			out = append(out, anthropic.NewToolResultBlock(c.ID, string(b), false))
		}
	}
	return out, true
}

// speaker turns streamed text into speech sentence by sentence. Synthesis
// runs in a goroutine so the next sentence is generated while the previous
// one is being spoken.
type speaker struct {
	ctx   context.Context
	s     *session
	buf   strings.Builder
	queue chan string
	done  chan struct{}
	spoke bool
}

func newSpeaker(ctx context.Context, s *session) *speaker {
	sp := &speaker{ctx: ctx, s: s, queue: make(chan string, 32), done: make(chan struct{})}
	go sp.run()
	return sp
}

func (sp *speaker) run() {
	defer close(sp.done)
	for text := range sp.queue {
		if sp.ctx.Err() != nil {
			continue
		}
		pcm, err := sp.s.p.cfg.Synthesizer.Synthesize(sp.ctx, text, sp.s.opts.Language)
		if err != nil {
			if !errors.Is(err, context.Canceled) {
				sp.s.p.log.Warn("claude: speech synthesis failed", "err", err, "text", text)
			}
			continue
		}
		sp.s.emit(sp.ctx, provider.Event{Kind: provider.EventOutputTranscript, Text: text + " "})
		// Chunks of ~100 ms keep the device buffer smooth.
		chunk := sp.s.p.cfg.Synthesizer.Format().FrameSize(100 * time.Millisecond)
		for len(pcm) > 0 {
			n := min(chunk, len(pcm))
			if !sp.s.emit(sp.ctx, provider.Event{Kind: provider.EventAudio, Audio: pcm[:n]}) {
				break
			}
			pcm = pcm[n:]
		}
	}
}

// write adds streamed text and queues every complete sentence.
func (sp *speaker) write(t string) {
	sp.buf.WriteString(t)
	for {
		s := sp.buf.String()
		i := sentenceEnd(s)
		if i < 0 {
			return
		}
		sp.say(s[:i])
		sp.buf.Reset()
		sp.buf.WriteString(s[i:])
	}
}

// flush queues whatever text is buffered.
func (sp *speaker) flush() {
	s := sp.buf.String()
	sp.buf.Reset()
	sp.say(s)
}

func (sp *speaker) say(text string) {
	text = strings.TrimSpace(text)
	if text == "" {
		return
	}
	sp.spoke = true
	select {
	case sp.queue <- text:
	case <-sp.ctx.Done():
	}
}

// close waits until everything queued has been spoken.
func (sp *speaker) close() {
	close(sp.queue)
	<-sp.done
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
