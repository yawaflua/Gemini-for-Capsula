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

package ytdlp

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/yawaflua/marusya-gemini/internal/media"
)

// fakes writes stand-ins for yt-dlp and ffmpeg: yt-dlp prints its
// arguments' target as the title and "audio" as the download, ffmpeg copies
// stdin to stdout.
func fakes(t *testing.T, ytdlp string) *Source {
	t.Helper()
	dir := t.TempDir()
	write := func(name, body string) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte("#!/bin/sh\n"+body), 0o755); err != nil {
			t.Fatal(err)
		}
		return p
	}
	return &Source{
		YTDLP:        write("yt-dlp", ytdlp),
		FFmpeg:       write("ffmpeg", "exec cat\n"),
		StartTimeout: 5 * time.Second,
	}
}

const found = `for a; do last="$a"; done
printf '%s\t212.5\n' "$last" > /dev/fd/3
printf 'audio-bytes'
`

func TestOpenSearch(t *testing.T) {
	s := fakes(t, found)
	tr, err := s.Open(context.Background(), media.Request{Query: "-rf песня"})
	if err != nil {
		t.Fatal(err)
	}
	defer tr.Audio.Close()
	if tr.Title != "ytsearch1:-rf песня" {
		t.Errorf("title = %q; the query must follow --", tr.Title)
	}
	if tr.Duration != 212500*time.Millisecond {
		t.Errorf("duration = %v", tr.Duration)
	}
	b, err := io.ReadAll(tr.Audio)
	if err != nil || string(b) != "audio-bytes" {
		t.Errorf("audio = %q, %v", b, err)
	}
}

func TestOpenNotFound(t *testing.T) {
	s := fakes(t, "exit 0\n")
	_, err := s.Open(context.Background(), media.Request{Query: "nothing"})
	if !errors.Is(err, media.ErrNotFound) {
		t.Errorf("err = %v, want ErrNotFound", err)
	}
}

func TestOpenFailure(t *testing.T) {
	s := fakes(t, "echo 'ERROR: Video unavailable' >&2; exit 1\n")
	_, err := s.Open(context.Background(), media.Request{URL: "https://youtu.be/x"})
	if err == nil || !strings.Contains(err.Error(), "Video unavailable") {
		t.Errorf("err = %v", err)
	}
}

func TestCloseStopsStream(t *testing.T) {
	s := fakes(t, `printf 'live\t\n' > /dev/fd/3
while :; do printf 'xxxxxxxx'; done
`)
	tr, err := s.Open(context.Background(), media.Request{URL: "https://radio.example/live"})
	if err != nil {
		t.Fatal(err)
	}
	if tr.Duration != 0 {
		t.Errorf("duration = %v, want 0 for live", tr.Duration)
	}
	buf := make([]byte, 64)
	if _, err := io.ReadFull(tr.Audio, buf); err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() { tr.Audio.Close(); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Close did not stop the processes")
	}
}

func TestRejectsLocalURLs(t *testing.T) {
	s := fakes(t, found)
	for _, u := range []string{"file:///etc/passwd", "/etc/passwd", "-o/tmp/x"} {
		if _, err := s.Open(context.Background(), media.Request{URL: u}); err == nil {
			t.Errorf("Open(%q) succeeded", u)
		}
	}
}
