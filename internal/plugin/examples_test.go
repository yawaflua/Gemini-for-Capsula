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
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/yawaflua/marusya-gemini/internal/models"
	"github.com/yawaflua/marusya-gemini/internal/provider"
	"github.com/yawaflua/marusya-gemini/internal/tools"
	"github.com/yawaflua/marusya-gemini/pkg/pluginsdk"
)

// commands records the speaker commands of a plugin.
type commands struct {
	fakeHandler
	mu  sync.Mutex
	ran []string
}

func (h *commands) Command(_ context.Context, plugin, device, action string, args map[string]any) (map[string]any, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.ran = append(h.ran, fmt.Sprintf("%s@%s %s %s", plugin, device, action, mustJSON(args)))
	return map[string]any{}, nil
}

func (h *commands) Devices(context.Context) []pluginsdk.Device {
	return []pluginsdk.Device{{ID: "kitchen"}}
}

func (h *commands) list() string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return strings.Join(h.ran, "\n")
}

// pythonExample copies a Python example and the SDK into a plugin
// directory, with config merged into the manifest's config, and starts it.
func pythonExample(t *testing.T, name string, config map[string]any, h Handler) (*Manager, string) {
	t.Helper()
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("no python3")
	}
	ex := filepath.Join("..", "..", "examples", "plugins", name)
	root := t.TempDir()
	dir := filepath.Join(root, name)
	os.MkdirAll(dir, 0o755)
	copyFile(t, filepath.Join(ex, name+".py"), filepath.Join(dir, name+".py"))
	copyFile(t, filepath.Join("..", "..", "sdk", "python", "marusya_plugin.py"), filepath.Join(dir, "marusya_plugin.py"))
	b, err := os.ReadFile(filepath.Join(ex, ManifestFile))
	if err != nil {
		t.Fatal(err)
	}
	var man map[string]any
	if err := json.Unmarshal(b, &man); err != nil {
		t.Fatal(err)
	}
	cfg, _ := man["config"].(map[string]any)
	if cfg == nil {
		cfg = map[string]any{}
	}
	for k, v := range config {
		cfg[k] = v
	}
	man["config"] = cfg
	os.WriteFile(filepath.Join(dir, ManifestFile), []byte(mustJSON(man)), 0o644)

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	m := New(Config{Dir: root, Handler: h})
	m.Start(ctx)
	if st := m.List(); len(st) != 1 || st[0].State != StateRunning {
		t.Fatalf("%s: %+v", name, st)
	}
	return m, dir
}

func writeScript(t *testing.T, path, body string) {
	t.Helper()
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"+body), 0o755); err != nil {
		t.Fatal(err)
	}
}

