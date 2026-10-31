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

// Package plugin runs Marusya plugins: programs next to a plugin.json
// manifest that talk JSON-RPC over stdio (see pkg/pluginsdk). The server
// and the agent each run a [Manager] for the plugins of their side.
package plugin

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"time"

	"github.com/yawaflua/marusya-gemini/pkg/pluginsdk"
)

// Handler serves what plugins ask of the host.
type Handler interface {
	// Command runs a speaker action.
	Command(ctx context.Context, plugin, device, action string, args map[string]any) (map[string]any, error)
	// Say speaks text on a speaker and, with listen, returns the answer.
	Say(ctx context.Context, plugin, device, text string, listen bool) (string, error)
	// Devices lists the connected speakers.
	Devices(ctx context.Context) []pluginsdk.Device
	// Send delivers data to the other half of the plugin.
	Send(ctx context.Context, plugin, device string, data json.RawMessage) error
}

// ServerHandler is implemented by the [Handler] of the server for what only
// server plugins may ask.
type ServerHandler interface {
	// Request makes a request to the assistant on a speaker as if the
	// user had said text.
	Request(ctx context.Context, plugin, device, text string) error
}

// Config configures a [Manager].
type Config struct {
	// Dir holds one subdirectory per plugin.
	Dir string
	// Host is pluginsdk.HostServer or pluginsdk.HostAgent.
	Host        string
	HostVersion string
	// Device is the speaker the agent runs on (agent only).
	Device *pluginsdk.Device
	// Handler serves the requests of plugins; may be nil.
	Handler Handler
	// Reserved reports names of built-in tools plugins may not use.
	Reserved func(name string) bool
	// StateFile keeps which plugins are disabled and their configuration;
	// empty uses .state.json in Dir.
	StateFile string
	// OnChange is called when plugins start, stop or change their tools.
	OnChange func()
	Logger   *slog.Logger
}

// Tool is a tool declared by a plugin.
type Tool struct {
	Plugin string
	pluginsdk.ToolSpec
}

// Status describes a plugin for the web panel.
type Status struct {
	ID          string          `json:"id"`
	Name        string          `json:"name,omitempty"`
	Version     string          `json:"version,omitempty"`
	Description string          `json:"description,omitempty"`
	Dir         string          `json:"dir"`
	Sides       []string        `json:"sides"`
	Enabled     bool            `json:"enabled"`
	State       string          `json:"state"`
	Error       string          `json:"error,omitempty"`
	Restarts    int             `json:"restarts,omitempty"`
	Since       *time.Time      `json:"since,omitempty"`
	Tools       []string        `json:"tools"`
	Config      json.RawMessage `json:"config,omitempty"`
	// What a server plugin implements besides tools.
	Providers    []string `json:"providers,omitempty"`
	Speech       []string `json:"speech,omitempty"`
	HTTP         string   `json:"http,omitempty"` // "", "login" or "public"
	Instructions string   `json:"instructions,omitempty"`
}

// state is persisted in Config.StateFile.
type state struct {
	Disabled []string                   `json:"disabled,omitempty"`
	Config   map[string]json.RawMessage `json:"config,omitempty"`
}

// Manager runs the plugins of one side.
type Manager struct {
	cfg Config
	log *slog.Logger

	mu        sync.Mutex
	ctx       context.Context
	plugins   map[string]*instance // only those with a side here
	order     []string
	st        state
	errs      []string // broken manifests
	changeMu  sync.Mutex
	changeDue bool
}

// New returns a manager; call [Manager.Start] to run the plugins.
func New(cfg Config) *Manager {
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	if cfg.Host == "" {
		cfg.Host = pluginsdk.HostServer
	}
	if cfg.StateFile == "" && cfg.Dir != "" {
		cfg.StateFile = filepath.Join(cfg.Dir, ".state.json")
	}
	m := &Manager{cfg: cfg, log: cfg.Logger.With("component", "plugins"), plugins: map[string]*instance{}}
	m.loadState()
	return m
}

// Start runs the enabled plugins and returns once each was initialized or
// failed to start (at most a few seconds). Plugins stop when ctx is done.
func (m *Manager) Start(ctx context.Context) {
	m.mu.Lock()
	m.ctx = ctx
	m.mu.Unlock()
	m.Rescan()
	go func() {
		<-ctx.Done()
		m.stopAll()
	}()
}

