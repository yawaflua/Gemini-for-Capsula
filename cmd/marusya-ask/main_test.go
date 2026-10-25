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

package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
)

func TestMain(m *testing.M) {
	os.Setenv("MARUSYA_ASK_LOG", "-")
	os.Exit(m.Run())
}

func TestSummarize(t *testing.T) {
	msg := "## Готово\n\nЯ поправил `ringled.go` и прогнал тесты, см. [PR](https://x/y).\n\n```go\nfmt.Println(1)\n```\n\nОставить старый драйвер? Или удалить его совсем?"
	text, q := summarize(msg)
	if !q || text != "Оставить старый драйвер? Или удалить его совсем?" {
		t.Errorf("question: %q %v", text, q)
	}
	text, q = summarize("**Сделано.** Все тесты проходят.")
	if q || text != "Сделано." {
		t.Errorf("done: %q %v", text, q)
	}
}

func TestHookStop(t *testing.T) {
	var got askRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/ask" || r.Header.Get("Authorization") != "Bearer tok" {
			http.Error(w, "no", 401)
			return
		}
		json.NewDecoder(r.Body).Decode(&got)
		json.NewEncoder(w).Encode(askResponse{Device: "d", Answered: true, Answer: "да, удаляй"})
	}))
	defer srv.Close()
	*serverURL, *token = srv.URL, "tok"

	in := `{"hook_event_name":"Stop","cwd":"/home/u/Marusa","last_assistant_message":"Удалить старый драйвер?"}`
	var out bytes.Buffer
	if err := runHook(strings.NewReader(in), &out); err != nil {
		t.Fatal(err)
	}
	if !got.Listen || got.Source != "claude" || got.Text != "Клод в проекте Marusa спрашивает: Удалить старый драйвер?" {
		t.Errorf("request = %+v", got)
	}
	var res map[string]string
	json.Unmarshal(out.Bytes(), &res)
	if res["decision"] != "block" || !strings.Contains(res["reason"], "да, удаляй") {
		t.Errorf("hook output = %s", out.String())
	}

	out.Reset()
	got = askRequest{}
	in = `{"hook_event_name":"Notification","message":"Claude needs your permission to use Bash"}`
	if err := runHook(strings.NewReader(in), &out); err != nil {
		t.Fatal(err)
	}
	if got.Listen || !strings.Contains(got.Text, "разрешение на Bash") || out.Len() != 0 {
		t.Errorf("notification request = %+v, output %q", got, out.String())
	}
}

func TestHookAskUserQuestion(t *testing.T) {
	var got []askRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req askRequest
		json.NewDecoder(r.Body).Decode(&req)
		got = append(got, req)
		json.NewEncoder(w).Encode(askResponse{Answered: true, Answer: "синий"})
	}))
	defer srv.Close()
	*serverURL, *token = srv.URL, "tok"

	in := `{"hook_event_name":"PreToolUse","tool_name":"AskUserQuestion","cwd":"/p/Marusa",
		"tool_input":{"questions":[{"question":"Какой цвет тебе нравится?","options":[{"label":"Синий"},{"label":"Красный"},{"label":"Зелёный"}]}]}}`
	var out bytes.Buffer
	if err := runHook(strings.NewReader(in), &out); err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || !got[0].Listen || got[0].Text != "Клод в проекте Marusa спрашивает: Какой цвет тебе нравится? Варианты: Синий, Красный или Зелёный." {
		t.Errorf("requests = %+v", got)
	}
	var res struct {
		H map[string]string `json:"hookSpecificOutput"`
	}
	json.Unmarshal(out.Bytes(), &res)
	if res.H["permissionDecision"] != "deny" || !strings.Contains(res.H["permissionDecisionReason"], "«синий»") {
		t.Errorf("output = %s", out.String())
	}

	// The permission notification for the same dialog stays silent.
	got = nil
	out.Reset()
	runHook(strings.NewReader(`{"hook_event_name":"Notification","message":"Claude needs your permission to use AskUserQuestion"}`), &out)
	if len(got) != 0 {
		t.Errorf("notification read out: %+v", got)
	}
}

func TestIsDone(t *testing.T) {
	for _, a := range []string{"Всё.", "все", "Нет, спасибо!", "Хватит"} {
		if !isDone(a) {
			t.Errorf("%q should end", a)
		}
	}
	for _, a := range []string{"Всё переделай", "нет, сделай по-другому"} {
		if isDone(a) {
			t.Errorf("%q is an instruction", a)
		}
	}
}
