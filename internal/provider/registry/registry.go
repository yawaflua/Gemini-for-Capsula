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

// Package registry keeps the model providers configured in the web panel
// (Gemini, OpenAI, Anthropic and servers compatible with them) and switches
// between them while the server runs.
//
// The provider configured with flags and environment variables is always
// present as [DefaultID] and cannot be edited.
package registry

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"

	"github.com/yawaflua/marusya-gemini/internal/provider"
)

// DefaultID identifies the provider configured at startup.
const DefaultID = "default"

// Kind is a provider backend.
type Kind string

const (
	// KindGemini is the Gemini Live API (speech to speech).
	KindGemini Kind = "gemini"
	// KindOpenAIRealtime is the OpenAI Realtime API (speech to speech).
	KindOpenAIRealtime Kind = "openai-realtime"
	// KindOpenAI is an OpenAI compatible Chat Completions API with speech
	// recognition and synthesis around it.
	KindOpenAI Kind = "openai"
	// KindAnthropic is the Claude Messages API with speech recognition and
	// synthesis around it.
	KindAnthropic Kind = "anthropic"
	// KindPlugin is a text model implemented by a server plugin; Model is
	// "<plugin>/<provider>".
	KindPlugin Kind = "plugin"
)

// KindInfo describes a [Kind] for the panel.
type KindInfo struct {
	Kind    Kind   `json:"kind"`
	Label   string `json:"label"`
	BaseURL string `json:"base_url"` // default endpoint
	Model   string `json:"model"`    // default model
	// Pipeline kinds are text models; they need speech recognition and
	// synthesis (Spec.Speech).
	Pipeline bool `json:"pipeline"`
}

// Spec configures one provider.
type Spec struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	Kind Kind   `json:"kind"`
	// BaseURL overrides the endpoint of the kind, for compatible servers
	// (OpenRouter, Ollama, a gateway, ...).
	BaseURL string `json:"base_url,omitempty"`
	APIKey  string `json:"api_key,omitempty"`
	Model   string `json:"model,omitempty"`
	Voice   string `json:"voice,omitempty"`
	// Speech chooses speech recognition and synthesis for pipeline kinds:
	// "" uses the server's Gemini speech, SpeechSelf this provider's own
	// OpenAI compatible audio endpoints, anything else is the ID of a
	// Gemini or OpenAI provider whose key is used, and SpeechPlugin +
	// "<plugin>/<engine>" a speech engine of a server plugin.
	Speech string `json:"speech,omitempty"`
}

// SpeechPlugin prefixes a speech engine of a plugin in [Spec.Speech].
const SpeechPlugin = "plugin:"

// SpeechSelf in [Spec.Speech] uses the provider's own audio endpoints.
const SpeechSelf = "self"

// View is a [Spec] as shown in the panel: without the API key.
type View struct {
	Spec
	HasKey   bool `json:"has_key"`
	ReadOnly bool `json:"read_only,omitempty"`
}

