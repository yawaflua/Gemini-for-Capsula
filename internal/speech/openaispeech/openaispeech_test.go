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

package openaispeech

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/yawaflua/marusya-gemini/internal/models"
)

func TestTranscribe(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/audio/transcriptions" || r.Header.Get("Authorization") != "Bearer k" {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		if err := r.ParseMultipartForm(1 << 20); err != nil {
			t.Error(err)
		}
		if got := r.FormValue("language"); got != "ru" {
			t.Errorf("language = %q, want ru", got)
		}
		if got := r.FormValue("model"); got != DefaultSTTModel {
			t.Errorf("model = %q", got)
		}
		f, _, err := r.FormFile("file")
		if err != nil {
			t.Fatal(err)
		}
		head := make([]byte, 4)
		io.ReadFull(f, head)
		if string(head) != "RIFF" {
			t.Errorf("file is not a WAV: %q", head)
		}
		io.WriteString(w, `{"text":" сделай потише "}`)
	}))
	defer srv.Close()

	s := New(Config{BaseURL: srv.URL + "/v1/", APIKey: "k"})
	got, err := s.Transcribe(context.Background(), make([]byte, 320), models.DeviceInputFormat, "ru-RU")
	if err != nil {
		t.Fatal(err)
	}
	if got != "сделай потише" {
		t.Fatalf("got %q", got)
	}
}

func TestSynthesize(t *testing.T) {
	var req map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewDecoder(r.Body).Decode(&req)
		w.Write([]byte{1, 2, 3, 4, 5}) // an odd trailing byte is dropped
	}))
	defer srv.Close()

	s := New(Config{BaseURL: srv.URL})
	s.SetVoice("marin")
	pcm, err := s.Synthesize(context.Background(), "привет", "ru-RU")
	if err != nil {
		t.Fatal(err)
	}
	if len(pcm) != 4 {
		t.Fatalf("got %d bytes, want 4", len(pcm))
	}
	if req["voice"] != "marin" || req["response_format"] != "pcm" || req["input"] != "привет" {
		t.Fatalf("request = %v", req)
	}
}

func TestAPIError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		io.WriteString(w, `{"error":{"message":"Incorrect API key"}}`)
	}))
	defer srv.Close()

	_, err := New(Config{BaseURL: srv.URL}).Synthesize(context.Background(), "x", "")
	if err == nil || !strings.Contains(err.Error(), "Incorrect API key") {
		t.Fatalf("err = %v", err)
	}
}
