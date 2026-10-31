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

package agent_test

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/sha512"
	"encoding/hex"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/yawaflua/marusya-gemini/internal/agent"
	"github.com/yawaflua/marusya-gemini/internal/controller"
	"github.com/yawaflua/marusya-gemini/internal/models"
	"github.com/yawaflua/marusya-gemini/internal/plugin"
	"github.com/yawaflua/marusya-gemini/internal/transport"
)

// rescanPlugins are plugins that count rescans of their directory.
type rescanPlugins struct {
	fakePlugins
	rescans atomic.Int32
}

func (p *rescanPlugins) Rescan() { p.rescans.Add(1) }

func zipArchive(t *testing.T, files map[string]string) []byte {
	t.Helper()
	var buf bytes.Buffer
	w := zip.NewWriter(&buf)
	for name, body := range files {
		f, err := w.Create(name)
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

func sum512(b []byte) string {
	s := sha512.Sum512(b)
	return hex.EncodeToString(s[:])
}

func writePlugin(t *testing.T, dir, manifest string, mark *plugin.CatalogInstall) {
	t.Helper()
	os.MkdirAll(dir, 0o755)
	os.WriteFile(filepath.Join(dir, plugin.ManifestFile), []byte(manifest), 0o644)
	if mark != nil {
		b, _ := json.Marshal(mark)
		os.WriteFile(filepath.Join(dir, plugin.CatalogMark), b, 0o644)
	}
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	for deadline := time.Now().Add(5 * time.Second); !cond(); time.Sleep(10 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
	}
}

func TestAgentCatalogPlugins(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	root := t.TempDir()
	dir := filepath.Join(root, "plugins")

	hello := zipArchive(t, map[string]string{
		"hello/plugin.json": `{"id":"hello","version":"1.0.0","agent":{"command":["./run"]}}`,
		"hello/run":         "#!/bin/sh\n",
	})
	slip := zipArchive(t, map[string]string{"plugin.json": `{"id":"slip","agent":{"command":["x"]}}`, "../evil": "x"})
	serverOnly := zipArchive(t, map[string]string{"plugin.json": `{"id":"srv","server":{"command":["x"]}}`})
	archives := map[string][]byte{"hello": hello, "slip": slip, "srv": serverOnly, "tampered": hello}
	var downloads atomic.Int32

	// A plugin put by hand, one with the id of a catalog plugin, and a
	// catalog plugin the server no longer lists.
	writePlugin(t, filepath.Join(dir, "manual"), `{"id":"manual","agent":{"command":["x"]}}`, nil)
	writePlugin(t, filepath.Join(dir, "clash"), `{"id":"clash","agent":{"command":["x"]}}`, nil)
	writePlugin(t, filepath.Join(dir, "stale"), `{"id":"stale","agent":{"command":["x"]}}`, &plugin.CatalogInstall{Slug: "stale", SHA512: strings.Repeat("0", 128)})

	list := func(ids ...string) *models.CatalogPlugins {
		out := &models.CatalogPlugins{Plugins: []models.CatalogPlugin{}}
		for _, id := range ids {
			sum := sum512(archives[id])
			if id == "tampered" {
				sum = strings.Repeat("ab", 64)
			}
			out.Plugins = append(out.Plugins, models.CatalogPlugin{ID: id, Version: "1.0.0", SHA512: sum, Path: "/v1/plugins/" + id, ProjectID: "p-" + id})
		}
		return out
	}
	hellos := make(chan *models.Hello, 4)
	lists := make(chan *models.CatalogPlugins, 4)
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/plugins/{id}", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer tok" {
			http.Error(w, "unauthorized", 401)
			return
		}
		downloads.Add(1)
		w.Write(archives[r.PathValue("id")])
	})
	mux.HandleFunc(transport.Path, func(w http.ResponseWriter, r *http.Request) {
		conn, err := transport.Accept(w, r)
		if err != nil {
			return
		}
		defer conn.Close("bye")
		f, err := conn.Read(ctx)
		if err != nil || f.Message == nil || f.Message.Type != models.TypeHello {
			return
		}
		hellos <- f.Message.Hello
		conn.Send(ctx, &models.Message{Type: models.TypeReady, Ready: &models.Ready{SessionID: "s", OutputFormat: models.DefaultOutputFormat}})
		for {
			select {
			case l := <-lists:
				conn.Send(ctx, &models.Message{Type: models.TypeCatalogPlugins, CatalogPlugins: l})
			case <-r.Context().Done():
				return
			case <-ctx.Done():
				return
			}
		}
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	plugins := &rescanPlugins{fakePlugins: fakePlugins{delivered: make(chan string, 1)}}
	a := agent.New(agent.Config{DeviceID: "test", Token: "tok", PluginsDir: dir,
		ServerURL: "ws" + strings.TrimPrefix(srv.URL, "http") + transport.Path},
		slog.New(slog.DiscardHandler),
		agent.Deps{Capture: &fakeMic{}, Player: &fakePlayer{done: make(chan struct{})}, Detector: make(fakeWake),
			Dispatcher: &controller.Dispatcher{}, Plugins: plugins})
	stopped := make(chan struct{})
	go func() {
		a.Run(ctx)
		close(stopped)
	}()
	defer func() {
		cancel()
		<-stopped
	}()

	if h := <-hellos; !slices.Contains(h.Features, models.FeatureCatalogPlugins) {
		t.Fatalf("features = %v, want catalog_plugins", h.Features)
	}
	lists <- list("hello", "clash", "slip", "srv", "tampered")
	helloDir := filepath.Join(dir, "hello")
	waitFor(t, "stale to be removed", func() bool {
		_, err := os.Stat(filepath.Join(dir, "stale"))
		return os.IsNotExist(err)
	})
	mark, ok := plugin.ReadCatalogMark(helloDir)
	if !ok || mark.SHA512 != sum512(hello) || mark.Slug != "hello" || mark.ProjectID != "p-hello" {
		t.Fatalf("mark of hello = %+v %v", mark, ok)
	}
	if st, err := os.Stat(filepath.Join(helloDir, "run")); err != nil || st.Mode().IsDir() {
		t.Fatalf("hello/run = %v %v", st, err)
	}
	for _, id := range []string{"slip", "srv", "tampered"} {
		if _, err := os.Stat(filepath.Join(dir, id)); !os.IsNotExist(err) {
			t.Errorf("%s was installed: %v", id, err)
		}
	}
	if _, err := os.Stat(filepath.Join(root, "evil")); err == nil {
		t.Error("a file was written outside the plugins dir")
	}
	for _, id := range []string{"manual", "clash"} {
		if _, ok := plugin.ReadCatalogMark(filepath.Join(dir, id)); ok {
			t.Errorf("%s was replaced", id)
		}
		if _, err := os.Stat(filepath.Join(dir, id, plugin.ManifestFile)); err != nil {
			t.Errorf("%s was touched: %v", id, err)
		}
	}
	if plugins.rescans.Load() == 0 {
		t.Error("the plugins were not rescanned")
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 3 {
		t.Errorf("plugins dir = %v, want clash, hello, manual", entries)
	}

	// The same list again downloads nothing.
	n := downloads.Load()
	lists <- list("hello")
	lists <- list()
	waitFor(t, "hello to be removed", func() bool {
		_, err := os.Stat(helloDir)
		return os.IsNotExist(err)
	})
	if got := downloads.Load(); got != n {
		t.Errorf("downloads = %d after an unchanged list, want %d", got, n)
	}
	if _, err := os.Stat(filepath.Join(dir, "manual", plugin.ManifestFile)); err != nil {
		t.Errorf("the empty list removed a plugin put by hand: %v", err)
	}
}

func TestAgentCatalogPluginsNeedDir(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	hellos := make(chan *models.Hello, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := transport.Accept(w, r)
		if err != nil {
			return
		}
		defer conn.Close("bye")
		if f, err := conn.Read(ctx); err == nil && f.Message != nil {
			hellos <- f.Message.Hello
		}
		<-r.Context().Done()
	}))
	defer srv.Close()
	a := agent.New(agent.Config{DeviceID: "test", ServerURL: "ws" + strings.TrimPrefix(srv.URL, "http") + transport.Path},
		slog.New(slog.DiscardHandler),
		agent.Deps{Capture: &fakeMic{}, Player: &fakePlayer{done: make(chan struct{})}, Detector: make(fakeWake),
			Dispatcher: &controller.Dispatcher{}, Plugins: &rescanPlugins{}})
	go a.Run(ctx)
	select {
	case h := <-hellos:
		if slices.Contains(h.Features, models.FeatureCatalogPlugins) || !slices.Contains(h.Features, models.FeaturePlugins) {
			t.Errorf("features = %v, want plugins without catalog_plugins", h.Features)
		}
	case <-ctx.Done():
		t.Fatal("timed out")
	}
}