func TestMoodExample(t *testing.T) {
	h := &commands{}
	m, _ := pythonExample(t, "mood", nil, h)
	if got := m.Instructions(); got != "" {
		t.Errorf("initial instructions = %q", got)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	out, err := m.Call(ctx, "set_mood", map[string]any{"mood": "пиратская"}, pluginsdk.Device{ID: "kitchen"})
	if err != nil || out["mood"] != "пиратская" {
		t.Fatalf("set_mood = %v, %v", out, err)
	}
	if got := m.Instructions(); !strings.Contains(got, "пират") {
		t.Errorf("instructions = %q", got)
	}
	if got := h.list(); !strings.Contains(got, `mood@kitchen ring.set`) || !strings.Contains(got, `"ff3300"`) {
		t.Errorf("commands = %s", got)
	}
	res, err := m.HTTP(ctx, "mood", pluginsdk.HTTPRequest{Method: "GET", Path: "/"})
	if err != nil || res.Status != 200 || !strings.Contains(string(res.Body), "<button disabled onclick=\"pick(this)\">пиратская") {
		t.Fatalf("page = %v %s", err, res.Body)
	}
	if _, err := m.HTTP(ctx, "mood", pluginsdk.HTTPRequest{Method: "POST", Path: "/", Body: []byte(`{"mood":"обычная"}`)}); err != nil {
		t.Fatal(err)
	}
	if got := m.Instructions(); got != "" {
		t.Errorf("instructions after reset = %q", got)
	}
}

func TestYTMusicExample(t *testing.T) {
	h := &commands{}
	// yt-dlp prints one search result.
	bin := t.TempDir()
	writeScript(t, filepath.Join(bin, "yt-dlp"), `case "$*" in *music.youtube.com/search*) ;; *) exit 1;; esac
echo '{"entries":[{"id":"abc123","title":"Группа крови","channel":"Кино"}]}'`)
	m, _ := pythonExample(t, "ytmusic", map[string]any{"yt_dlp": filepath.Join(bin, "yt-dlp")}, h)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	out, err := m.Call(ctx, "play_youtube_music", map[string]any{"query": "кино группа крови"}, pluginsdk.Device{ID: "kitchen"})
	if err != nil || out["title"] != "Группа крови" || out["artist"] != "Кино" {
		t.Fatalf("play = %v, %v", out, err)
	}
	if got := h.list(); got != `ytmusic@kitchen media.play_url {"url":"https://www.youtube.com/watch?v=abc123"}` {
		t.Errorf("commands = %s", got)
	}
	res, err := m.HTTP(ctx, "ytmusic", pluginsdk.HTTPRequest{Method: "GET", Path: "/", Query: "q=kino"})
	if err != nil || !strings.Contains(string(res.Body), `data-id="abc123"`) || !strings.Contains(string(res.Body), "<option>kitchen</option>") {
		t.Fatalf("page = %v %s", err, res.Body)
	}
}

func TestPiperExample(t *testing.T) {
	m, dir := pythonExample(t, "piper", map[string]any{
		"piper": "./fake-piper", "voice": "b",
		"voices": map[string]any{"a": "a.onnx", "b": "b.onnx"},
	}, &fakeHandler{})
	// Piper prints 100 samples per call; voice b speaks at half the rate.
	writeScript(t, filepath.Join(dir, "fake-piper"), `cat >/dev/null; head -c 200 /dev/zero`)
	os.WriteFile(filepath.Join(dir, "b.onnx.json"), []byte(`{"audio":{"sample_rate":11025}}`), 0o644)

	ss := m.SpeechEngines()
	if len(ss) != 1 || ss[0].ID != "piper/piper" || ss[0].SampleRate != 22050 || strings.Join(ss[0].Voices, ",") != "a,b" {
		t.Fatalf("engines = %+v", ss)
	}
	b := &Backends{}
	b.Set(m)
	sp, _ := b.Speech("piper/piper", fakeSpeech{}, fakeSpeech{})
	pcm, err := sp.Synthesize(context.Background(), "Привет", "ru-RU")
	if err != nil || len(pcm) != 400 {
		t.Fatalf("voice b = %d bytes, %v", len(pcm), err)
	}
	sp.SetVoice("a")
	if pcm, err := sp.Synthesize(context.Background(), "Привет", "ru-RU"); err != nil || len(pcm) != 200 {
		t.Fatalf("voice a = %d bytes, %v", len(pcm), err)
	}
}

func TestOllamaExample(t *testing.T) {
	var mu sync.Mutex
	var bodies []map[string]any
	ollama := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		json.NewDecoder(r.Body).Decode(&body)
		mu.Lock()
		bodies = append(bodies, body)
		mu.Unlock()
		msgs := body["messages"].([]any)
		last := msgs[len(msgs)-1].(map[string]any)
		bw := bufio.NewWriter(w)
		line := func(v any) { bw.WriteString(mustJSON(v) + "\n") }
		switch {
		case last["role"] == "tool":
			line(map[string]any{"message": map[string]any{"role": "assistant", "content": "Громкость "}})
			line(map[string]any{"message": map[string]any{"role": "assistant", "content": "сорок два."}, "done": true})
		default:
			line(map[string]any{"message": map[string]any{"role": "assistant", "tool_calls": []any{
				map[string]any{"function": map[string]any{"name": "volume_get", "arguments": map[string]any{}}},
			}}, "done": true})
		}
		bw.Flush()
	}))
	defer ollama.Close()

	m, _ := pythonExample(t, "ollama", map[string]any{"url": ollama.URL, "model": "test"}, &fakeHandler{})
	if ps := m.Providers(); len(ps) != 1 || ps[0].ID != "ollama/chat" {
		t.Fatalf("providers = %+v", ps)
	}
	b := &Backends{}
	b.Set(m)
	p, err := b.Provider("ollama/chat", fakeSpeech{}, fakeSpeech{})
	if err != nil {
		t.Fatal(err)
	}
	vol, _ := tools.Lookup("volume_get")
	s, err := p.Connect(context.Background(), provider.SessionOptions{
		SystemInstruction: "Ты Маруся.", InputFormat: models.DeviceInputFormat, Tools: []tools.Tool{vol},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	s.(provider.TextSender).SendText("какая громкость?")
	_, said := collect(t, s, func(provider.ToolCall) map[string]any { return map[string]any{"level": 42} })
	if strings.TrimSpace(said) != "Громкость сорок два." {
		t.Errorf("said %q", said)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(bodies) != 2 || bodies[0]["model"] != "test" {
		t.Fatalf("requests = %v", bodies)
	}
	first := bodies[0]["messages"].([]any)
	tl := bodies[0]["tools"].([]any)[0].(map[string]any)["function"].(map[string]any)
	if first[0].(map[string]any)["content"] != "Ты Маруся." || tl["name"] != "volume_get" {
		t.Errorf("first request = %v", bodies[0])
	}
	second := bodies[1]["messages"].([]any)
	if len(second) != 4 || !strings.Contains(second[3].(map[string]any)["content"].(string), `"level": 42`) {
		t.Errorf("second request messages = %v", second)
	}
}

func TestMemoryExample(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "memory")
	os.MkdirAll(dir, 0o755)
	build := exec.Command("go", "build", "-o", filepath.Join(dir, "memory"), "../../examples/plugins/memory")
	build.Env = append(os.Environ(), "CGO_ENABLED=0")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build: %v\n%s", err, out)
	}
	copyFile(t, filepath.Join("..", "..", "examples", "plugins", "memory", ManifestFile), filepath.Join(dir, ManifestFile))
	start := func() (*Manager, context.CancelFunc) {
		ctx, cancel := context.WithCancel(context.Background())
		m := New(Config{Dir: root, Host: pluginsdk.HostServer, Handler: &fakeHandler{}})
		m.Start(ctx)
		if st := m.List(); len(st) != 1 || st[0].State != StateRunning {
			t.Fatalf("memory: %+v", st)
		}
		return m, cancel
	}
	m, stop := start()
	if got := m.Instructions(); got != "" {
		t.Errorf("initial instructions = %q", got)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	kitchen := pluginsdk.Device{ID: "kitchen"}
	if out, err := m.Call(ctx, "memory_save", map[string]any{"fact": "Пользователя зовут Дима"}, kitchen); err != nil || out["id"] != float64(1) {
		t.Fatalf("save = %v, %v", out, err)
	}
	m.Call(ctx, "memory_save", map[string]any{"fact": "Кота зовут Барсик"}, kitchen)
	if got := m.Instructions(); !strings.Contains(got, "[1] Пользователя зовут Дима\n[2] Кота зовут Барсик") {
		t.Errorf("instructions = %q", got)
	}
	if out, err := m.Call(ctx, "memory_forget", map[string]any{"text": "барсик"}, kitchen); err != nil || out["forgotten"] != "Кота зовут Барсик" {
		t.Fatalf("forget = %v, %v", out, err)
	}
	if _, err := m.HTTP(ctx, "memory", pluginsdk.HTTPRequest{Method: "POST", Path: "/", Body: []byte(`{"action":"add","text":"Дима любит <джаз>"}`)}); err != nil {
		t.Fatal(err)
	}
	res, err := m.HTTP(ctx, "memory", pluginsdk.HTTPRequest{Method: "GET", Path: "/"})
	if err != nil || res.Status != 200 || !strings.Contains(string(res.Body), "Дима любит &lt;джаз&gt;") || strings.Contains(string(res.Body), "<td>Кота зовут Барсик</td>") {
		t.Fatalf("page = %v %s", err, res.Body)
	}
	stop()

	// The memory is kept outside the plugin directory and survives a restart.
	if _, err := os.Stat(filepath.Join(root, ".memory.json")); err != nil {
		t.Fatal(err)
	}
	m, stop = start()
	defer stop()
	if got := m.Instructions(); !strings.Contains(got, "[1] Пользователя зовут Дима\n[3] Дима любит <джаз>") {
		t.Errorf("instructions after restart = %q", got)
	}
}
