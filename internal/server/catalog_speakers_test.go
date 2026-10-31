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
	"bytes"
	"context"
	"crypto/sha512"
	"encoding/hex"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/yawaflua/marusya-gemini/internal/models"
	"github.com/yawaflua/marusya-gemini/internal/transport"
)

const agentManifest = `{"id":"hello","version":"1.0.0","agent":{"command":["./run.sh"]}}`

// dialCatalogSpeaker connects a fake speaker announcing features.
func dialCatalogSpeaker(t *testing.T, e *panelEnv, device string, features ...models.Feature) *transport.Conn {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	conn, err := transport.Dial(ctx, "ws"+strings.TrimPrefix(e.ts.URL, "http")+transport.Path, "")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close("done") })
	conn.Send(ctx, &models.Message{Type: models.TypeHello, Hello: &models.Hello{
		ProtocolVersion: models.ProtocolVersion, DeviceID: device,
		InputFormat: models.DeviceInputFormat, Features: features,
	}})
	if f, err := conn.Read(ctx); err != nil || f.Message.Type != models.TypeReady {
		t.Fatalf("ready: %+v %v", f, err)
	}
	return conn
}

// readCatalogPlugins waits for a list of catalog plugins matching ok.
func readCatalogPlugins(t *testing.T, conn *transport.Conn, ok func([]models.CatalogPlugin) bool) []models.CatalogPlugin {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	for {
		f, err := conn.Read(ctx)
		if err != nil {
			t.Fatalf("waiting for catalog plugins: %v", err)
		}
		if m := f.Message; m != nil && m.Type == models.TypeCatalogPlugins && ok(m.CatalogPlugins.Plugins) {
			return m.CatalogPlugins.Plugins
		}
	}
}

func sha512Hex(b []byte) string {
	sum := sha512.Sum512(b)
	return hex.EncodeToString(sum[:])
}

func TestCatalogSpeakerInstallUpdateRemove(t *testing.T) {
	cat := &fakeCatalog{version: "1.0.0", environment: "PLUGIN_ENVIRONMENT_AGENT",
		archive: zipOf(t, map[string]string{"hello/plugin.json": agentManifest, "hello/run.sh": "#!/bin/sh\n"})}
	e, root := newCatalogEnv(t, cat)
	conn := dialCatalogSpeaker(t, e, "kitchen", models.FeaturePlugins, models.FeatureCatalogPlugins)
	readCatalogPlugins(t, conn, func(l []models.CatalogPlugin) bool { return len(l) == 0 })

	code, out := e.do("POST", "/panel/api/catalog/speakers/kitchen", map[string]string{"project_id": "p1"}, true)
	if code != 200 {
		t.Fatalf("install = %d %v", code, out)
	}
	dev := out["devices"].([]any)[0].(map[string]any)
	if dev["catalog"] != true || len(dev["from_catalog"].([]any)) != 1 {
		t.Fatalf("device = %v", dev)
	}
	// The server keeps the archive for the speakers, not in its own plugins.
	if _, err := os.Stat(filepath.Join(root, "hello")); !os.IsNotExist(err) {
		t.Errorf("the speaker plugin was installed on the server: %v", err)
	}
	l := readCatalogPlugins(t, conn, func(l []models.CatalogPlugin) bool { return len(l) == 1 })
	if p := l[0]; p.ID != "hello" || p.Version != "1.0.0" || p.SHA512 != sha512Hex(cat.archive) || p.Path != "/v1/plugins/hello" || p.ProjectID != "p1" {
		t.Fatalf("plugin = %+v", p)
	}

	resp, err := http.Get(e.ts.URL + l[0].Path)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 || !bytes.Equal(body, cat.archive) || resp.Header.Get(checksumHeader) != l[0].SHA512 {
		t.Fatalf("download = %d, %d bytes, checksum %q", resp.StatusCode, len(body), resp.Header.Get(checksumHeader))
	}

	code, out = e.do("GET", "/panel/api/catalog/search", nil, true)
	if p := out["projects"].([]any)[0].(map[string]any); code != 200 || p["speakers"].(map[string]any)["kitchen"] != "1.0.0" {
		t.Fatalf("search = %d %v", code, out)
	}

	// A new version reaches the speaker.
	cat.version = "1.1.0"
	cat.archive = zipOf(t, map[string]string{"plugin.json": strings.Replace(agentManifest, "1.0.0", "1.1.0", 1)})
	if code, out = e.do("POST", "/panel/api/catalog/speakers/kitchen", map[string]string{"project_id": "p1"}, true); code != 200 {
		t.Fatalf("update = %d %v", code, out)
	}
	readCatalogPlugins(t, conn, func(l []models.CatalogPlugin) bool {
		return len(l) == 1 && l[0].Version == "1.1.0" && l[0].SHA512 == sha512Hex(cat.archive)
	})

	// A reconnecting speaker gets its list again.
	conn2 := dialCatalogSpeaker(t, e, "kitchen", models.FeaturePlugins, models.FeatureCatalogPlugins)
	readCatalogPlugins(t, conn2, func(l []models.CatalogPlugin) bool { return len(l) == 1 })
	conn2.Close("done")

	if code, out = e.do("DELETE", "/panel/api/catalog/speakers/kitchen/hello", nil, true); code != 200 {
		t.Fatalf("remove = %d %v", code, out)
	}
	readCatalogPlugins(t, conn, func(l []models.CatalogPlugin) bool { return len(l) == 0 })
	if _, err := os.Stat(filepath.Join(root, speakersDir, "hello")); !os.IsNotExist(err) {
		t.Errorf("the archive stayed: %v", err)
	}
	if resp, err := http.Get(e.ts.URL + "/v1/plugins/hello"); err != nil || resp.StatusCode != 404 {
		t.Fatalf("download after remove = %v %v", resp, err)
	}
	if code, _ := e.do("DELETE", "/panel/api/catalog/speakers/kitchen/hello", nil, true); code != 404 {
		t.Errorf("second remove = %d, want 404", code)
	}
}

