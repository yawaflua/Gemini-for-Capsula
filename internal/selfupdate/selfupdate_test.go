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

package selfupdate

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestAgentURL(t *testing.T) {
	for in, want := range map[string]string{
		"ws://10.0.0.5:8080/v1/stream":          "http://10.0.0.5:8080/v1/agent/" + runtime.GOARCH,
		"wss://m.example.com/v1/stream?x=1":     "https://m.example.com/v1/agent/" + runtime.GOARCH,
		"wss://m.example.com/marusya/v1/stream": "https://m.example.com/marusya/v1/agent/" + runtime.GOARCH,
	} {
		if got, err := AgentURL(in); err != nil || got != want {
			t.Errorf("AgentURL(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	if _, err := AgentURL("ftp://x/v1/stream"); err == nil {
		t.Error("ftp accepted")
	}
}

// TestCorruptDownload checks that a download not matching the ETag leaves
// the binary alone.
func TestCorruptDownload(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("ETag", ETag(strings.Repeat("0", 64)))
		w.Write([]byte("truncated"))
	}))
	defer srv.Close()
	exe := filepath.Join(t.TempDir(), "agent")
	os.WriteFile(exe, []byte("old"), 0o755)
	if ok, err := (&Updater{URL: srv.URL, Exe: exe}).Check(context.Background()); ok || err == nil || !strings.Contains(err.Error(), "corrupt") {
		t.Errorf("Check = %v, %v; want a corrupt download error", ok, err)
	}
	if b, _ := os.ReadFile(exe); string(b) != "old" {
		t.Errorf("binary %q", b)
	}
	if left, _ := filepath.Glob(filepath.Join(filepath.Dir(exe), ".agent.new-*")); len(left) != 0 {
		t.Errorf("temporary files left: %v", left)
	}
}

func TestNoBinaryOnServer(t *testing.T) {
	srv := httptest.NewServer(http.NotFoundHandler())
	defer srv.Close()
	exe := filepath.Join(t.TempDir(), "agent")
	os.WriteFile(exe, []byte("old"), 0o755)
	if ok, err := (&Updater{URL: srv.URL, Exe: exe}).Check(context.Background()); ok || err != nil {
		t.Errorf("Check = %v, %v; want nothing to do", ok, err)
	}
}
