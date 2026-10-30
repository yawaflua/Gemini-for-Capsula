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

package registry

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/yawaflua/marusya-gemini/internal/provider"
	"github.com/yawaflua/marusya-gemini/internal/provider/echo"
)

// named is a provider that records the spec it was built from.
type named struct {
	echo.Provider
	spec Spec
}

func (n named) Name() string { return n.spec.ID + "/" + n.spec.Model + "/" + n.spec.APIKey }

func fakeBuild(_ context.Context, s Spec, _ []Spec) (provider.Provider, error) {
	if s.Model == "broken" {
		return nil, errors.New("broken")
	}
	return named{spec: s}, nil
}

func TestPutActivatePersist(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "providers.json")
	r, err := Open(path, echo.Provider{}, fakeBuild, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.Put(ctx, Spec{ID: "or", Name: "OpenRouter", Kind: KindOpenAI, APIKey: "sk-1", Model: "m1"}); err != nil {
		t.Fatal(err)
	}
	if err := r.Activate(ctx, "or"); err != nil {
		t.Fatal(err)
	}
	if got := r.Provider().Name(); got != "or/m1/sk-1" {
		t.Fatalf("active provider = %q", got)
	}

	// Editing the active provider without a key keeps the key and rebuilds it.
	active, err := r.Put(ctx, Spec{ID: "or", Name: "OpenRouter", Kind: KindOpenAI, Model: "m2"})
	if err != nil || !active {
		t.Fatalf("Put = %v, %v", active, err)
	}
	if got := r.Provider().Name(); got != "or/m2/sk-1" {
		t.Fatalf("after edit = %q", got)
	}

	views, act := r.List()
	if act != "or" || len(views) != 2 || views[0].ID != DefaultID || !views[1].HasKey || views[1].APIKey != "" {
		t.Fatalf("List = %+v, %q", views, act)
	}

	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Fatalf("providers file mode = %v, want 0600", fi.Mode().Perm())
	}

	// A new registry restores the active provider.
	r2, err := Open(path, echo.Provider{}, fakeBuild, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := r2.Restore(ctx); err != nil {
		t.Fatal(err)
	}
	if got := r2.Provider().Name(); got != "or/m2/sk-1" {
		t.Fatalf("restored = %q", got)
	}

	if err := r2.Delete("or"); err == nil {
		t.Fatal("deleted the active provider")
	}
	if err := r2.Activate(ctx, DefaultID); err != nil {
		t.Fatal(err)
	}
	if got := r2.Provider().Name(); got != "echo" {
		t.Fatalf("default = %q", got)
	}
	if err := r2.Delete("or"); err != nil {
		t.Fatal(err)
	}
}

func TestBrokenActiveEditIsRejected(t *testing.T) {
	ctx := context.Background()
	r, _ := Open("", echo.Provider{}, fakeBuild, nil)
	r.Put(ctx, Spec{ID: "a", Name: "A", Kind: KindGemini, Model: "ok"})
	r.Activate(ctx, "a")
	if _, err := r.Put(ctx, Spec{ID: "a", Name: "A", Kind: KindGemini, Model: "broken"}); err == nil {
		t.Fatal("a broken edit of the active provider was accepted")
	}
	if got := r.Provider().Name(); got != "a/ok/" {
		t.Fatalf("active provider = %q", got)
	}
}

func TestSpeechReferences(t *testing.T) {
	ctx := context.Background()
	r, _ := Open("", echo.Provider{}, fakeBuild, nil)
	if _, err := r.Put(ctx, Spec{ID: "c", Name: "Claude", Kind: KindAnthropic, Speech: "nope"}); err == nil {
		t.Fatal("unknown speech provider accepted")
	}
	if _, err := r.Put(ctx, Spec{ID: "c", Name: "Claude", Kind: KindAnthropic, Speech: SpeechSelf}); err == nil {
		t.Fatal("Claude cannot transcribe speech itself")
	}
	r.Put(ctx, Spec{ID: "oa", Name: "OpenAI", Kind: KindOpenAIRealtime})
	if _, err := r.Put(ctx, Spec{ID: "c", Name: "Claude", Kind: KindAnthropic, Speech: "oa"}); err != nil {
		t.Fatal(err)
	}
	if err := r.Delete("oa"); err == nil || !strings.Contains(err.Error(), "Claude") {
		t.Fatalf("deleting a speech provider in use: %v", err)
	}
}

func TestValidate(t *testing.T) {
	for _, s := range []Spec{
		{ID: "default", Name: "x", Kind: KindGemini},
		{ID: "Bad id", Name: "x", Kind: KindGemini},
		{ID: "a", Name: "", Kind: KindGemini},
		{ID: "a", Name: "x", Kind: "other"},
		{ID: "a", Name: "x", Kind: KindOpenAI, BaseURL: "api.openai.com"},
		{ID: "a", Name: "x", Kind: KindOpenAI, Model: "two words"},
	} {
		if err := s.validate(nil); err == nil {
			t.Errorf("%+v is valid", s)
		}
	}
	s := Spec{ID: " My-AI ", Name: "x", Kind: KindOpenAIRealtime}
	if err := s.validate(nil); err != nil || s.ID != "my-ai" || s.Model == "" {
		t.Fatalf("validate = %v, %+v", err, s)
	}
}

func TestSwitchTunable(t *testing.T) {
	sw := NewSwitch(echo.Provider{})
	if _, ok := provider.AsTuner(sw); ok {
		t.Fatal("echo is not tunable")
	}
	if sw.Tuning() != (provider.Tuning{}) {
		t.Fatal("tuning of an untunable provider")
	}
}
