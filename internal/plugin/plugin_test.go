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
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/yawaflua/marusya-gemini/pkg/pluginsdk"
)

// The test binary doubles as a plugin written with the Go SDK.
func TestMain(m *testing.M) {
	if os.Getenv("MARUSYA_TEST_PLUGIN") == "1" {
		os.Exit(testPlugin())
	}
	if os.Getenv("MARUSYA_TEST_PLUGIN") == "caps" {
		os.Exit(capsPlugin())
	}
	os.Exit(m.Run())
}

func testPlugin() int {
	p := pluginsdk.New()
	var greeting string
	p.OnInit(func(_ context.Context, in pluginsdk.Init) error {
		var c struct{ Greeting string }
		if err := json.Unmarshal(in.Config, &c); err != nil {
			return err
		}
		greeting = c.Greeting
		return nil
	})
	p.Tool(pluginsdk.ToolSpec{
		Name: "greet", Description: "Greets someone.",
		Params: []pluginsdk.Param{{Name: "name", Type: pluginsdk.TypeString, Required: true}},
	}, func(ctx context.Context, c pluginsdk.Call) (any, error) {
		// Calls back into the host while serving a call.
		vol, err := p.Command(ctx, c.Device.ID, "volume.get", nil)
		if err != nil {
			return nil, err
		}
		return map[string]any{"text": greeting + ", " + c.String("name"), "volume": vol["level"]}, nil
	})
	p.Tool(pluginsdk.ToolSpec{Name: "volume_get", Description: "Clashes with a built-in tool."}, nil)
	p.Tool(pluginsdk.ToolSpec{Name: "crash", Description: "Exits."}, func(context.Context, pluginsdk.Call) (any, error) {
		os.Exit(3)
		return nil, nil
	})
	p.OnEvent(func(ctx context.Context, e pluginsdk.Event) {
		p.Send(ctx, e.Device, map[string]string{"seen": e.Kind})
	}, pluginsdk.EventTurn)
	p.OnMessage(func(ctx context.Context, m pluginsdk.Message) {
		p.Say(ctx, m.Device, "echo "+string(m.Data), false)
	})
	if err := p.Run(); err != nil {
		return 1
	}
	return 0
}

type fakeHandler struct {
	mu   sync.Mutex
	sent []string
	said []string
	got  chan struct{}
}

func (h *fakeHandler) Command(_ context.Context, plugin, device, action string, _ map[string]any) (map[string]any, error) {
	if action != "volume.get" {
		return nil, errors.New("unexpected " + action)
	}
	return map[string]any{"level": 42}, nil
}

func (h *fakeHandler) Say(_ context.Context, plugin, device, text string, _ bool) (string, error) {
	h.mu.Lock()
	h.said = append(h.said, plugin+"@"+device+": "+text)
	h.mu.Unlock()
	h.got <- struct{}{}
	return "", nil
}

func (h *fakeHandler) Devices(context.Context) []pluginsdk.Device { return nil }

func (h *fakeHandler) Send(_ context.Context, plugin, device string, data json.RawMessage) error {
	h.mu.Lock()
	h.sent = append(h.sent, plugin+"@"+device+": "+string(data))
	h.mu.Unlock()
	h.got <- struct{}{}
	return nil
}

func writePlugin(t *testing.T, root, id string, config string) {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(root, id)
	os.MkdirAll(dir, 0o755)
	man := `{"id":"` + id + `","name":"Тест","server":{"command":[` + strings.Trim(mustJSON(exe), "") +
		`],"env":{"MARUSYA_TEST_PLUGIN":"1"}},"config":` + config + `}`
	if err := os.WriteFile(filepath.Join(dir, ManifestFile), []byte(man), 0o644); err != nil {
		t.Fatal(err)
	}
}

func mustJSON(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}

func wait(t *testing.T, ch <-chan struct{}) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(5 * time.Second):
		t.Fatal("timed out")
	}
}