// Rescan loads new plugins from the directory and forgets removed ones.
func (m *Manager) Rescan() {
	ms, errs := Discover(m.cfg.Dir)
	m.mu.Lock()
	ctx := m.ctx
	m.errs = m.errs[:0]
	for _, err := range errs {
		m.errs = append(m.errs, err.Error())
		m.log.Warn("plugin manifest", "err", err)
	}
	seen := map[string]bool{}
	var start, stop []*instance
	m.order = m.order[:0]
	for _, man := range ms {
		if man.Side(m.cfg.Host) == nil {
			continue
		}
		seen[man.ID] = true
		m.order = append(m.order, man.ID)
		in, ok := m.plugins[man.ID]
		if !ok {
			in = newInstance(man, m)
			m.plugins[man.ID] = in
		}
		if m.enabledLocked(man.ID) {
			start = append(start, in)
		} else {
			in.state = StateDisabled
		}
	}
	for id, in := range m.plugins {
		if !seen[id] {
			delete(m.plugins, id)
			stop = append(stop, in)
		}
	}
	m.mu.Unlock()

	for _, in := range stop {
		in.stop(StateStopped)
	}
	if ctx == nil {
		return
	}
	for _, in := range start {
		in.start(ctx)
	}
	for _, in := range start {
		in.waitReady()
	}
	m.changed()
}

func (m *Manager) stopAll() {
	m.mu.Lock()
	all := make([]*instance, 0, len(m.plugins))
	for _, in := range m.plugins {
		all = append(all, in)
	}
	m.mu.Unlock()
	var wg sync.WaitGroup
	for _, in := range all {
		wg.Add(1)
		go func() {
			defer wg.Done()
			in.stop(StateStopped)
		}()
	}
	wg.Wait()
}

// changed calls OnChange, coalescing bursts.
func (m *Manager) changed() {
	if m.cfg.OnChange == nil {
		return
	}
	m.changeMu.Lock()
	defer m.changeMu.Unlock()
	if m.changeDue {
		return
	}
	m.changeDue = true
	time.AfterFunc(200*time.Millisecond, func() {
		m.changeMu.Lock()
		m.changeDue = false
		m.changeMu.Unlock()
		m.cfg.OnChange()
	})
}

func (m *Manager) instances() []*instance {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]*instance, 0, len(m.order))
	for _, id := range m.order {
		out = append(out, m.plugins[id])
	}
	return out
}

func (m *Manager) instance(id string) (*instance, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	in, ok := m.plugins[id]
	return in, ok
}

// Tools returns the tools of the running plugins. When two plugins declare
// the same name, the one with the smaller id wins.
func (m *Manager) Tools() []Tool {
	if m == nil {
		return nil
	}
	var out []Tool
	seen := map[string]string{}
	for _, in := range m.instances() {
		in.mu.Lock()
		tools := in.tools
		in.mu.Unlock()
		for _, t := range tools {
			if other, ok := seen[t.Name]; ok {
				in.log.Warn("tool ignored: another plugin declares it", "tool", t.Name, "owner", other)
				continue
			}
			seen[t.Name] = in.m.ID
			out = append(out, Tool{Plugin: in.m.ID, ToolSpec: t})
		}
	}
	return out
}

// Lookup finds the tool called name.
func (m *Manager) Lookup(name string) (Tool, bool) {
	for _, t := range m.Tools() {
		if t.Name == name {
			return t, true
		}
	}
	return Tool{}, false
}

// ErrNoTool means no running plugin declares the tool.
var ErrNoTool = errors.New("no plugin has this function")

// Call runs a tool.
func (m *Manager) Call(ctx context.Context, name string, args map[string]any, dev pluginsdk.Device) (map[string]any, error) {
	if m == nil {
		return nil, fmt.Errorf("%s: %w", name, ErrNoTool)
	}
	t, ok := m.Lookup(name)
	if !ok {
		return nil, fmt.Errorf("%s: %w", name, ErrNoTool)
	}
	in, ok := m.instance(t.Plugin)
	if !ok {
		return nil, fmt.Errorf("%s: %w", name, ErrNoTool)
	}
	return in.call(ctx, name, args, dev)
}

// Event sends an event to the plugins subscribed to its kind.
func (m *Manager) Event(e pluginsdk.Event) {
	if m == nil {
		return
	}
	for _, in := range m.instances() {
		if in.wants(e.Kind) {
			in.notify(pluginsdk.MethodEvent, e)
		}
	}
}

// Deliver passes data from the other half of a plugin.
func (m *Manager) Deliver(plugin, device string, data json.RawMessage) error {
	in, ok := m.instance(plugin)
	if !ok {
		return fmt.Errorf("plugin %s does not run here", plugin)
	}
	in.notify(pluginsdk.MethodMessage, pluginsdk.Message{Device: device, Data: data})
	return nil
}

