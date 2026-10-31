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
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/yawaflua/marusya-gemini/internal/models"
	"github.com/yawaflua/marusya-gemini/internal/plugin"
	"github.com/yawaflua/marusya-gemini/internal/tools"
	"github.com/yawaflua/marusya-gemini/pkg/pluginsdk"
)

// EventPlugins is published when plugins start, stop or change.
const EventPlugins EventKind = "plugins"

// newPlugins creates the manager of the server plugins, or nil.
func (s *Server) newPlugins() *plugin.Manager {
	if s.cfg.PluginsDir == "" {
		return nil
	}
	m := plugin.New(plugin.Config{
		Dir:         s.cfg.PluginsDir,
		Host:        pluginsdk.HostServer,
		HostVersion: s.cfg.Version,
		Handler:     pluginHost{s},
		Reserved:    s.builtinTool,
		OnChange:    s.pluginsChanged,
		Logger:      s.cfg.Logger,
	})
	if s.cfg.PluginBackends != nil {
		s.cfg.PluginBackends.Set(m)
	}
	return m
}

// pluginInstructions is what the plugins add to the system instruction.
func pluginInstructions(m *plugin.Manager) string {
	if t := m.Instructions(); t != "" {
		return "\n\n" + t
	}
	return ""
}

// StartPlugins runs the server plugins until ctx is done. Without
// Config.PluginsDir it does nothing.
func (s *Server) StartPlugins(ctx context.Context) {
	if s.plugins == nil {
		return
	}
	s.plugins.Start(ctx)
	sub := s.events.subscribe()
	go func() {
		defer s.events.unsubscribe(sub)
		for {
			select {
			case <-ctx.Done():
				return
			case e := <-sub.events:
				if pe, ok := pluginEvent(e); ok {
					s.plugins.Event(pe)
				}
			case <-sub.devices:
			}
		}
	}()
}

// pluginEvent converts a panel event for plugins.
func pluginEvent(e Event) (pluginsdk.Event, bool) {
	switch e.Kind {
	case EventConnect, EventDisconnect, EventTurn, EventSkill, EventCommand, EventWake:
	default:
		return pluginsdk.Event{}, false
	}
	var data map[string]any
	b, _ := json.Marshal(e)
	json.Unmarshal(b, &data)
	for _, k := range []string{"id", "kind", "device"} {
		delete(data, k)
	}
	return pluginsdk.Event{Kind: string(e.Kind), Device: e.Device, Data: data}, true
}

// builtinTool reports whether name is a function of the server or the
// speaker, which plugins may not take.
func (s *Server) builtinTool(name string) bool {
	if _, ok := tools.Lookup(name); ok {
		return true
	}
	_, ok := s.cfg.Skills.Lookup(name)
	return ok
}

// pluginsChanged refreshes the model sessions when the set of plugin tools
// changed: the tools of a session are fixed when it opens.
func (s *Server) pluginsChanged() {
	var names []string
	for _, t := range s.plugins.Tools() {
		names = append(names, t.Name)
	}
	sig := strings.Join(names, ",") + "\x00" + s.plugins.Instructions()
	s.plmu.Lock()
	changed := sig != s.pluginTools
	s.pluginTools = sig
	s.plmu.Unlock()
	if changed {
		for _, d := range s.sessions() {
			d.refresh()
		}
	}
	s.events.publish(Event{Kind: EventPlugins})
}

// pluginHost serves the requests of server plugins.
type pluginHost struct{ s *Server }

var (
	errNoDevice     = errors.New("колонка не подключена")
	errManyDevices  = errors.New("подключено несколько колонок: укажите device")
	errNoAgentHalf  = errors.New("на колонке нет этого плагина")
	errNoPluginLink = errors.New("агент на колонке не поддерживает плагины")
)

// device finds the speaker a plugin means: by ID, or the only one.
func (h pluginHost) device(id string) (*deviceSession, error) {
	ds := h.s.sessions()
	if id == "" {
		switch len(ds) {
		case 0:
			return nil, errNoDevice
		case 1:
			return ds[0], nil
		}
		return nil, errManyDevices
	}
	for _, d := range ds {
		if d.hello.DeviceID == id {
			return d, nil
		}
	}
	return nil, errNoDevice
}

func (h pluginHost) Command(ctx context.Context, plugin, device, action string, args map[string]any) (map[string]any, error) {
	d, err := h.device(device)
	if err != nil {
		return nil, err
	}
	a := models.Action(action)
	if !d.supports(a) {
		return nil, fmt.Errorf("%s: %w", action, errUnsupported)
	}
	r, err := d.command(ctx, "", "", a, args, "plugin:"+plugin)
	switch {
	case err != nil:
		return nil, err
	case !r.OK:
		return nil, errors.New(r.Error)
	}
	return r.Data, nil
}

