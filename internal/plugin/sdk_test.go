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

package plugin

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/yawaflua/marusya-gemini/pkg/pluginsdk"
)

// relay connects the two halves of a plugin like the server and the agent
// do over the speaker connection.
type relay struct {
	fakeHandler
	other  **Manager
	device string
}

func (r *relay) Send(_ context.Context, plugin, device string, data json.RawMessage) error {
	if device == "" {
		device = r.device
	}
	return (*r.other).Deliver(plugin, device, data)
}

func copyFile(t *testing.T, src, dst string) {
	t.Helper()
	b, err := os.ReadFile(src)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dst, b, 0o644); err != nil {
		t.Fatal(err)
	}
}

// TestCppAndPythonSDKs runs the speaker-stats example: its C++ half as an
// agent plugin and its Python half as a server plugin.
func TestCppAndPythonSDKs(t *testing.T) {
	cxx, err := exec.LookPath("g++")
	if err != nil {
		t.Skip("no g++")
	}
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("no python3")
	}
	ex := filepath.Join("..", "..", "examples", "plugins", "speaker-stats")
	root := t.TempDir()
	dir := filepath.Join(root, "speaker-stats")
	os.MkdirAll(dir, 0o755)
	for _, f := range []string{"plugin.json", "server.py"} {
		copyFile(t, filepath.Join(ex, f), filepath.Join(dir, f))
	}
	copyFile(t, filepath.Join("..", "..", "sdk", "python", "marusya_plugin.py"), filepath.Join(dir, "marusya_plugin.py"))
	build := exec.Command(cxx, "-std=c++17", "-O1", "-Wall", "-Wextra", "-Werror", "-pthread",
		"-I", filepath.Join("..", "..", "sdk", "cpp"), filepath.Join(ex, "agent.cpp"), "-o", filepath.Join(dir, "speaker-stats"))
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("g++: %v\n%s", err, out)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	var srv, agent *Manager
	srv = New(Config{Dir: root, Host: pluginsdk.HostServer, Handler: &relay{other: &agent}, StateFile: filepath.Join(root, "s.json")})
	agent = New(Config{Dir: root, Host: pluginsdk.HostAgent, Device: &pluginsdk.Device{ID: "kitchen"},
		Handler: &relay{other: &srv, device: "kitchen"}, StateFile: filepath.Join(root, "a.json")})
	srv.Start(ctx)
	agent.Start(ctx)

	if st := agent.List(); len(st) != 1 || st[0].State != StateRunning {
		t.Fatalf("C++ half: %+v", st)
	}
	if st := srv.List(); len(st) != 1 || st[0].State != StateRunning {
		t.Fatalf("Python half: %+v", st)
	}
	up, err := agent.Call(ctx, "speaker_uptime", nil, pluginsdk.Device{ID: "kitchen"})
	if err != nil || up["uptime_minutes"] == nil {
		t.Fatalf("speaker_uptime = %v, %v", up, err)
	}

	agent.Event(pluginsdk.Event{Kind: pluginsdk.EventState, Device: "kitchen", Data: map[string]any{"state": "listening"}})
	srv.Event(pluginsdk.Event{Kind: pluginsdk.EventTurn, Device: "kitchen"})
	for {
		out, err := srv.Call(ctx, "speaker_stats", map[string]any{}, pluginsdk.Device{ID: "kitchen"})
		if err != nil {
			t.Fatal(err)
		}
		k, _ := out["speakers"].(map[string]any)["kitchen"].(map[string]any)
		if k["wakes"] == float64(1) && k["turns"] == float64(1) && out["asked_from"] == "kitchen" {
			break
		}
		if ctx.Err() != nil {
			t.Fatalf("stats = %v", out)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// TestCoinExample runs the simplest example plugin.
func TestCoinExample(t *testing.T) {
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("no python3")
	}
	root := t.TempDir()
	dir := filepath.Join(root, "coin")
	os.MkdirAll(dir, 0o755)
	ex := filepath.Join("..", "..", "examples", "plugins", "coin")
	for _, f := range []string{"plugin.json", "coin.py"} {
		copyFile(t, filepath.Join(ex, f), filepath.Join(dir, f))
	}
	copyFile(t, filepath.Join("..", "..", "sdk", "python", "marusya_plugin.py"), filepath.Join(dir, "marusya_plugin.py"))

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	m := New(Config{Dir: root})
	m.Start(ctx)
	out, err := m.Call(ctx, "coin_flip", nil, pluginsdk.Device{ID: "kitchen"})
	if err != nil || (out["side"] != "орёл" && out["side"] != "решка") {
		t.Fatalf("coin_flip = %v, %v", out, err)
	}
}
