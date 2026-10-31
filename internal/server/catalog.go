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
	"crypto/sha512"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/yawaflua/marusya-gemini/internal/plugin"
)

// The plugin catalog (registry/ in this repository) publishes plugins as
// ZIP archives with a plugin.json. The panel searches it and installs
// plugins into Config.PluginsDir. Downloads need no account; the catalog
// sends the SHA-512 of the archive in X-Checksum-Sha512 and the archive is
// installed only if it matches.

const (
	catalogTimeout = 2 * time.Minute
	// checksumHeader carries the hex SHA-512 of a downloaded archive.
	checksumHeader = "X-Checksum-Sha512"
)

var sha512Re = regexp.MustCompile(`^[0-9a-f]{128}$`)

// catalogProject is a search result as the panel shows it.
type catalogProject struct {
	ID            string `json:"id"`
	Slug          string `json:"slug"`
	Name          string `json:"name"`
	Description   string `json:"description"`
	LatestVersion string `json:"latest_version"`
	// Environment is where the latest version runs: "server", "agent" or
	// "both"; empty if unknown.
	Environment string `json:"environment,omitempty"`
	URL         string `json:"url"` // the project page on the catalog site
	// Installed is the version in the plugins directory, if any.
	Installed string `json:"installed,omitempty"`
	// Speakers maps the speakers that run the plugin to its version there.
	Speakers map[string]string `json:"speakers,omitempty"`
}

var (
	errNoCatalog    = errors.New("каталог плагинов выключен: задайте адрес MARUSYA_PLUGIN_REGISTRY")
	errAgentOnly    = errors.New("плагин работает только на колонке: поставьте его на колонку кнопкой рядом с ней")
	errBadChecksum  = errors.New("контрольная сумма SHA-512 архива не совпала, установка отменена")
	errNoChecksum   = errors.New("каталог не прислал SHA-512 архива, установка отменена")
	errArchiveLarge = errors.New("архив плагина больше 64 МБ")
)

func (s *Server) registerCatalogAPI() {
	s.mux.HandleFunc("GET /panel/api/catalog/search", s.panelAPI(s.handleCatalogSearch))
	s.mux.HandleFunc("POST /panel/api/catalog/install", s.panelAPI(s.handleCatalogInstall))
	s.mux.HandleFunc("DELETE /panel/api/catalog/{id}", s.panelAPI(s.handleCatalogRemove))
}

func (s *Server) catalogClient() *http.Client {
	if s.cfg.HTTPClient != nil {
		return s.cfg.HTTPClient
	}
	return http.DefaultClient
}

// catalogURL joins the catalog address and an API path with a query.
func (s *Server) catalogURL(p string, q url.Values) string {
	u := strings.TrimRight(s.cfg.PluginRegistry, "/") + p
	if len(q) > 0 {
		u += "?" + q.Encode()
	}
	return u
}