var idRE = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,31}$`)

const (
	maxName = 64
	maxURL  = 512
	maxKey  = 512
)

// validate normalizes s and checks it against the other specs.
func (s *Spec) validate(others []Spec) error {
	s.ID = strings.ToLower(strings.TrimSpace(s.ID))
	s.Name = strings.TrimSpace(s.Name)
	s.BaseURL = strings.TrimSpace(s.BaseURL)
	s.APIKey = strings.TrimSpace(s.APIKey)
	s.Model = strings.TrimSpace(s.Model)
	s.Voice = strings.TrimSpace(s.Voice)
	s.Speech = strings.TrimSpace(s.Speech)
	switch {
	case s.ID == DefaultID || !idRE.MatchString(s.ID):
		return fmt.Errorf("недопустимый идентификатор %q: нужны латинские буквы, цифры, - и _", s.ID)
	case s.Name == "" || len(s.Name) > maxName:
		return errors.New("нужно название до 64 символов")
	case len(s.BaseURL) > maxURL || len(s.APIKey) > maxKey || len(s.Model) > 128 || len(s.Voice) > 64:
		return errors.New("слишком длинное значение")
	case strings.ContainsAny(s.Model+s.Voice, " \t\n"):
		return errors.New("модель и голос пишутся без пробелов")
	}
	if s.BaseURL != "" && !strings.Contains(s.BaseURL, "://") {
		return fmt.Errorf("адрес %q должен начинаться с https://", s.BaseURL)
	}
	info, ok := kindInfo(s.Kind)
	if !ok {
		return fmt.Errorf("неизвестный тип провайдера %q", s.Kind)
	}
	if s.Model == "" {
		s.Model = info.Model
	}
	if s.Kind == KindPlugin && !strings.Contains(s.Model, "/") {
		return errors.New("модель провайдера из плагина пишется как «плагин/провайдер»")
	}
	if !info.Pipeline {
		s.Speech = ""
		return nil
	}
	switch {
	case s.Speech == "":
	case strings.HasPrefix(s.Speech, SpeechPlugin):
		if !strings.Contains(strings.TrimPrefix(s.Speech, SpeechPlugin), "/") {
			return errors.New("речь из плагина пишется как «plugin:плагин/движок»")
		}
	case s.Speech == SpeechSelf:
		if s.Kind != KindOpenAI {
			return errors.New("свои аудио-эндпоинты бывают только у OpenAI-совместимого провайдера")
		}
	default:
		i := slices.IndexFunc(others, func(o Spec) bool { return o.ID == s.Speech })
		if i < 0 || s.Speech == s.ID {
			return fmt.Errorf("нет провайдера %q для распознавания и синтеза речи", s.Speech)
		}
		if k := others[i].Kind; k == KindAnthropic {
			return errors.New("речь умеют распознавать только провайдеры Gemini и OpenAI")
		}
	}
	return nil
}

// Builder creates providers from specs.
type Builder func(ctx context.Context, spec Spec, all []Spec) (provider.Provider, error)

// file is the format of the providers file.
type file struct {
	Active    string `json:"active,omitempty"`
	Providers []Spec `json:"providers"`
}

// Registry is the set of configured providers.
type Registry struct {
	path    string
	builtin provider.Provider
	build   Builder
	sw      *Switch
	log     *slog.Logger

	mu     sync.Mutex
	specs  []Spec
	active string
}

// Open loads the providers kept in path (if not empty) and returns a
// registry whose [Registry.Provider] is builtin until [Registry.Restore]
// or [Registry.Activate].
func Open(path string, builtin provider.Provider, build Builder, log *slog.Logger) (*Registry, error) {
	if log == nil {
		log = slog.Default()
	}
	r := &Registry{path: path, builtin: builtin, build: build, sw: NewSwitch(builtin), log: log, active: DefaultID}
	if path == "" {
		return r, nil
	}
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return r, nil
	}
	if err != nil {
		return r, err
	}
	var f file
	if err := json.Unmarshal(b, &f); err != nil {
		return r, fmt.Errorf("%s: %w", path, err)
	}
	r.specs = f.Providers
	if f.Active != "" {
		r.active = f.Active
	}
	return r, nil
}

// Provider returns the switch that delegates to the active provider.
func (r *Registry) Provider() *Switch { return r.sw }

// Restore activates the provider that was active when the registry was
// saved. On failure the startup provider stays active.
func (r *Registry) Restore(ctx context.Context) error {
	r.mu.Lock()
	id := r.active
	r.mu.Unlock()
	if id == DefaultID {
		return nil
	}
	if err := r.Activate(ctx, id); err != nil {
		r.mu.Lock()
		r.active = DefaultID
		r.mu.Unlock()
		return err
	}
	return nil
}

// Kinds lists the provider kinds.
func Kinds() []KindInfo { return slices.Clone(kinds) }

func kindInfo(k Kind) (KindInfo, bool) {
	i := slices.IndexFunc(kinds, func(i KindInfo) bool { return i.Kind == k })
	if i < 0 {
		return KindInfo{}, false
	}
	return kinds[i], true
}

// List returns the providers and the ID of the active one.
func (r *Registry) List() ([]View, string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := []View{{Spec: Spec{ID: DefaultID, Name: "Из конфигурации сервера: " + r.builtin.Name()}, HasKey: true, ReadOnly: true}}
	for _, s := range r.specs {
		key := s.APIKey != ""
		s.APIKey = ""
		out = append(out, View{Spec: s, HasKey: key})
	}
	return out, r.active
}

// Active returns the ID of the active provider.
func (r *Registry) Active() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.active
}

// Put adds or replaces a provider. An empty API key keeps the saved one.
// If the provider is in use, it is rebuilt: active reports that the
// provider of new sessions changed.
func (r *Registry) Put(ctx context.Context, s Spec) (active bool, err error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	others := slices.DeleteFunc(slices.Clone(r.specs), func(o Spec) bool { return o.ID == s.ID })
	if err := s.validate(others); err != nil {
		return false, err
	}
	i := slices.IndexFunc(r.specs, func(o Spec) bool { return o.ID == s.ID })
	if i >= 0 && s.APIKey == "" {
		s.APIKey = r.specs[i].APIKey
	}
	specs := slices.Clone(r.specs)
	if i >= 0 {
		specs[i] = s
	} else {
		specs = append(specs, s)
	}
	// Providers using this one for speech are rebuilt too.
	affected := r.active == s.ID || r.speechOf(specs, r.active) == s.ID
	var p provider.Provider
	if affected {
		if p, err = r.build(ctx, r.find(specs, r.active), specs); err != nil {
			return false, fmt.Errorf("провайдер используется, но не запускается: %w", err)
		}
	}
	if err := r.save(r.active, specs); err != nil {
		return false, err
	}
	r.specs = specs
	if affected {
		r.sw.Set(p)
	}
	return affected, nil
}

// Delete removes a provider that is neither active nor used by another
// one for speech.
func (r *Registry) Delete(id string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if id == r.active {
		return errors.New("этот провайдер сейчас используется; сначала выберите другой")
	}
	i := slices.IndexFunc(r.specs, func(o Spec) bool { return o.ID == id })
	if i < 0 {
		return fmt.Errorf("нет провайдера %q", id)
	}
	for _, o := range r.specs {
		if o.Speech == id {
			return fmt.Errorf("провайдер %q распознаёт речь для «%s»", id, o.Name)
		}
	}
	specs := slices.Delete(slices.Clone(r.specs), i, i+1)
	if err := r.save(r.active, specs); err != nil {
		return err
	}
	r.specs = specs
	return nil
}

// Activate makes id the provider of new sessions.
func (r *Registry) Activate(ctx context.Context, id string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	var p provider.Provider = r.builtin
	if id != DefaultID {
		s := r.find(r.specs, id)
		if s.ID == "" {
			return fmt.Errorf("нет провайдера %q", id)
		}
		var err error
		if p, err = r.build(ctx, s, r.specs); err != nil {
			return err
		}
	}
	if err := r.save(id, r.specs); err != nil {
		return err
	}
	r.active = id
	r.sw.Set(p)
	r.log.Info("provider activated", "id", id, "provider", p.Name())
	return nil
}

// SetTuning saves the model and voice chosen in the settings into the
// active provider, so they survive switching away and back.
func (r *Registry) SetTuning(t provider.Tuning) {
	r.mu.Lock()
	defer r.mu.Unlock()
	i := slices.IndexFunc(r.specs, func(o Spec) bool { return o.ID == r.active })
	if i < 0 || (r.specs[i].Model == t.Model && r.specs[i].Voice == t.Voice) {
		return
	}
	specs := slices.Clone(r.specs)
	specs[i].Model, specs[i].Voice = t.Model, t.Voice
	if err := r.save(r.active, specs); err != nil {
		r.log.Warn("save providers", "err", err)
		return
	}
	r.specs = specs
}

func (r *Registry) find(specs []Spec, id string) Spec {
	if i := slices.IndexFunc(specs, func(o Spec) bool { return o.ID == id }); i >= 0 {
		return specs[i]
	}
	return Spec{}
}

func (r *Registry) speechOf(specs []Spec, id string) string { return r.find(specs, id).Speech }

// save writes the providers atomically and readable by the owner only:
// the file holds API keys.
func (r *Registry) save(active string, specs []Spec) error {
	if r.path == "" {
		return nil
	}
	b, err := json.MarshalIndent(file{Active: active, Providers: specs}, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(r.path), ".providers-*")
	if err != nil {
		return fmt.Errorf("не удалось сохранить провайдеров: %w", err)
	}
	defer os.Remove(tmp.Name())
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(append(b, '\n')); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), r.path)
}
