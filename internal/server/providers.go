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
	"errors"
	"net/http"

	"github.com/yawaflua/marusya-gemini/internal/provider"
	"github.com/yawaflua/marusya-gemini/internal/provider/registry"
)

// EventProvider is published when the active provider changes.
const EventProvider EventKind = "provider"

func (s *Server) registerProviderAPI() {
	s.mux.HandleFunc("GET /panel/api/providers", s.panelAPI(s.handleProviders))
	s.mux.HandleFunc("PUT /panel/api/providers/{id}", s.panelAPI(s.handlePutProvider))
	s.mux.HandleFunc("DELETE /panel/api/providers/{id}", s.panelAPI(s.handleDeleteProvider))
	s.mux.HandleFunc("POST /panel/api/providers/{id}/activate", s.panelAPI(s.handleActivateProvider))
}

// providersState is what the providers tab of the panel shows.
type providersState struct {
	Enabled   bool                `json:"enabled"`
	Saved     bool                `json:"saved"` // kept across restarts
	Kinds     []registry.KindInfo `json:"kinds"`
	Providers []registry.View     `json:"providers"`
	Active    string              `json:"active"`
}

func (s *Server) providersState() providersState {
	reg := s.cfg.Providers
	if reg == nil {
		return providersState{}
	}
	views, active := reg.List()
	return providersState{
		Enabled: true, Saved: s.cfg.ProvidersFile != "",
		Kinds: registry.Kinds(), Providers: views, Active: active,
	}
}

var errNoRegistry = errors.New("провайдеры настраиваются только в конфигурации сервера")

func (s *Server) handleProviders(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, s.providersState())
}

func (s *Server) handlePutProvider(w http.ResponseWriter, r *http.Request) {
	if s.cfg.Providers == nil {
		writeError(w, http.StatusNotFound, errNoRegistry)
		return
	}
	var spec registry.Spec
	if !readJSON(w, r, &spec) {
		return
	}
	spec.ID = r.PathValue("id")
	active, err := s.cfg.Providers.Put(r.Context(), spec)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	s.cfg.Logger.Info("provider saved", "id", spec.ID, "kind", spec.Kind, "base_url", spec.BaseURL, "model", spec.Model)
	if active {
		s.providerChanged()
	}
	writeJSON(w, http.StatusOK, s.providersState())
}

func (s *Server) handleDeleteProvider(w http.ResponseWriter, r *http.Request) {
	if s.cfg.Providers == nil {
		writeError(w, http.StatusNotFound, errNoRegistry)
		return
	}
	if err := s.cfg.Providers.Delete(r.PathValue("id")); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	writeJSON(w, http.StatusOK, s.providersState())
}

func (s *Server) handleActivateProvider(w http.ResponseWriter, r *http.Request) {
	if s.cfg.Providers == nil {
		writeError(w, http.StatusNotFound, errNoRegistry)
		return
	}
	if err := s.cfg.Providers.Activate(r.Context(), r.PathValue("id")); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	s.providerChanged()
	writeJSON(w, http.StatusOK, s.providersState())
}

// providerChanged follows a switch to another provider: the model and voice
// in the settings become those of the new provider, and the speakers move
// to it (a speaker that must learn a new audio format reconnects).
func (s *Server) providerChanged() {
	var t provider.Tuning
	if tn, ok := provider.AsTuner(s.cfg.Provider); ok {
		t = tn.Tuning()
	}
	s.smu.Lock()
	s.defaults.Model, s.defaults.Voice = t.Model, t.Voice
	s.settings.Model, s.settings.Voice = t.Model, t.Voice
	set, def := s.settings, s.defaults
	s.smu.Unlock()
	if s.cfg.SettingsFile != "" {
		if err := saveOverrides(s.cfg.SettingsFile, set.overrides(def)); err != nil {
			s.cfg.Logger.Warn("save settings", "err", err)
		}
	}
	s.cfg.Logger.Info("provider changed", "provider", s.cfg.Provider.Name())
	s.events.publish(Event{Kind: EventProvider, Name: s.cfg.Provider.Name(), Source: SourcePanel})
	for _, d := range s.sessions() {
		d.providerChanged()
	}
	s.events.devicesChanged()
}