// List describes every plugin of this side.
func (m *Manager) List() []Status {
	if m == nil {
		return nil
	}
	var out []Status
	for _, in := range m.instances() {
		in.mu.Lock()
		s := Status{
			ID: in.m.ID, Name: in.m.Name, Version: in.m.Version, Description: in.m.Description,
			Dir: in.m.Dir, Sides: in.m.Sides(), State: in.state, Error: in.err,
			Restarts: in.restarts, Tools: []string{},
		}
		for _, t := range in.tools {
			s.Tools = append(s.Tools, t.Name)
		}
		for _, p := range in.caps.providers {
			s.Providers = append(s.Providers, p.Name)
		}
		for _, sp := range in.caps.speech {
			s.Speech = append(s.Speech, sp.Name)
		}
		switch h := in.caps.http; {
		case h != nil && h.Public:
			s.HTTP = "public"
		case h != nil:
			s.HTTP = "login"
		}
		s.Instructions = in.caps.instructions
		if in.state == StateRunning {
			t := in.started
			s.Since = &t
		}
		in.mu.Unlock()
		s.Enabled = m.Enabled(s.ID)
		s.Config = m.config(in.m)
		out = append(out, s)
	}
	return out
}

// Errors lists the manifests that could not be loaded.
func (m *Manager) Errors() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return slices.Clone(m.errs)
}

// Enabled reports whether a plugin is enabled.
func (m *Manager) Enabled(id string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.enabledLocked(id)
}

func (m *Manager) enabledLocked(id string) bool { return !slices.Contains(m.st.Disabled, id) }

// ErrUnknown means there is no such plugin.
var ErrUnknown = errors.New("unknown plugin")

// SetEnabled starts or stops a plugin and remembers it.
func (m *Manager) SetEnabled(id string, on bool) error {
	in, ok := m.instance(id)
	if !ok {
		return ErrUnknown
	}
	m.mu.Lock()
	m.st.Disabled = slices.DeleteFunc(m.st.Disabled, func(s string) bool { return s == id })
	if !on {
		m.st.Disabled = append(m.st.Disabled, id)
	}
	ctx := m.ctx
	err := m.saveLocked()
	m.mu.Unlock()
	if on {
		if ctx != nil {
			in.start(ctx)
			in.waitReady()
		}
	} else {
		in.stop(StateDisabled)
	}
	m.changed()
	return err
}

// Restart restarts a running plugin, e.g. after its configuration changed.
func (m *Manager) Restart(id string) error {
	in, ok := m.instance(id)
	if !ok {
		return ErrUnknown
	}
	if !m.Enabled(id) {
		return nil
	}
	in.stop(StateStopped)
	m.mu.Lock()
	ctx := m.ctx
	m.mu.Unlock()
	if ctx != nil {
		in.start(ctx)
		in.waitReady()
	}
	return nil
}

// SetConfig replaces the configuration of a plugin (a JSON object; null
// returns to the manifest's) and restarts it.
func (m *Manager) SetConfig(id string, cfg json.RawMessage) error {
	if _, ok := m.instance(id); !ok {
		return ErrUnknown
	}
	reset := len(cfg) == 0 || string(cfg) == "null"
	if !reset {
		var obj map[string]any
		if err := json.Unmarshal(cfg, &obj); err != nil || obj == nil {
			return errors.New("настройки плагина должны быть JSON-объектом")
		}
	}
	m.mu.Lock()
	if m.st.Config == nil {
		m.st.Config = map[string]json.RawMessage{}
	}
	if reset {
		delete(m.st.Config, id)
	} else {
		m.st.Config[id] = cfg
	}
	err := m.saveLocked()
	m.mu.Unlock()
	if err != nil {
		return err
	}
	return m.Restart(id)
}

// config returns the effective configuration of a plugin.
func (m *Manager) config(man Manifest) json.RawMessage {
	m.mu.Lock()
	defer m.mu.Unlock()
	if c, ok := m.st.Config[man.ID]; ok {
		return c
	}
	return man.Config
}

func (m *Manager) loadState() {
	if m.cfg.StateFile == "" {
		return
	}
	b, err := os.ReadFile(m.cfg.StateFile)
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			m.log.Warn("read plugin state", "err", err)
		}
		return
	}
	if err := json.Unmarshal(b, &m.st); err != nil {
		m.log.Warn("read plugin state", "file", m.cfg.StateFile, "err", err)
	}
	for id, c := range m.st.Config {
		var buf bytes.Buffer
		if json.Compact(&buf, c) == nil {
			m.st.Config[id] = buf.Bytes()
		}
	}
}

func (m *Manager) saveLocked() error {
	if m.cfg.StateFile == "" {
		return nil
	}
	b, err := json.MarshalIndent(m.st, "", "  ")
	if err != nil {
		return err
	}
	tmp := m.cfg.StateFile + ".tmp"
	// Configuration may hold API keys of the plugins.
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return fmt.Errorf("save plugin state: %w", err)
	}
	return os.Rename(tmp, m.cfg.StateFile)
}
