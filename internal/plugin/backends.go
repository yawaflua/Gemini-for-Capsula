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

package plugin

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/yawaflua/marusya-gemini/internal/models"
	"github.com/yawaflua/marusya-gemini/internal/provider"
	"github.com/yawaflua/marusya-gemini/internal/provider/pipeline"
	"github.com/yawaflua/marusya-gemini/internal/speech"
	"github.com/yawaflua/marusya-gemini/internal/tools"
	"github.com/yawaflua/marusya-gemini/pkg/pluginsdk"
)

// maxProviderRounds bounds the function call rounds of one turn.
const maxProviderRounds = 8

// ProviderInfo describes a model provider of a server plugin.
type ProviderInfo struct {
	ID          string `json:"id"` // "<plugin>/<name>"
	Plugin      string `json:"plugin"`
	Label       string `json:"label,omitempty"`
	Description string `json:"description,omitempty"`
}

// SpeechInfo describes a speech engine of a server plugin.
type SpeechInfo struct {
	ID     string `json:"id"` // "<plugin>/<name>"
	Plugin string `json:"plugin"`
	pluginsdk.SpeechSpec
}

// Instructions returns what the running plugins add to the system
// instruction, one paragraph per plugin.
func (m *Manager) Instructions() string {
	if m == nil {
		return ""
	}
	var parts []string
	for _, in := range m.instances() {
		if t := in.capabilities().instructions; t != "" {
			parts = append(parts, t)
		}
	}
	return strings.Join(parts, "\n\n")
}

// Providers lists the model providers of the running plugins.
func (m *Manager) Providers() []ProviderInfo {
	if m == nil {
		return nil
	}
	var out []ProviderInfo
	for _, in := range m.instances() {
		for _, p := range in.capabilities().providers {
			out = append(out, ProviderInfo{ID: in.m.ID + "/" + p.Name, Plugin: in.m.ID, Label: p.Label, Description: p.Description})
		}
	}
	return out
}

// SpeechEngines lists the speech engines of the running plugins.
func (m *Manager) SpeechEngines() []SpeechInfo {
	if m == nil {
		return nil
	}
	var out []SpeechInfo
	for _, in := range m.instances() {
		for _, sp := range in.capabilities().speech {
			out = append(out, SpeechInfo{ID: in.m.ID + "/" + sp.Name, Plugin: in.m.ID, SpeechSpec: sp})
		}
	}
	return out
}

// HTTPSpec returns how the plugin serves HTTP, or nil if it does not.
func (m *Manager) HTTPSpec(plugin string) *pluginsdk.HTTPSpec {
	in, ok := m.instance(plugin)
	if !ok {
		return nil
	}
	return in.capabilities().http
}

// HTTP passes a request to a plugin.
func (m *Manager) HTTP(ctx context.Context, plugin string, req pluginsdk.HTTPRequest) (pluginsdk.HTTPResponse, error) {
	var res pluginsdk.HTTPResponse
	in, ok := m.instance(plugin)
	if !ok || in.capabilities().http == nil {
		return res, fmt.Errorf("plugin %s does not serve HTTP", plugin)
	}
	err := in.request(ctx, pluginsdk.MethodHTTP, req, &res)
	return res, err
}

// splitID splits "<plugin>/<name>".
func splitID(id string) (plugin, name string, err error) {
	plugin, name, ok := strings.Cut(id, "/")
	if !ok || plugin == "" || name == "" {
		return "", "", fmt.Errorf("%q: нужно «плагин/имя»", id)
	}
	return plugin, name, nil
}

// ErrNotRunning means the plugin that implements a provider or a speech
// engine does not run (yet).
var ErrNotRunning = errors.New("плагин не запущен")

// Backends gives the provider registry the model providers and speech
// engines of server plugins. It is created before the server, which sets
// the manager with [Backends.Set]; plugins are looked up on every use, so
// a provider keeps working across restarts of its plugin.
type Backends struct {
	m      atomic.Pointer[Manager]
	Logger *slog.Logger
}

// Set sets the manager of the server plugins.
func (b *Backends) Set(m *Manager) { b.m.Store(m) }

// Manager returns the manager, or nil before [Backends.Set].
func (b *Backends) Manager() *Manager { return b.m.Load() }

func (b *Backends) logger() *slog.Logger {
	if b.Logger != nil {
		return b.Logger
	}
	return slog.Default()
}

// instance finds the running plugin.
func (b *Backends) instance(plugin string) (*instance, error) {
	m := b.Manager()
	if m == nil {
		return nil, fmt.Errorf("%s: %w", plugin, ErrNotRunning)
	}
	in, ok := m.instance(plugin)
	if !ok || !in.running() {
		return nil, fmt.Errorf("%s: %w", plugin, ErrNotRunning)
	}
	return in, nil
}

// Provider returns the model provider id ("<plugin>/<name>") of a plugin,
// speaking through rec and syn.
func (b *Backends) Provider(id string, rec speech.Recognizer, syn speech.Synthesizer) (provider.Provider, error) {
	plugin, name, err := splitID(id)
	if err != nil {
		return nil, err
	}
	if rec == nil || syn == nil {
		return nil, errors.New("провайдеру из плагина нужны распознавание и синтез речи")
	}
	return &pluginProvider{b: b, id: id, plugin: plugin, name: name, rec: rec, syn: syn}, nil
}