func (h pluginHost) Say(ctx context.Context, plugin, device, text string, listen bool) (string, error) {
	var d *deviceSession
	if device == "" {
		d = h.s.askDevice("")
	} else {
		d, _ = h.device(device)
	}
	if d == nil {
		return "", errNoDevice
	}
	answer, err := d.announce(ctx, text, listen, "plugin:"+plugin)
	if errors.Is(err, errNoAnswer) {
		err = nil
	}
	return answer, err
}

// Request makes a typed request to the assistant on a speaker.
func (h pluginHost) Request(_ context.Context, plugin, device, text string) error {
	text = strings.TrimSpace(text)
	if text == "" || utf8.RuneCountInString(text) > maxAskText {
		return fmt.Errorf("текст запроса: от 1 до %d символов", maxAskText)
	}
	d, err := h.device(device)
	if err != nil {
		return err
	}
	return d.request(text, "plugin:"+plugin)
}

func (h pluginHost) Devices(context.Context) []pluginsdk.Device {
	var out []pluginsdk.Device
	for _, d := range h.s.sessions() {
		out = append(out, d.pluginDevice())
	}
	return out
}

func (h pluginHost) Send(ctx context.Context, plugin, device string, data json.RawMessage) error {
	var ds []*deviceSession
	if device == "" {
		ds = h.s.sessions()
	} else {
		d, err := h.device(device)
		if err != nil {
			return err
		}
		ds = []*deviceSession{d}
	}
	sent := 0
	var last error
	for _, d := range ds {
		if !d.supportsFeature(models.FeaturePlugins) {
			last = errNoPluginLink
			continue
		}
		msg := &models.Message{Type: models.TypePlugin, Plugin: &models.PluginMessage{Plugin: plugin, Data: data}}
		if err := d.send(msg); err != nil {
			last = err
			continue
		}
		sent++
	}
	if sent == 0 {
		if last == nil {
			last = errNoDevice
		}
		return last
	}
	return nil
}

// pluginDevice describes the speaker to plugins.
func (d *deviceSession) pluginDevice() pluginsdk.Device {
	dev := d.device()
	return pluginsdk.Device{ID: dev.ID, Language: dev.Language, Location: dev.Location, TimeZone: dev.TimeZone}
}

// agentTools returns the valid tools of the plugins on the speaker, by
// name. Names taken by the server or its plugins are dropped.
func (d *deviceSession) agentTools() map[string]plugin.Tool {
	out := map[string]plugin.Tool{}
	for _, pt := range d.hello.PluginTools {
		t, err := plugin.Announced(pt)
		if err != nil {
			continue
		}
		if d.srv.builtinTool(t.Name) {
			continue
		}
		if _, ok := d.srv.plugins.Lookup(t.Name); ok {
			continue
		}
		if _, dup := out[t.Name]; !dup {
			out[t.Name] = t
		}
	}
	return out
}

// pluginTools declares the plugin functions to the model: those of the
// server plugins, then those of the plugins on the speaker.
func (d *deviceSession) pluginTools() []tools.Tool {
	var out []tools.Tool
	for _, t := range d.srv.plugins.Tools() {
		out = append(out, plugin.ToolOf(t.ToolSpec))
	}
	agent := d.agentTools()
	names := make([]string, 0, len(agent))
	for n := range agent {
		names = append(names, n)
	}
	slices.Sort(names)
	for _, n := range names {
		out = append(out, plugin.ToolOf(agent[n].ToolSpec))
	}
	return out
}

// runPlugin runs a function of a server plugin.
func (d *deviceSession) runPlugin(c pluginCall) (map[string]any, error) {
	ctx, cancel := context.WithTimeout(d.ctx, d.cfg.CommandTimeout)
	defer cancel()
	start := time.Now()
	out, err := d.srv.plugins.Call(ctx, c.name, c.args, d.pluginDevice())
	ev := Event{Kind: EventSkill, Session: d.id, Device: d.hello.DeviceID, Source: "plugin:" + c.plugin,
		Name: c.name, Args: c.args, LatencyMS: time.Since(start).Milliseconds(), Result: out}
	if err != nil {
		ev.Error = err.Error()
	}
	d.log.Info("plugin function", "plugin", c.plugin, "name", c.name, "args", c.args, "latency", time.Since(start), "err", err)
	d.srv.events.publish(ev)
	return out, err
}

type pluginCall struct {
	plugin, name string
	args         map[string]any
}

// onPluginMessage passes data from the speaker half of a plugin to the
// server half.
func (d *deviceSession) onPluginMessage(m *models.PluginMessage) {
	if m == nil {
		return
	}
	if d.srv.plugins == nil {
		d.log.Debug("plugin message without server plugins", "plugin", m.Plugin)
		return
	}
	if err := d.srv.plugins.Deliver(m.Plugin, d.hello.DeviceID, m.Data); err != nil {
		d.log.Debug("plugin message", "plugin", m.Plugin, "err", err)
	}
}

// Panel API.