func TestManagerRunsGoPlugin(t *testing.T) {
	root := t.TempDir()
	writePlugin(t, root, "hello", `{"greeting":"Привет"}`)
	h := &fakeHandler{got: make(chan struct{}, 4)}
	m := New(Config{
		Dir: root, Host: pluginsdk.HostServer, Handler: h,
		Reserved: func(n string) bool { return n == "volume_get" },
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	m.Start(ctx)

	tools := m.Tools()
	if len(tools) != 2 || tools[0].Name != "greet" || tools[0].Plugin != "hello" {
		t.Fatalf("tools = %+v (the built-in name must be rejected)", tools)
	}
	out, err := m.Call(ctx, "greet", map[string]any{"name": "Маша"}, pluginsdk.Device{ID: "kitchen"})
	if err != nil {
		t.Fatal(err)
	}
	if out["text"] != "Привет, Маша" || out["volume"] != float64(42) {
		t.Fatalf("greet = %v", out)
	}

	m.Event(pluginsdk.Event{Kind: pluginsdk.EventCommand, Device: "kitchen"}) // not subscribed
	m.Event(pluginsdk.Event{Kind: pluginsdk.EventTurn, Device: "kitchen"})
	wait(t, h.got)
	m.Deliver("hello", "kitchen", json.RawMessage(`{"x":1}`))
	wait(t, h.got)
	h.mu.Lock()
	if strings.Join(h.sent, "|") != `hello@kitchen: {"seen":"turn"}` || strings.Join(h.said, "|") != `hello@kitchen: echo {"x":1}` {
		t.Fatalf("sent %q, said %q", h.sent, h.said)
	}
	h.mu.Unlock()

	st := m.List()
	if len(st) != 1 || st[0].State != StateRunning || !st[0].Enabled || st[0].Name != "Тест" {
		t.Fatalf("status = %+v", st)
	}
}

func TestCrashedPluginIsRestarted(t *testing.T) {
	root := t.TempDir()
	writePlugin(t, root, "crashy", `{"greeting":"Hi"}`)
	m := New(Config{Dir: root, Handler: &fakeHandler{got: make(chan struct{}, 4)}})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	m.Start(ctx)

	if _, err := m.Call(ctx, "crash", nil, pluginsdk.Device{}); err == nil {
		t.Fatal("crash returned no error")
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		st := m.List()[0]
		if st.State == StateRunning && st.Restarts == 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("not restarted: %+v", st)
		}
		time.Sleep(50 * time.Millisecond)
	}
	if _, err := m.Call(ctx, "greet", map[string]any{"name": "x"}, pluginsdk.Device{}); err != nil {
		t.Fatalf("after restart: %v", err)
	}
}

func TestDisableAndConfigArePersisted(t *testing.T) {
	root := t.TempDir()
	writePlugin(t, root, "hello", `{"greeting":"Привет"}`)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	h := &fakeHandler{got: make(chan struct{}, 4)}
	m := New(Config{Dir: root, Handler: h})
	m.Start(ctx)

	if err := m.SetConfig("hello", json.RawMessage(`{"greeting":"Шалом"}`)); err != nil {
		t.Fatal(err)
	}
	out, err := m.Call(ctx, "greet", map[string]any{"name": "Дима"}, pluginsdk.Device{})
	if err != nil || out["text"] != "Шалом, Дима" {
		t.Fatalf("after SetConfig: %v, %v", out, err)
	}
	if err := m.SetConfig("hello", json.RawMessage(`[1]`)); err == nil {
		t.Error("a non-object config was accepted")
	}
	if err := m.SetEnabled("hello", false); err != nil {
		t.Fatal(err)
	}
	if len(m.Tools()) != 0 || m.List()[0].State != StateDisabled {
		t.Fatalf("disabled plugin: tools %v, %+v", m.Tools(), m.List())
	}

	// A new manager remembers both.
	m2 := New(Config{Dir: root, Handler: h})
	m2.Start(ctx)
	if st := m2.List()[0]; st.Enabled || st.State != StateDisabled || string(st.Config) != `{"greeting":"Шалом"}` {
		t.Fatalf("reloaded = %+v", st)
	}
	m2.SetEnabled("hello", true)
	if _, err := m2.Call(ctx, "greet", map[string]any{"name": "a"}, pluginsdk.Device{}); err != nil {
		t.Fatalf("re-enabled: %v", err)
	}
}

func TestDiscoverSkipsBrokenManifests(t *testing.T) {
	root := t.TempDir()
	for dir, man := range map[string]string{
		"ok":     `{"id":"ok","agent":{"command":["./a"]}}`,
		"bad-id": `{"id":"Bad ID","server":{"command":["x"]}}`,
		"noside": `{"id":"noside"}`,
		"zz-dup": `{"id":"ok","server":{"command":["x"]}}`,
		"broken": `{`,
	} {
		os.MkdirAll(filepath.Join(root, dir), 0o755)
		os.WriteFile(filepath.Join(root, dir, ManifestFile), []byte(man), 0o644)
	}
	ms, errs := Discover(root)
	if len(ms) != 1 || ms[0].ID != "ok" || len(errs) != 4 {
		t.Fatalf("manifests %+v, errors %v", ms, errs)
	}
	// An agent-only plugin does not run on the server.
	m := New(Config{Dir: root, Host: pluginsdk.HostServer})
	m.Start(t.Context())
	if len(m.List()) != 0 {
		t.Fatalf("server lists %+v", m.List())
	}
}
