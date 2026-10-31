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
	"archive/zip"
	"bytes"
	"cmp"
	"context"
	"crypto/sha512"
	"encoding/hex"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// fakeCatalog serves one project of the plugin catalog API.
type fakeCatalog struct {
	version     string
	environment string
	archive     []byte
	checksum    string // sent in X-Checksum-Sha512; empty sends the real one
}

func (c *fakeCatalog) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	sum := sha512.Sum512(c.archive)
	real := hex.EncodeToString(sum[:])
	switch r.URL.Path {
	case "/v1/projects:search":
		json.NewEncoder(w).Encode(map[string]any{"projects": []any{map[string]any{"project": map[string]any{
			"id": "p1", "slug": "hello", "name": "Привет", "description": "d", "latest_version": c.version,
		}}}})
	case "/v1/projects/p1/versions":
		json.NewEncoder(w).Encode(map[string]any{"versions": []any{map[string]any{
			"id": "v1", "version": c.version, "sha512": real,
			"metadata": map[string]any{"slug": "hello", "environment": c.environment},
		}}})
	case "/v1/projects/id/p1/versions/v1:download":
		w.Header().Set("X-Checksum-Sha512", cmp.Or(c.checksum, real))
		w.Header().Set("Content-Type", "application/zip")
		w.Write(c.archive)
	default:
		http.NotFound(w, r)
	}
}

