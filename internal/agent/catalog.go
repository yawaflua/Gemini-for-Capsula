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
	"crypto/sha512"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/yawaflua/marusya-gemini/internal/models"
	"github.com/yawaflua/marusya-gemini/internal/plugin"
)

// The server tells the agent which plugins from the plugin catalog it
// should run (models.TypeCatalogPlugins). The agent downloads the missing
// and changed ones from the server, checks their SHA-512, unpacks them into
// Config.PluginsDir and removes the catalog plugins that are not in the
// list. Plugins put there by hand (without plugin.CatalogMark) are never
// touched.

var sha512Re = regexp.MustCompile(`^[0-9a-f]{128}$`)

// rescanner is implemented by plugins that can reread their directory.
type rescanner interface{ Rescan() }

// catalogEnabled reports whether the agent installs catalog plugins.
func (a *Agent) catalogEnabled() bool {
	if a.cfg.PluginsDir == "" || a.plugins == nil {
		return false
	}
	_, ok := a.plugins.(rescanner)
	return ok
}

// onCatalogPlugins syncs the plugins directory with the server's list in
// the background. Only the newest list is applied. A sync outlives the
// connection: an installed plugin with new functions makes the agent
// reconnect to announce them.
func (a *Agent) onCatalogPlugins(ctx context.Context, m *models.CatalogPlugins) {
	if m == nil || !a.catalogEnabled() {
		return
	}
	ctx = context.WithoutCancel(ctx)
	gen := a.catGen.Add(1)
	go func() {
		a.catMu.Lock()
		defer a.catMu.Unlock()
		if a.catGen.Load() != gen {
			return
		}
		a.syncCatalog(ctx, m.Plugins)
	}()
}

func (a *Agent) rescanPlugins() {
	if r, ok := a.plugins.(rescanner); ok {
		r.Rescan()
	}
}

// syncCatalog installs, updates and removes catalog plugins to match list.
func (a *Agent) syncCatalog(ctx context.Context, list []models.CatalogPlugin) {
	root := a.cfg.PluginsDir
	want := map[string]bool{}
	for _, p := range list {
		if !validPluginID(p.ID) || !sha512Re.MatchString(p.SHA512) {
			a.log.Warn("catalog plugin: bad entry", "plugin", p.ID)
			continue
		}
		want[p.ID] = true
		dir := filepath.Join(root, p.ID)
		mark, marked := plugin.ReadCatalogMark(dir)
		if marked && mark.SHA512 == p.SHA512 {
			continue
		}
		if _, err := os.Lstat(dir); err == nil && !marked {
			a.log.Warn("catalog plugin: a plugin put by hand has the same id, skipped", "plugin", p.ID, "dir", dir)
			continue
		}
		if err := a.installCatalogPlugin(ctx, p); err != nil {
			a.log.Warn("catalog plugin: install failed", "plugin", p.ID, "version", p.Version, "err", err)
			continue
		}
		a.log.Info("catalog plugin installed", "plugin", p.ID, "version", p.Version)
	}

	entries, _ := os.ReadDir(root)
	for _, e := range entries {
		name := e.Name()
		if strings.HasPrefix(name, ".") || want[name] {
			continue
		}
		if _, ok := plugin.ReadCatalogMark(filepath.Join(root, name)); !ok {
			continue
		}
		if err := plugin.RemoveCatalog(root, name, a.rescanPlugins); err != nil {
			a.log.Warn("catalog plugin: remove failed", "plugin", name, "err", err)
			continue
		}
		a.log.Info("catalog plugin removed", "plugin", name)
	}
}

func validPluginID(id string) bool {
	return id != "" && id == filepath.Base(id) && !strings.HasPrefix(id, ".") && !strings.ContainsAny(id, `/\`)
}

// installCatalogPlugin downloads a plugin from the server and installs it.
func (a *Agent) installCatalogPlugin(ctx context.Context, p models.CatalogPlugin) error {
	archive, err := a.downloadCatalogPlugin(ctx, p)
	if err != nil {
		return err
	}
	mark := plugin.CatalogInstall{ProjectID: p.ProjectID, Version: p.Version, SHA512: p.SHA512, Catalog: p.Catalog}
	_, err = plugin.InstallArchive(a.cfg.PluginsDir, archive, mark, func(m plugin.Manifest) error {
		if m.ID != p.ID {
			return fmt.Errorf("id плагина в архиве %q, ожидался %q", m.ID, p.ID)
		}
		if m.Agent == nil {
			return errors.New("у плагина нет половины для колонки")
		}
		return nil
	}, a.rescanPlugins)
	return err
}

// downloadCatalogPlugin fetches the archive and checks its SHA-512
// against the one in the list.
func (a *Agent) downloadCatalogPlugin(ctx context.Context, p models.CatalogPlugin) ([]byte, error) {
	u, err := serverURL(a.cfg.ServerURL, p.Path)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	if a.cfg.Token != "" {
		req.Header.Set("Authorization", "Bearer "+a.cfg.Token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%s: %s", u, resp.Status)
	}
	h := sha512.New()
	archive, err := io.ReadAll(io.TeeReader(io.LimitReader(resp.Body, plugin.MaxArchive+1), h))
	if err != nil {
		return nil, err
	}
	if len(archive) > plugin.MaxArchive {
		return nil, errors.New("архив плагина больше 64 МБ")
	}
	if got := hex.EncodeToString(h.Sum(nil)); got != p.SHA512 {
		return nil, fmt.Errorf("контрольная сумма SHA-512 архива не совпала: %s", got)
	}
	return archive, nil
}

// serverURL resolves a path on the server for its stream URL
// (ws://host:8080/v1/stream + /v1/plugins/x -> http://host:8080/v1/plugins/x).
func serverURL(streamURL, path string) (string, error) {
	if !strings.HasPrefix(path, "/") || strings.Contains(path, "..") {
		return "", fmt.Errorf("bad path %q", path)
	}
	u, err := url.Parse(streamURL)
	if err != nil {
		return "", err
	}
	switch u.Scheme {
	case "ws":
		u.Scheme = "http"
	case "wss":
		u.Scheme = "https"
	case "http", "https":
	default:
		return "", fmt.Errorf("unsupported scheme %q", u.Scheme)
	}
	u.Path = strings.TrimSuffix(strings.TrimSuffix(u.Path, "/"), "/v1/stream") + path
	u.RawPath, u.RawQuery, u.Fragment = "", "", ""
	return u.String(), nil
}