func TestCatalogSpeakerInstallRefuses(t *testing.T) {
	cat := &fakeCatalog{version: "1.0.0", environment: "PLUGIN_ENVIRONMENT_SERVER",
		archive: zipOf(t, map[string]string{"plugin.json": helloManifest("1.0.0")})}
	e, root := newCatalogEnv(t, cat)
	dialCatalogSpeaker(t, e, "kitchen", models.FeaturePlugins, models.FeatureCatalogPlugins)
	dialCatalogSpeaker(t, e, "old", models.FeaturePlugins)

	install := func(device string) (int, map[string]any) {
		return e.do("POST", "/panel/api/catalog/speakers/"+device, map[string]string{"project_id": "p1"}, true)
	}
	if code, out := install("kitchen"); code != http.StatusBadGateway {
		t.Errorf("server-only plugin = %d %v, want an error", code, out)
	}
	// The catalog says "both", the archive has no agent half.
	cat.environment = "PLUGIN_ENVIRONMENT_BOTH"
	if code, out := install("kitchen"); code != http.StatusBadGateway {
		t.Errorf("archive without an agent half = %d %v, want an error", code, out)
	}
	cat.environment = "PLUGIN_ENVIRONMENT_AGENT"
	cat.archive = zipOf(t, map[string]string{"plugin.json": agentManifest, "../evil": "x"})
	if code, out := install("kitchen"); code != http.StatusBadGateway {
		t.Errorf("zip slip = %d %v, want an error", code, out)
	}
	cat.archive = zipOf(t, map[string]string{"plugin.json": agentManifest})
	cat.checksum = strings.Repeat("ab", 64)
	if code, out := install("kitchen"); code != http.StatusBadGateway {
		t.Errorf("bad checksum = %d %v, want an error", code, out)
	}
	cat.checksum = ""
	if code, out := install("old"); code != http.StatusConflict {
		t.Errorf("old agent = %d %v, want 409", code, out)
	}
	if code, out := install("nobody"); code != http.StatusNotFound {
		t.Errorf("unknown speaker = %d %v, want 404", code, out)
	}
	if entries, _ := os.ReadDir(filepath.Join(root, speakersDir)); len(entries) != 0 {
		t.Errorf("speaker plugins = %v, want none", entries)
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(root), "evil")); err == nil {
		t.Error("a file was written outside the plugins dir")
	}
}

func TestCatalogSpeakerDownloadNeedsToken(t *testing.T) {
	s := New(Config{Provider: &textProvider{texts: make(chan string, 1)}, PluginsDir: t.TempDir(), AuthToken: "secret"})
	archive := zipOf(t, map[string]string{"plugin.json": agentManifest})
	dir := filepath.Join(s.speakerRoot(), "hello")
	os.MkdirAll(dir, 0o755)
	os.WriteFile(filepath.Join(dir, speakerArchive), archive, 0o644)
	sp := speakerPlugin{Devices: []string{"kitchen"}}
	sp.SHA512 = sha512Hex(archive)
	if err := s.writeSpeakerPlugin("hello", sp); err != nil {
		t.Fatal(err)
	}
	for tok, want := range map[string]int{"": 401, "wrong": 401, "secret": 200} {
		req, _ := http.NewRequest("GET", "/v1/plugins/hello", nil)
		if tok != "" {
			req.Header.Set("Authorization", "Bearer "+tok)
		}
		rec := httptest.NewRecorder()
		s.ServeHTTP(rec, req)
		if rec.Code != want {
			t.Errorf("token %q: %d, want %d", tok, rec.Code, want)
		}
	}
}
