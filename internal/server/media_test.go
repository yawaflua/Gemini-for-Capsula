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
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/yawaflua/marusya-gemini/internal/media"
)

type fakeMedia struct{ got media.Request }

func (f *fakeMedia) Open(_ context.Context, r media.Request) (*media.Track, error) {
	f.got = r
	if r.Query == "nothing" {
		return nil, media.ErrNotFound
	}
	return &media.Track{Title: "Кино — Группа крови", Duration: 285 * time.Second, Audio: io.NopCloser(strings.NewReader("pcm-data"))}, nil
}

func TestMediaStream(t *testing.T) {
	fm := &fakeMedia{}
	s := New(Config{Provider: &optsProvider{}, AuthToken: "shared-secret", Media: fm, Logger: slog.New(slog.DiscardHandler)})
	if _, err := s.SetProfile("haifa-1", Profile{Token: "device-token-0123456789"}); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(s)
	defer srv.Close()
	ctx := context.Background()
	ep, _ := media.EndpointFor("ws" + strings.TrimPrefix(srv.URL, "http") + "/v1/stream")

	// A speaker with its own token can play.
	c := &media.Client{URL: ep, Token: "device-token-0123456789"}
	tr, err := c.Open(ctx, media.Request{Query: "кино группа крови"})
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(tr.Audio)
	tr.Audio.Close()
	if tr.Title != "Кино — Группа крови" || tr.Duration != 285*time.Second || string(b) != "pcm-data" {
		t.Errorf("track = %q %v %q", tr.Title, tr.Duration, b)
	}
	if fm.got.Query != "кино группа крови" {
		t.Errorf("source got %+v", fm.got)
	}

	if _, err := c.Open(ctx, media.Request{Query: "nothing"}); !errors.Is(err, media.ErrNotFound) {
		t.Errorf("not found: err = %v", err)
	}
	if _, err := (&media.Client{URL: ep, Token: "wrong"}).Open(ctx, media.Request{Query: "x"}); err == nil || !strings.Contains(err.Error(), "401") {
		t.Errorf("wrong token: err = %v", err)
	}
	// The server checks the URL too, not only the client.
	resp, err := srv.Client().Get(ep + "?url=file:///etc/passwd")
	if err == nil {
		resp.Body.Close()
	}
	if err != nil || resp.StatusCode != 401 {
		// Unauthenticated requests are refused before anything else.
		t.Errorf("unauthenticated: %v %v", resp.StatusCode, err)
	}
}

func TestMediaDisabled(t *testing.T) {
	s := New(Config{Provider: &optsProvider{}, Logger: slog.New(slog.DiscardHandler)})
	srv := httptest.NewServer(s)
	defer srv.Close()
	_, err := (&media.Client{URL: srv.URL + media.Path}).Open(context.Background(), media.Request{Query: "x"})
	if err == nil || !strings.Contains(err.Error(), "yt-dlp") {
		t.Errorf("err = %v, want a hint about yt-dlp", err)
	}
}
