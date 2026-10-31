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
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/yawaflua/marusya-gemini/internal/models"
	"github.com/yawaflua/marusya-gemini/internal/plugin"
)

// Plugins from the catalog for the speakers (the "agent" half). The server
// downloads the archive, checks its SHA-512 and keeps it with the list of
// speakers that should run it in <PluginsDir>/.speakers/<id>/. Speakers
// announcing models.FeatureCatalogPlugins get the list of their plugins on
// connect and on every change (models.TypeCatalogPlugins), download the
// archives from GET /v1/plugins/{id} and install them themselves.

const (
	speakersDir     = ".speakers"
	speakerArchive  = "plugin.zip"
	speakerManifest = "speaker.json"
)

// pluginIDRe matches the plugin ids the server serves to speakers.
var pluginIDRe = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,127}$`)

// speakerPlugin is what speaker.json records.
type speakerPlugin struct {
	plugin.CatalogInstall
	// Devices are the speakers that should run the plugin.
	Devices []string `json:"devices"`
}

var (
	errServerOnly    = errors.New("у плагина нет половины для колонки")
	errNoCatalogLink = errors.New("агент на колонке не умеет ставить плагины из каталога: обновите его")
)

func (s *Server) registerSpeakerCatalogAPI() {
	s.mux.HandleFunc("POST /panel/api/catalog/speakers/{device}", s.panelAPI(s.handleSpeakerInstall))
	s.mux.HandleFunc("DELETE /panel/api/catalog/speakers/{device}/{id}", s.panelAPI(s.handleSpeakerRemove))
}

func (s *Server) speakerRoot() string { return filepath.Join(s.cfg.PluginsDir, speakersDir) }

// speakerPlugins returns the catalog plugins for speakers, by id.
func (s *Server) speakerPlugins() map[string]speakerPlugin {
	out := map[string]speakerPlugin{}
	if s.cfg.PluginsDir == "" {
		return out
	}
	entries, _ := os.ReadDir(s.speakerRoot())
	for _, e := range entries {
		if !e.IsDir() || !pluginIDRe.MatchString(e.Name()) {
			continue
		}
		if sp, ok := s.readSpeakerPlugin(e.Name()); ok {
			out[e.Name()] = sp
		}
	}
	return out
}

func (s *Server) readSpeakerPlugin(id string) (speakerPlugin, bool) {
	var sp speakerPlugin
	b, err := os.ReadFile(filepath.Join(s.speakerRoot(), id, speakerManifest))
	if err != nil || json.Unmarshal(b, &sp) != nil || !sha512Re.MatchString(sp.SHA512) {
		return sp, false
	}
	return sp, true
}

func (s *Server) writeSpeakerPlugin(id string, sp speakerPlugin) error {
	b, _ := json.MarshalIndent(sp, "", "  ")
	return writeFileAtomic(filepath.Join(s.speakerRoot(), id, speakerManifest), b)
}

// writeFileAtomic replaces a file with data.
func writeFileAtomic(path string, data []byte) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Chmod(tmp.Name(), 0o644); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}

// catalogPluginsFor returns the plugins a speaker should run.
func (s *Server) catalogPluginsFor(device string) []models.CatalogPlugin {
	out := []models.CatalogPlugin{}
	for id, sp := range s.speakerPlugins() {
		if !slices.Contains(sp.Devices, device) {
			continue
		}
		out = append(out, models.CatalogPlugin{
			ID: id, Version: sp.Version, SHA512: sp.SHA512, Path: "/v1/plugins/" + id,
			ProjectID: sp.ProjectID, Catalog: sp.Catalog,
		})
	}
	slices.SortFunc(out, func(a, b models.CatalogPlugin) int { return strings.Compare(a.ID, b.ID) })
	return out
}

// pushCatalogPlugins sends the speaker the list of its catalog plugins.
// Without a plugins directory the server sends nothing, so a speaker keeps
// what it has.
func (d *deviceSession) pushCatalogPlugins() {
	if d.srv.cfg.PluginsDir == "" || !d.supportsFeature(models.FeatureCatalogPlugins) {
		return
	}
	list := d.srv.catalogPluginsFor(d.hello.DeviceID)
	msg := &models.Message{Type: models.TypeCatalogPlugins, CatalogPlugins: &models.CatalogPlugins{Plugins: list}}
	if err := d.send(msg); err != nil {
		d.log.Warn("send catalog plugins", "err", err)
		return
	}
	d.log.Info("catalog plugins sent", "plugins", len(list))
}

// pushCatalogPluginsTo sends the lists to the connected speakers in
// devices.
func (s *Server) pushCatalogPluginsTo(devices ...string) {
	for _, d := range s.sessions() {
		if slices.Contains(devices, d.hello.DeviceID) {
			go d.pushCatalogPlugins()
		}
	}
}

// speakerSession returns a connected speaker by device ID.
func (s *Server) speakerSession(device string) (*deviceSession, bool) {
	for _, d := range s.sessions() {
		if d.hello.DeviceID == device {
			return d, true
		}
	}
	return nil, false
}

func (s *Server) handleSpeakerInstall(w http.ResponseWriter, r *http.Request) {
	switch {
	case s.cfg.PluginRegistry == "":
		writeError(w, http.StatusConflict, errNoCatalog)
		return
	case s.cfg.PluginsDir == "":
		writeError(w, http.StatusConflict, errNoPlugins)
		return
	}
	device := r.PathValue("device")
	d, ok := s.speakerSession(device)
	switch {
	case !ok:
		writeError(w, http.StatusNotFound, errNoDevice)
		return
	case !d.supportsFeature(models.FeatureCatalogPlugins):
		writeError(w, http.StatusConflict, errNoCatalogLink)
		return
	}
	var req struct {
		ProjectID string `json:"project_id"`
	}
	if !readJSON(w, r, &req) {
		return
	}
	if req.ProjectID == "" || strings.ContainsAny(req.ProjectID, "/?#") {
		writeError(w, http.StatusBadRequest, errors.New("не указан плагин"))
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), catalogTimeout)
	defer cancel()
	id, err := s.installForSpeaker(ctx, req.ProjectID, device)
	if err != nil {
		s.cfg.Logger.Warn("speaker plugin install", "project", req.ProjectID, "device", device, "err", err)
		writeError(w, http.StatusBadGateway, err)
		return
	}
	s.cfg.Logger.Info("plugin from the catalog assigned to a speaker", "plugin", id, "device", device)
	writeJSON(w, http.StatusOK, s.pluginsState())
}

// installForSpeaker downloads the latest version of a project for the
// speakers and adds device to the speakers that run it. Speakers that
// already run an older version get the new one too.
func (s *Server) installForSpeaker(ctx context.Context, projectID, device string) (string, error) {
	latest, err := s.catalogLatest(ctx, projectID)
	if err != nil {
		return "", err
	}
	if latest.Metadata.environment() == "server" {
		return "", errServerOnly
	}
	archive, sum, err := s.catalogDownload(ctx, projectID, latest.ID)
	if err != nil {
		return "", err
	}
	if latest.SHA512 != "" && !strings.EqualFold(latest.SHA512, sum) {
		return "", errBadChecksum
	}
	man, err := checkSpeakerArchive(archive)
	if err != nil {
		return "", err
	}
	if latest.Metadata.Slug != "" && man.ID != latest.Metadata.Slug {
		return "", fmt.Errorf("id плагина в архиве %q не совпадает с каталогом (%q)", man.ID, latest.Metadata.Slug)
	}
	if !pluginIDRe.MatchString(man.ID) {
		return "", fmt.Errorf("недопустимый id плагина %q", man.ID)
	}

	s.catmu.Lock()
	defer s.catmu.Unlock()
	dir := filepath.Join(s.speakerRoot(), man.ID)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	sp, _ := s.readSpeakerPlugin(man.ID)
	if err := writeFileAtomic(filepath.Join(dir, speakerArchive), archive); err != nil {
		return "", err
	}
	sp.CatalogInstall = plugin.CatalogInstall{
		ProjectID: projectID, Slug: man.ID, Version: latest.Version, SHA512: sum,
		Catalog: s.cfg.PluginRegistry, InstalledAt: time.Now().UTC(),
	}
	if !slices.Contains(sp.Devices, device) {
		sp.Devices = append(sp.Devices, device)
	}
	slices.Sort(sp.Devices)
	if err := s.writeSpeakerPlugin(man.ID, sp); err != nil {
		return "", err
	}
	s.pushCatalogPluginsTo(sp.Devices...)
	return man.ID, nil
}

// checkSpeakerArchive unpacks an archive into a temporary directory to
// check it the way the speaker will and returns its manifest.
func checkSpeakerArchive(archive []byte) (plugin.Manifest, error) {
	tmp, err := os.MkdirTemp("", "marusya-plugin-")
	if err != nil {
		return plugin.Manifest{}, err
	}
	defer os.RemoveAll(tmp)
	if err := plugin.Unpack(archive, tmp); err != nil {
		return plugin.Manifest{}, err
	}
	man, err := plugin.Load(tmp)
	if err != nil {
		return plugin.Manifest{}, fmt.Errorf("plugin.json в архиве: %w", err)
	}
	if man.Agent == nil {
		return plugin.Manifest{}, errServerOnly
	}
	return man, nil
}

func (s *Server) handleSpeakerRemove(w http.ResponseWriter, r *http.Request) {
	if s.cfg.PluginsDir == "" {
		writeError(w, http.StatusConflict, errNoPlugins)
		return
	}
	device, id := r.PathValue("device"), r.PathValue("id")
	if !pluginIDRe.MatchString(id) {
		writeError(w, http.StatusNotFound, errors.New("нет такого плагина"))
		return
	}
	s.catmu.Lock()
	sp, ok := s.readSpeakerPlugin(id)
	if !ok || !slices.Contains(sp.Devices, device) {
		s.catmu.Unlock()
		writeError(w, http.StatusNotFound, errors.New("этого плагина нет на колонке"))
		return
	}
	sp.Devices = slices.DeleteFunc(sp.Devices, func(d string) bool { return d == device })
	var err error
	if len(sp.Devices) == 0 {
		err = os.RemoveAll(filepath.Join(s.speakerRoot(), id))
	} else {
		err = s.writeSpeakerPlugin(id, sp)
	}
	s.catmu.Unlock()
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	s.pushCatalogPluginsTo(device)
	s.cfg.Logger.Info("plugin from the catalog removed from a speaker", "plugin", id, "device", device)
	writeJSON(w, http.StatusOK, s.pluginsState())
}

// handleSpeakerPlugin serves the archive of a catalog plugin to the
// speakers. A speaker with its own token gets only its plugins.
func (s *Server) handleSpeakerPlugin(w http.ResponseWriter, r *http.Request) {
	if !s.authorizedDevice(r) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	id := r.PathValue("id")
	if !pluginIDRe.MatchString(id) {
		http.NotFound(w, r)
		return
	}
	s.catmu.Lock()
	sp, ok := s.readSpeakerPlugin(id)
	var archive []byte
	var err error
	if ok {
		archive, err = os.ReadFile(filepath.Join(s.speakerRoot(), id, speakerArchive))
	}
	s.catmu.Unlock()
	if !ok || err != nil {
		http.NotFound(w, r)
		return
	}
	if !s.authorized(r) {
		if dev, _ := s.deviceForToken(bearerToken(r)); !slices.Contains(sp.Devices, dev) {
			http.NotFound(w, r)
			return
		}
	}
	w.Header().Set(checksumHeader, sp.SHA512)
	w.Header().Set("Content-Type", "application/zip")
	w.Header().Set("Content-Disposition", "attachment; filename="+url.PathEscape(id)+".zip")
	w.Write(archive)
}