// pluginProvider is a text model provider implemented by a plugin.
type pluginProvider struct {
	b                *Backends
	id, plugin, name string
	rec              speech.Recognizer
	syn              speech.Synthesizer
	sessions         atomic.Int64
}

func (p *pluginProvider) Name() string                     { return "plugin:" + p.id }
func (p *pluginProvider) OutputFormat() models.AudioFormat { return p.syn.Format() }
func (p *pluginProvider) AcceptsText() bool                { return true }
func (p *pluginProvider) Voices() []string                 { return provider.Voices(p.syn) }

func (p *pluginProvider) Connect(_ context.Context, opts provider.SessionOptions) (provider.Session, error) {
	if err := opts.InputFormat.Validate(); err != nil {
		return nil, err
	}
	in, err := p.b.instance(p.plugin)
	if err != nil {
		return nil, err
	}
	if !hasProvider(in, p.name) {
		return nil, fmt.Errorf("в плагине %s нет провайдера %q", p.plugin, p.name)
	}
	c := &conversation{
		p:       p,
		session: fmt.Sprintf("%s-%d", p.name, p.sessions.Add(1)),
		system:  opts.SystemInstruction,
		lang:    opts.Language,
		tools:   toolSpecs(opts.Tools),
	}
	s := pipeline.New(pipeline.Config{
		Recognizer: p.rec, Synthesizer: p.syn, Options: opts,
		Name: p.Name(), Logger: p.b.logger(),
	}, c)
	return &pluginSession{Session: s, c: c}, nil
}

func hasProvider(in *instance, name string) bool {
	for _, sp := range in.capabilities().providers {
		if sp.Name == name {
			return true
		}
	}
	return false
}

// toolSpecs declares the functions of a session to a plugin.
func toolSpecs(ts []tools.Tool) []pluginsdk.ToolSpec {
	out := make([]pluginsdk.ToolSpec, 0, len(ts))
	for _, t := range ts {
		spec := pluginsdk.ToolSpec{Name: t.Name(), Description: t.Description}
		for _, p := range t.Parameters {
			spec.Params = append(spec.Params, pluginsdk.Param{
				Name: p.Name, Type: string(p.Type), Description: p.Description,
				Required: p.Required, Enum: p.Enum, Minimum: p.Minimum, Maximum: p.Maximum,
			})
		}
		out = append(out, spec)
	}
	return out
}

// pluginSession tells the plugin when the conversation ends.
type pluginSession struct {
	*pipeline.Session
	c *conversation
}

func (s *pluginSession) Close() error {
	err := s.Session.Close()
	if in, ierr := s.c.p.b.instance(s.c.p.plugin); ierr == nil {
		in.notify(pluginsdk.MethodProviderClose, pluginsdk.ProviderCloseParams{Provider: s.c.p.name, Session: s.c.session})
	}
	return err
}

// conversation is the model side of a [pipeline.Session]; the plugin
// keeps the history.
type conversation struct {
	p       *pluginProvider
	session string
	system  string
	lang    string
	tools   []pluginsdk.ToolSpec
}

// Turn implements [pipeline.Conversation].
func (c *conversation) Turn(ctx context.Context, text string, out *pipeline.Output) error {
	in, err := c.p.b.instance(c.p.plugin)
	if err != nil {
		return err
	}
	params := pluginsdk.ProviderTurnParams{
		Provider: c.p.name, Session: c.session, Text: text,
		System: c.system, Language: c.lang, Tools: c.tools,
	}
	for range maxProviderRounds {
		fw := newForwarder(out)
		res, err := in.turn(ctx, params, fw.add)
		fw.close()
		if err != nil {
			return err
		}
		if res.Text != "" {
			out.Write(res.Text)
		}
		if len(res.ToolCalls) == 0 {
			return nil
		}
		out.Flush()
		calls := make([]provider.ToolCall, len(res.ToolCalls))
		for i, tc := range res.ToolCalls {
			if tc.Args == nil {
				tc.Args = map[string]any{}
			}
			calls[i] = provider.ToolCall{ID: tc.ID, Name: tc.Name, Args: tc.Args}
		}
		results, ok := out.CallTools(calls)
		if !ok {
			return nil
		}
		params.Text = ""
		params.ToolResults = make([]pluginsdk.ProviderToolResult, len(results))
		for i, r := range results {
			params.ToolResults[i] = pluginsdk.ProviderToolResult{ID: r.ID, Name: r.Name, Output: r.Output, Error: r.Err}
		}
	}
	return errors.New("слишком много вызовов функций за один ответ")
}

// forwarder passes streamed text to the output without blocking the
// connection of the plugin: the output may wait for speech synthesis,
// which can be a call to the same plugin.
type forwarder struct {
	out  *pipeline.Output
	mu   sync.Mutex
	buf  []string
	wake chan struct{}
	done chan struct{}
	end  bool
}