// catalogGet fetches JSON from the catalog API.
func (s *Server) catalogGet(ctx context.Context, p string, q url.Values, v any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, s.catalogURL(p, q), nil)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/json")
	resp, err := s.catalogClient().Do(req)
	if err != nil {
		return fmt.Errorf("каталог плагинов недоступен: %w", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return fmt.Errorf("каталог плагинов: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return catalogError(resp.StatusCode, body)
	}
	return json.Unmarshal(body, v)
}

// catalogError turns an error reply of the catalog (a gRPC status in JSON)
// into an error.
func catalogError(code int, body []byte) error {
	var st struct {
		Message string `json:"message"`
	}
	if json.Unmarshal(body, &st) == nil && st.Message != "" {
		return fmt.Errorf("каталог плагинов: %s", st.Message)
	}
	return fmt.Errorf("каталог плагинов ответил %d", code)
}

// installedPlugins returns the installed versions of the plugins, by id,
// and which of them came from the catalog.
func (s *Server) installedPlugins() (versions map[string]string, marks map[string]plugin.CatalogInstall) {
	versions, marks = map[string]string{}, map[string]plugin.CatalogInstall{}
	if s.cfg.PluginsDir == "" {
		return
	}
	ms, _ := plugin.Discover(s.cfg.PluginsDir)
	for _, m := range ms {
		versions[m.ID] = m.Version
		if mark, ok := plugin.ReadCatalogMark(m.Dir); ok {
			marks[m.ID] = mark
			if versions[m.ID] == "" {
				versions[m.ID] = mark.Version
			}
		}
	}
	return
}

func (s *Server) handleCatalogSearch(w http.ResponseWriter, r *http.Request) {
	if s.cfg.PluginRegistry == "" {
		writeError(w, http.StatusConflict, errNoCatalog)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
	defer cancel()
	query := strings.TrimSpace(r.URL.Query().Get("q"))
	if len(query) > 200 {
		query = query[:200]
	}
	var res struct {
		Projects []struct {
			Project struct {
				ID            string `json:"id"`
				Slug          string `json:"slug"`
				Name          string `json:"name"`
				Description   string `json:"description"`
				LatestVersion string `json:"latest_version"`
			} `json:"project"`
		} `json:"projects"`
	}
	q := url.Values{"page_size": {"20"}}
	if query != "" {
		q.Set("query", query)
	}
	if err := s.catalogGet(ctx, "/v1/projects:search", q, &res); err != nil {
		writeError(w, http.StatusBadGateway, err)
		return
	}
	versions, _ := s.installedPlugins()
	speakers := s.speakerPlugins()
	out := make([]catalogProject, len(res.Projects))
	var wg sync.WaitGroup
	for i, p := range res.Projects {
		out[i] = catalogProject{
			ID: p.Project.ID, Slug: p.Project.Slug, Name: p.Project.Name,
			Description: p.Project.Description, LatestVersion: p.Project.LatestVersion,
			URL:       s.catalogURL("/projects/"+url.PathEscape(p.Project.ID), nil),
			Installed: versions[p.Project.Slug],
		}
		if sp, ok := speakers[p.Project.Slug]; ok {
			out[i].Speakers = map[string]string{}
			for _, d := range sp.Devices {
				out[i].Speakers[d] = sp.Version
			}
		}
		if p.Project.LatestVersion == "" {
			continue
		}
		wg.Add(1)
		go func(cp *catalogProject) {
			defer wg.Done()
			if v, err := s.catalogLatest(ctx, cp.ID); err == nil {
				cp.Environment = v.Metadata.environment()
			}
		}(&out[i])
	}
	wg.Wait()
	writeJSON(w, http.StatusOK, map[string]any{"catalog": s.cfg.PluginRegistry, "projects": out})
}

// catalogVersion is a version as the catalog API returns it.
type catalogVersion struct {
	ID       string             `json:"id"`
	Version  string             `json:"version"`
	SHA512   string             `json:"sha512"`
	Metadata catalogVersionMeta `json:"metadata"`
}

type catalogVersionMeta struct {
	Slug        string `json:"slug"`
	Environment string `json:"environment"`
}

func (m catalogVersionMeta) environment() string {
	switch m.Environment {
	case "PLUGIN_ENVIRONMENT_SERVER":
		return "server"
	case "PLUGIN_ENVIRONMENT_AGENT":
		return "agent"
	case "PLUGIN_ENVIRONMENT_BOTH":
		return "both"
	}
	return ""
}

// catalogLatest returns the newest version of a project.
func (s *Server) catalogLatest(ctx context.Context, projectID string) (catalogVersion, error) {
	var res struct {
		Versions []catalogVersion `json:"versions"`
	}
	err := s.catalogGet(ctx, "/v1/projects/"+url.PathEscape(projectID)+"/versions", url.Values{"page_size": {"1"}}, &res)
	if err != nil {
		return catalogVersion{}, err
	}
	if len(res.Versions) == 0 {
		return catalogVersion{}, errors.New("у плагина в каталоге нет версий")
	}
	return res.Versions[0], nil
}

func (s *Server) handleCatalogInstall(w http.ResponseWriter, r *http.Request) {
	switch {
	case s.cfg.PluginRegistry == "":
		writeError(w, http.StatusConflict, errNoCatalog)
		return
	case s.plugins == nil:
		writeError(w, http.StatusConflict, errNoPlugins)
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
	id, err := s.installFromCatalog(ctx, req.ProjectID)
	if err != nil {
		s.cfg.Logger.Warn("plugin install", "project", req.ProjectID, "err", err)
		writeError(w, http.StatusBadGateway, err)
		return
	}
	s.cfg.Logger.Info("plugin installed from the catalog", "plugin", id)
	writeJSON(w, http.StatusOK, s.pluginsState())
}

// installFromCatalog downloads the latest version of a project, checks its
// SHA-512 and unpacks it into the plugins directory, replacing an older
// version. It returns the plugin id.
func (s *Server) installFromCatalog(ctx context.Context, projectID string) (string, error) {
	latest, err := s.catalogLatest(ctx, projectID)
	if err != nil {
		return "", err
	}
	if latest.Metadata.environment() == "agent" {
		return "", errAgentOnly
	}
	archive, sum, err := s.catalogDownload(ctx, projectID, latest.ID)
	if err != nil {
		return "", err
	}
	// The version list and the download must describe the same archive.
	if latest.SHA512 != "" && !strings.EqualFold(latest.SHA512, sum) {
		return "", errBadChecksum
	}

	s.catmu.Lock()
	defer s.catmu.Unlock()
	mark := plugin.CatalogInstall{ProjectID: projectID, Version: latest.Version, SHA512: sum, Catalog: s.cfg.PluginRegistry}
	man, err := plugin.InstallArchive(s.cfg.PluginsDir, archive, mark, func(m plugin.Manifest) error {
		if latest.Metadata.Slug != "" && m.ID != latest.Metadata.Slug {
			return fmt.Errorf("id плагина в архиве %q не совпадает с каталогом (%q)", m.ID, latest.Metadata.Slug)
		}
		if m.Server == nil {
			return errAgentOnly
		}
		return nil
	}, s.plugins.Rescan)
	if err != nil {
		return "", err
	}
	return man.ID, nil
}

// catalogDownload fetches an archive and checks it against the SHA-512 the
// catalog sends with it. It returns the archive and its hex SHA-512.
func (s *Server) catalogDownload(ctx context.Context, projectID, versionID string) ([]byte, string, error) {
	u := s.catalogURL("/v1/projects/id/"+url.PathEscape(projectID)+"/versions/"+url.PathEscape(versionID)+":download", nil)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, "", err
	}
	req.Header.Set("Accept", "application/zip")
	resp, err := s.catalogClient().Do(req)
	if err != nil {
		return nil, "", fmt.Errorf("каталог плагинов недоступен: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
		return nil, "", catalogError(resp.StatusCode, body)
	}
	want := strings.ToLower(strings.TrimSpace(resp.Header.Get(checksumHeader)))
	if !sha512Re.MatchString(want) {
		return nil, "", errNoChecksum
	}
	h := sha512.New()
	archive, err := io.ReadAll(io.TeeReader(io.LimitReader(resp.Body, plugin.MaxArchive+1), h))
	if err != nil {
		return nil, "", fmt.Errorf("загрузка плагина: %w", err)
	}
	if len(archive) > plugin.MaxArchive {
		return nil, "", errArchiveLarge
	}
	got := hex.EncodeToString(h.Sum(nil))
	if got != want {
		return nil, "", errBadChecksum
	}
	return archive, got, nil
}

func (s *Server) handleCatalogRemove(w http.ResponseWriter, r *http.Request) {
	if s.plugins == nil {
		writeError(w, http.StatusConflict, errNoPlugins)
		return
	}
	id := r.PathValue("id")
	s.catmu.Lock()
	err := plugin.RemoveCatalog(s.cfg.PluginsDir, id, s.plugins.Rescan)
	s.catmu.Unlock()
	switch {
	case errors.Is(err, os.ErrNotExist):
		writeError(w, http.StatusNotFound, errors.New("нет такого плагина"))
		return
	case errors.Is(err, plugin.ErrNotCatalog):
		writeError(w, http.StatusConflict, err)
		return
	case err != nil:
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	s.cfg.Logger.Info("plugin removed", "plugin", id)
	writeJSON(w, http.StatusOK, s.pluginsState())
}
