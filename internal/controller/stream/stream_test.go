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

package stream

import (
	"bytes"
	"context"
	"errors"
	"io"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/yawaflua/marusya-gemini/internal/audio"
	"github.com/yawaflua/marusya-gemini/internal/media"
	"github.com/yawaflua/marusya-gemini/internal/models"
)

// endless is an infinite silent track.
type endless struct{ closed atomic.Bool }

func (e *endless) Read(p []byte) (int, error) {
	if e.closed.Load() {
		return 0, errors.New("closed")
	}
	return len(p), nil
}
func (e *endless) Close() error { e.closed.Store(true); return nil }

type source struct {
	mu   sync.Mutex
	reqs []media.Request
	body func() io.ReadCloser
	err  error
}

func (s *source) Open(_ context.Context, r media.Request) (*media.Track, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.reqs = append(s.reqs, r)
	if s.err != nil {
		return nil, s.err
	}
	return &media.Track{Title: "Кино — Группа крови", Audio: s.body()}, nil
}

// output counts open streams and written bytes.
type output struct {
	open, bytes atomic.Int64
}

func (o *output) Open(context.Context, models.AudioFormat) (audio.Stream, error) {
	o.open.Add(1)
	return &outStream{o: o}, nil
}

type outStream struct {
	o    *output
	once sync.Once
}

func (s *outStream) Write(p []byte) error { s.o.bytes.Add(int64(len(p))); return nil }
func (s *outStream) Drain() error         { s.once.Do(func() { s.o.open.Add(-1) }); return nil }
func (s *outStream) Abort() error         { return s.Drain() }

type next struct {
	mu  sync.Mutex
	ops []string
}

func (n *next) Control(_ context.Context, op models.MediaOperation) error {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.ops = append(n.ops, string(op))
	return nil
}
func (n *next) PlayURL(_ context.Context, u string) error {
	return n.Control(context.Background(), models.MediaOperation("url "+u))
}
func (n *next) Status(context.Context) (models.MediaStatus, error) {
	return models.MediaStatus{Playing: true, Title: "spotify"}, nil
}
func (n *next) got() []string {
	n.mu.Lock()
	defer n.mu.Unlock()
	return append([]string(nil), n.ops...)
}

func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	for deadline := time.Now().Add(3 * time.Second); time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
		if cond() {
			return
		}
	}
	t.Fatalf("timed out waiting for %s", what)
}

func TestPlayPauseStop(t *testing.T) {
	ctx := context.Background()
	src := &source{body: func() io.ReadCloser { return &endless{} }}
	out := &output{}
	nx := &next{}
	p := &Player{Source: src, Out: out, Next: nx}

	if err := p.PlaySearch(ctx, "кино группа крови"); err != nil {
		t.Fatal(err)
	}
	if got := src.reqs[0]; got.Query != "кино группа крови" {
		t.Errorf("request = %+v", got)
	}
	if ops := nx.got(); len(ops) != 1 || ops[0] != "pause" {
		t.Errorf("next player ops = %v, want it paused", ops)
	}
	eventually(t, "playback", func() bool { return out.bytes.Load() > 0 })
	// Real-time pacing: no more than the lead plus a chunk in 100 ms.
	time.Sleep(100 * time.Millisecond)
	if max := int64(media.Format.FrameSize(lead + 300*time.Millisecond)); out.bytes.Load() > max {
		t.Errorf("wrote %d bytes, more than real time allows (%d)", out.bytes.Load(), max)
	}
	st, _ := p.Status(ctx)
	if !st.Playing || st.Title != "Кино — Группа крови" {
		t.Errorf("status = %+v", st)
	}

	if err := p.Control(ctx, models.MediaPause); err != nil {
		t.Fatal(err)
	}
	if out.open.Load() != 0 {
		t.Error("audio device not released when Control(pause) returned")
	}
	if st, _ := p.Status(ctx); st.Playing {
		t.Error("still playing after pause")
	}
	p.Control(ctx, models.MediaToggle)
	eventually(t, "resume", func() bool { return out.open.Load() == 1 })

	if err := p.Control(ctx, models.MediaNext); err == nil {
		t.Error("next on a stream succeeded")
	}
	if err := p.Control(ctx, models.MediaStop); err != nil {
		t.Fatal(err)
	}
	if out.open.Load() != 0 {
		t.Error("audio device still open after stop")
	}
	// Without a stream, controls go to the other player.
	st, _ = p.Status(ctx)
	if st.Title != "spotify" {
		t.Errorf("status after stop = %+v", st)
	}
	p.Control(ctx, models.MediaPlay)
	if ops := nx.got(); ops[len(ops)-1] != "play" {
		t.Errorf("next player ops = %v", ops)
	}
}

func TestSpotifyGoesToNext(t *testing.T) {
	src := &source{body: func() io.ReadCloser { return &endless{} }}
	nx := &next{}
	p := &Player{Source: src, Out: &output{}, Next: nx}
	if err := p.PlayURL(context.Background(), "spotify:track:123"); err != nil {
		t.Fatal(err)
	}
	if len(src.reqs) != 0 || len(nx.got()) != 1 {
		t.Errorf("source got %v, next got %v", src.reqs, nx.got())
	}
}

func TestHoldDelaysStart(t *testing.T) {
	var busy atomic.Bool
	busy.Store(true)
	out := &output{}
	p := &Player{Source: &source{body: func() io.ReadCloser { return &endless{} }}, Out: out, Hold: busy.Load}
	defer p.Stop()
	if err := p.PlayURL(context.Background(), "https://youtu.be/x"); err != nil {
		t.Fatal(err)
	}
	time.Sleep(300 * time.Millisecond)
	if out.open.Load() != 0 {
		t.Fatal("played over the assistant")
	}
	busy.Store(false)
	eventually(t, "start after the reply", func() bool { return out.open.Load() == 1 })
}

func TestTrackEnds(t *testing.T) {
	out := &output{}
	pcm := make([]byte, media.Format.FrameSize(250*time.Millisecond))
	p := &Player{Source: &source{body: func() io.ReadCloser { return io.NopCloser(bytes.NewReader(pcm)) }}, Out: out}
	if err := p.PlayURL(context.Background(), "https://youtu.be/x"); err != nil {
		t.Fatal(err)
	}
	eventually(t, "end of track", func() bool { st, _ := p.Status(context.Background()); return !st.Playing && out.open.Load() == 0 })
	if out.bytes.Load() != int64(len(pcm)) {
		t.Errorf("played %d of %d bytes", out.bytes.Load(), len(pcm))
	}
}

func TestOpenError(t *testing.T) {
	p := &Player{Source: &source{err: media.ErrNotFound}, Out: &output{}}
	if err := p.PlaySearch(context.Background(), "x"); !errors.Is(err, media.ErrNotFound) {
		t.Errorf("err = %v", err)
	}
}