func zipOf(t *testing.T, files map[string]string) []byte {
	t.Helper()
	var buf bytes.Buffer
	w := zip.NewWriter(&buf)
	for name, body := range files {
		h := &zip.FileHeader{Name: name, Method: zip.Deflate}
		h.SetMode(0o644)
		if strings.HasSuffix(name, ".sh") {
			h.SetMode(0o755)
		}
		f, err := w.CreateHeader(h)
		if err != nil {
			t.Fatal(err)
		}
		f.Write([]byte(body))
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func helloManifest(version string) string {
	exe, _ := os.Executable()
	cmd, _ := json.Marshal([]string{exe})
	return `{"id":"hello","version":"` + version + `","server":{"command":` + string(cmd) + `,"env":{"MARUSYA_TEST_PLUGIN":"1"}}}`
}

func newCatalogEnv(t *testing.T, cat *fakeCatalog) (*panelEnv, string) {
	t.Helper()
	cs := httptest.NewServer(cat)
	t.Cleanup(cs.Close)
	root := t.TempDir()
	s := New(Config{
		Provider: &textProvider{texts: make(chan string, 1)}, PanelPassword: "pw", Language: "ru-RU",
		PluginsDir: root, PluginRegistry: cs.URL, Logger: slog.New(slog.DiscardHandler),
	})
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	s.StartPlugins(ctx)
	ts := httptest.NewServer(s)
	t.Cleanup(ts.Close)
	jar, _ := cookiejar.New(nil)
	e := &panelEnv{t: t, srv: s, ts: ts, client: &http.Client{Jar: jar, Timeout: 30 * time.Second}}
	e.login()
	return e, root
}

func TestCatalogInstallUpdateRemove(t *testing.T) {
	cat := &fakeCatalog{version: "1.0.0", environment: "PLUGIN_ENVIRONMENT_SERVER",
		archive: zipOf(t, map[string]string{"hello/plugin.json": helloManifest("1.0.0"), "hello/run.sh": "#!/bin/sh\n"})}
	e, root := newCatalogEnv(t, cat)

	code, out := e.do("GET", "/panel/api/catalog/search?q=hello", nil, true)
	if code != 200 {
		t.Fatalf("search = %d %v", code, out)
	}
	p := out["projects"].([]any)[0].(map[string]any)
	if p["environment"] != "server" || p["installed"] != nil {
		t.Fatalf("search result = %v", p)
	}

	if code, out = e.do("POST", "/panel/api/catalog/install", map[string]string{"project_id": "p1"}, true); code != 200 {
		t.Fatalf("install = %d %v", code, out)
	}
	if out["from_catalog"].(map[string]any)["hello"] == nil {
		t.Fatalf("from_catalog = %v", out["from_catalog"])
	}
	st, err := os.Stat(filepath.Join(root, "hello", "run.sh"))
	if err != nil || st.Mode().Perm()&0o100 == 0 {
		t.Fatalf("run.sh = %v %v, want an executable", st, err)
	}
	mark, ok := readCatalogMark(filepath.Join(root, "hello"))
	sum := sha512.Sum512(cat.archive)
	if !ok || mark.Version != "1.0.0" || mark.SHA512 != hex.EncodeToString(sum[:]) {
		t.Fatalf("mark = %+v %v", mark, ok)
	}
	if _, ok := e.srv.plugins.Lookup("greet"); !ok {
		t.Fatal("the installed plugin does not run")
	}

	// A new version replaces the old one.
	cat.version = "1.1.0"
	cat.archive = zipOf(t, map[string]string{"plugin.json": helloManifest("1.1.0")})
	if code, out = e.do("POST", "/panel/api/catalog/install", map[string]string{"project_id": "p1"}, true); code != 200 {
		t.Fatalf("update = %d %v", code, out)
	}
	if _, err := os.Stat(filepath.Join(root, "hello", "run.sh")); !os.IsNotExist(err) {
		t.Errorf("files of the old version stayed: %v", err)
	}
	code, out = e.do("GET", "/panel/api/catalog/search", nil, true)
	if p := out["projects"].([]any)[0].(map[string]any); code != 200 || p["installed"] != "1.1.0" {
		t.Fatalf("search after update = %d %v", code, out)
	}
	entries, _ := os.ReadDir(root)
	if len(entries) != 1 {
		t.Errorf("plugins dir = %v, want only hello", entries)
	}

	if code, out = e.do("DELETE", "/panel/api/catalog/hello", nil, true); code != 200 {
		t.Fatalf("remove = %d %v", code, out)
	}
	if _, err := os.Stat(filepath.Join(root, "hello")); !os.IsNotExist(err) {
		t.Errorf("plugin dir stayed: %v", err)
	}
	if _, ok := e.srv.plugins.Lookup("greet"); ok {
		t.Error("the removed plugin still runs")
	}
}

func TestCatalogInstallRefusesBadChecksum(t *testing.T) {
	cat := &fakeCatalog{version: "1.0.0", environment: "PLUGIN_ENVIRONMENT_SERVER",
		archive: zipOf(t, map[string]string{"plugin.json": helloManifest("1.0.0")}), checksum: strings.Repeat("ab", 64)}
	e, root := newCatalogEnv(t, cat)
	code, out := e.do("POST", "/panel/api/catalog/install", map[string]string{"project_id": "p1"}, true)
	if code != http.StatusBadGateway || !strings.Contains(out["error"].(string), "SHA-512") {
		t.Fatalf("install = %d %v, want a checksum error", code, out)
	}
	if entries, _ := os.ReadDir(root); len(entries) != 0 {
		t.Errorf("plugins dir = %v, want empty", entries)
	}

	cat.checksum = "nope"
	if code, out = e.do("POST", "/panel/api/catalog/install", map[string]string{"project_id": "p1"}, true); code != http.StatusBadGateway {
		t.Fatalf("install without a checksum = %d %v", code, out)
	}
}

func TestCatalogInstallRefuses(t *testing.T) {
	for name, cat := range map[string]*fakeCatalog{
		"agent only": {version: "1.0.0", environment: "PLUGIN_ENVIRONMENT_AGENT",
			archive: zipOf(t, map[string]string{"plugin.json": `{"id":"hello","agent":{"command":["x"]}}`})},
		"zip slip": {version: "1.0.0", environment: "PLUGIN_ENVIRONMENT_SERVER",
			archive: zipOf(t, map[string]string{"plugin.json": helloManifest("1.0.0"), "../evil": "x"})},
		"other id": {version: "1.0.0", environment: "PLUGIN_ENVIRONMENT_SERVER",
			archive: zipOf(t, map[string]string{"plugin.json": `{"id":"other","server":{"command":["x"]}}`})},
		"no manifest": {version: "1.0.0", environment: "PLUGIN_ENVIRONMENT_SERVER",
			archive: zipOf(t, map[string]string{"a/plugin.json": "{}", "b/x": "x"})},
	} {
		t.Run(name, func(t *testing.T) {
			e, root := newCatalogEnv(t, cat)
			if code, out := e.do("POST", "/panel/api/catalog/install", map[string]string{"project_id": "p1"}, true); code == 200 {
				t.Fatalf("install = %d %v, want an error", code, out)
			}
			if entries, _ := os.ReadDir(root); len(entries) != 0 {
				t.Errorf("plugins dir = %v, want empty", entries)
			}
			if _, err := os.Stat(filepath.Join(filepath.Dir(root), "evil")); err == nil {
				t.Error("a file was written outside the plugins dir")
			}
		})
	}
}

func TestCatalogRemoveKeepsManualPlugins(t *testing.T) {
	e, root := newCatalogEnv(t, &fakeCatalog{})
	os.MkdirAll(filepath.Join(root, "hello"), 0o755)
	os.WriteFile(filepath.Join(root, "hello", "plugin.json"), []byte(helloManifest("1.0.0")), 0o644)
	e.srv.plugins.Rescan()
	if code, out := e.do("DELETE", "/panel/api/catalog/hello", nil, true); code != http.StatusConflict {
		t.Fatalf("remove = %d %v, want 409", code, out)
	}
	if _, err := os.Stat(filepath.Join(root, "hello", "plugin.json")); err != nil {
		t.Fatal(err)
	}
}