func (s *Server) registerPluginAPI() {
	s.mux.HandleFunc("GET /panel/api/plugins", s.panelAPI(s.handlePlugins))
	s.mux.HandleFunc("POST /panel/api/plugins/rescan", s.panelAPI(s.handleRescanPlugins))
	s.mux.HandleFunc("POST /panel/api/plugins/{id}/enabled", s.panelAPI(s.handleEnablePlugin))
	s.mux.HandleFunc("POST /panel/api/plugins/{id}/restart", s.panelAPI(s.handleRestartPlugin))
	s.mux.HandleFunc("PUT /panel/api/plugins/{id}/config", s.panelAPI(s.handlePluginConfig))
}

// pluginsState is what the plugins tab of the panel shows.
type pluginsState struct {
	Enabled bool            `json:"enabled"`
	Dir     string          `json:"dir,omitempty"`
	Plugins []plugin.Status `json:"plugins"`
	Errors  []string        `json:"errors"`
	// Devices lists the plugin functions of each connected speaker.
	Devices []devicePlugins `json:"devices"`
	// Catalog is the address of the plugin catalog, empty if off.
	Catalog string `json:"catalog,omitempty"`
	// FromCatalog lists the plugins installed from the catalog, by id.
	FromCatalog map[string]plugin.CatalogInstall `json:"from_catalog"`
}

type devicePlugins struct {
	Device  string              `json:"device"`
	Plugins bool                `json:"plugins"` // the agent runs plugins
	Tools   []models.PluginTool `json:"tools"`
	// Catalog means the agent installs plugins from the catalog.
	Catalog bool `json:"catalog"`
	// FromCatalog are the catalog plugins the speaker should run.
	FromCatalog []models.CatalogPlugin `json:"from_catalog"`
}

func (s *Server) pluginsState() pluginsState {
	st := pluginsState{Plugins: []plugin.Status{}, Errors: []string{}, Devices: []devicePlugins{}}
	if s.plugins != nil {
		st.Enabled, st.Dir = true, s.cfg.PluginsDir
		if l := s.plugins.List(); l != nil {
			st.Plugins = l
		}
		if e := s.plugins.Errors(); e != nil {
			st.Errors = e
		}
	}
	st.Catalog = s.cfg.PluginRegistry
	_, st.FromCatalog = s.installedPlugins()
	for _, d := range s.sessions() {
		dp := devicePlugins{
			Device: d.hello.DeviceID, Plugins: d.supportsFeature(models.FeaturePlugins),
			Tools:       append([]models.PluginTool{}, d.hello.PluginTools...),
			Catalog:     d.supportsFeature(models.FeatureCatalogPlugins),
			FromCatalog: s.catalogPluginsFor(d.hello.DeviceID),
		}
		st.Devices = append(st.Devices, dp)
	}
	slices.SortFunc(st.Devices, func(a, b devicePlugins) int { return strings.Compare(a.Device, b.Device) })
	return st
}

var errNoPlugins = errors.New("плагины выключены: задайте каталог -plugins_dir (MARUSYA_PLUGINS_DIR)")

func (s *Server) handlePlugins(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, s.pluginsState())
}

func (s *Server) pluginError(w http.ResponseWriter, err error) bool {
	switch {
	case err == nil:
		return false
	case errors.Is(err, plugin.ErrUnknown):
		writeError(w, http.StatusNotFound, errors.New("нет такого плагина"))
	default:
		writeError(w, http.StatusBadRequest, err)
	}
	return true
}

func (s *Server) handleRescanPlugins(w http.ResponseWriter, _ *http.Request) {
	if s.plugins == nil {
		writeError(w, http.StatusConflict, errNoPlugins)
		return
	}
	s.plugins.Rescan()
	writeJSON(w, http.StatusOK, s.pluginsState())
}

func (s *Server) handleEnablePlugin(w http.ResponseWriter, r *http.Request) {
	if s.plugins == nil {
		writeError(w, http.StatusConflict, errNoPlugins)
		return
	}
	var req struct {
		Enabled bool `json:"enabled"`
	}
	if !readJSON(w, r, &req) {
		return
	}
	if s.pluginError(w, s.plugins.SetEnabled(r.PathValue("id"), req.Enabled)) {
		return
	}
	writeJSON(w, http.StatusOK, s.pluginsState())
}

func (s *Server) handleRestartPlugin(w http.ResponseWriter, r *http.Request) {
	if s.plugins == nil {
		writeError(w, http.StatusConflict, errNoPlugins)
		return
	}
	if s.pluginError(w, s.plugins.Restart(r.PathValue("id"))) {
		return
	}
	writeJSON(w, http.StatusOK, s.pluginsState())
}

func (s *Server) handlePluginConfig(w http.ResponseWriter, r *http.Request) {
	if s.plugins == nil {
		writeError(w, http.StatusConflict, errNoPlugins)
		return
	}
	var cfg json.RawMessage
	if !readJSON(w, r, &cfg) {
		return
	}
	if s.pluginError(w, s.plugins.SetConfig(r.PathValue("id"), cfg)) {
		return
	}
	writeJSON(w, http.StatusOK, s.pluginsState())
}