func newForwarder(out *pipeline.Output) *forwarder {
	f := &forwarder{out: out, wake: make(chan struct{}, 1), done: make(chan struct{})}
	go f.run()
	return f
}

func (f *forwarder) add(text string) {
	f.mu.Lock()
	f.buf = append(f.buf, text)
	f.mu.Unlock()
	select {
	case f.wake <- struct{}{}:
	default:
	}
}

func (f *forwarder) run() {
	defer close(f.done)
	for {
		f.mu.Lock()
		buf, end := f.buf, f.end
		f.buf = nil
		f.mu.Unlock()
		for _, t := range buf {
			f.out.Write(t)
		}
		if end && len(buf) == 0 {
			return
		}
		if len(buf) == 0 {
			<-f.wake
		}
	}
}

// close waits until everything added was written.
func (f *forwarder) close() {
	f.mu.Lock()
	f.end = true
	f.mu.Unlock()
	select {
	case f.wake <- struct{}{}:
	default:
	}
	<-f.done
}

// Speech returns the speech engine id ("<plugin>/<name>") of a plugin.
// What the engine cannot do (or while its plugin is not running) falls
// back to rec and syn, which may be nil.
func (b *Backends) Speech(id string, rec speech.Recognizer, syn speech.Synthesizer) (*Speech, error) {
	plugin, name, err := splitID(id)
	if err != nil {
		return nil, err
	}
	return &Speech{b: b, plugin: plugin, name: name, fallbackRec: rec, fallbackSyn: syn}, nil
}

// Speech is a speech engine of a plugin, as a [speech.Recognizer] and
// [speech.Synthesizer].
type Speech struct {
	b            *Backends
	plugin, name string
	fallbackRec  speech.Recognizer
	fallbackSyn  speech.Synthesizer

	mu    sync.Mutex
	voice string
}

// SetVoice chooses the voice of synthesized speech.
func (s *Speech) SetVoice(v string) {
	s.mu.Lock()
	s.voice = v
	s.mu.Unlock()
}

// spec returns the engine and its plugin, if it runs.
func (s *Speech) spec() (*instance, pluginsdk.SpeechSpec, bool) {
	in, err := s.b.instance(s.plugin)
	if err != nil {
		return nil, pluginsdk.SpeechSpec{}, false
	}
	for _, sp := range in.capabilities().speech {
		if sp.Name == s.name {
			return in, sp, true
		}
	}
	return nil, pluginsdk.SpeechSpec{}, false
}

// Voices lists the voices of the engine.
func (s *Speech) Voices() []string {
	if _, sp, ok := s.spec(); ok && len(sp.Voices) > 0 {
		return sp.Voices
	}
	return provider.Voices(s.fallbackSyn)
}

// Format is the format of synthesized speech.
func (s *Speech) Format() models.AudioFormat {
	if _, sp, ok := s.spec(); ok && sp.Synthesize {
		return models.AudioFormat{Encoding: models.EncodingLinear16, SampleRateHertz: sp.SampleRate, Channels: 1}
	}
	if s.fallbackSyn != nil {
		return s.fallbackSyn.Format()
	}
	return models.DefaultOutputFormat
}

// Synthesize implements [speech.Synthesizer].
func (s *Speech) Synthesize(ctx context.Context, text, language string) ([]byte, error) {
	in, sp, ok := s.spec()
	if !ok || !sp.Synthesize {
		if s.fallbackSyn == nil {
			return nil, fmt.Errorf("синтез речи %s/%s: %w", s.plugin, s.name, ErrNotRunning)
		}
		return s.fallbackSyn.Synthesize(ctx, text, language)
	}
	s.mu.Lock()
	voice := s.voice
	s.mu.Unlock()
	var res pluginsdk.SynthesizeResult
	err := in.request(ctx, pluginsdk.MethodSynthesize, pluginsdk.SynthesizeParams{
		Engine: s.name, Text: text, Language: language, Voice: voice,
	}, &res)
	if len(res.Audio)%2 == 1 {
		res.Audio = res.Audio[:len(res.Audio)-1]
	}
	return res.Audio, err
}

// Transcribe implements [speech.Recognizer].
func (s *Speech) Transcribe(ctx context.Context, pcm []byte, f models.AudioFormat, language string) (string, error) {
	in, sp, ok := s.spec()
	if !ok || !sp.Transcribe {
		if s.fallbackRec == nil {
			return "", fmt.Errorf("распознавание речи %s/%s: %w", s.plugin, s.name, ErrNotRunning)
		}
		return s.fallbackRec.Transcribe(ctx, pcm, f, language)
	}
	var res pluginsdk.TranscribeResult
	err := in.request(ctx, pluginsdk.MethodTranscribe, pluginsdk.TranscribeParams{
		Engine: s.name, Audio: pcm, SampleRate: f.SampleRateHertz, Language: language,
	}, &res)
	return strings.TrimSpace(res.Text), err
}

func (in *instance) running() bool {
	in.mu.Lock()
	defer in.mu.Unlock()
	return in.state == StateRunning && in.conn != nil
}
