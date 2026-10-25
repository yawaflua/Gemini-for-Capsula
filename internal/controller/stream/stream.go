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

// Package stream implements [controller.Media] for sound streamed by
// marusya-server (YouTube and other sites, internet radio): the server finds
// and decodes it (see package media), the speaker only plays PCM. Spotify
// URIs and track skipping are left to another player, e.g. playerctl.
package stream

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/url"
	"sync"
	"sync/atomic"
	"time"

	"github.com/yawaflua/marusya-gemini/internal/audio"
	"github.com/yawaflua/marusya-gemini/internal/controller"
	"github.com/yawaflua/marusya-gemini/internal/media"
	"github.com/yawaflua/marusya-gemini/internal/models"
)

// Player plays [media.Source] tracks.
type Player struct {
	Source media.Source
	Out    audio.Player
	// Next handles everything that is not an http(s) stream (spotify:
	// URIs), next/previous, and is paused when a stream starts. May be nil.
	Next controller.Media
	// Hold reports whether the speaker is busy with the assistant: a new
	// stream waits until it is not, so it does not play over the reply.
	Hold   func() bool
	Logger *slog.Logger

	mu  sync.Mutex
	cur *playback
}

var _ controller.MediaSearcher = (*Player)(nil)

// lead is how far ahead of real time audio is written to the device.
const lead = 400 * time.Millisecond

// openTimeout bounds finding a track (the server search takes a few
// seconds); it stays under the server command timeout of 30 s.
const openTimeout = 28 * time.Second

// PlayURL implements [controller.Media].
func (p *Player) PlayURL(ctx context.Context, rawURL string) error {
	if u, err := url.Parse(rawURL); p.Next != nil && (err != nil || (u.Scheme != "http" && u.Scheme != "https")) {
		p.Stop()
		return p.Next.PlayURL(ctx, rawURL)
	}
	return p.play(ctx, media.Request{URL: rawURL})
}

// PlaySearch implements [controller.MediaSearcher].
func (p *Player) PlaySearch(ctx context.Context, query string) error {
	return p.play(ctx, media.Request{Query: query})
}

func (p *Player) play(ctx context.Context, r media.Request) error {
	if err := r.Validate(); err != nil {
		return err
	}
	octx, cancel := context.WithTimeout(ctx, openTimeout)
	defer cancel()
	// The track outlives the command: open it with an independent context
	// and close it when playback ends.
	tctx, stop := context.WithCancel(context.WithoutCancel(ctx))
	type opened struct {
		t   *media.Track
		err error
	}
	ch := make(chan opened, 1)
	go func() {
		t, err := p.Source.Open(tctx, r)
		ch <- opened{t, err}
	}()
	var t *media.Track
	select {
	case o := <-ch:
		if o.err != nil {
			stop()
			return o.err
		}
		t = o.t
	case <-octx.Done():
		stop()
		return fmt.Errorf("%s: %w", r, octx.Err())
	}

	p.Stop()
	p.pauseNext(ctx)
	pb := &playback{title: t.Title, track: t, stop: stop, done: make(chan struct{}), wake: make(chan struct{}, 1)}
	p.mu.Lock()
	p.cur = pb
	p.mu.Unlock()
	p.log().Info("stream: playing", "request", r.String(), "title", t.Title, "duration", t.Duration)
	go p.run(tctx, pb)
	return nil
}

func (p *Player) pauseNext(ctx context.Context) {
	if p.Next == nil {
		return
	}
	if st, err := p.Next.Status(ctx); err == nil && st.Playing {
		p.Next.Control(ctx, models.MediaPause)
	}
}

// Stop ends the current stream, if any.
func (p *Player) Stop() {
	p.mu.Lock()
	pb := p.cur
	p.cur = nil
	p.mu.Unlock()
	if pb != nil {
		pb.stop()
		pb.track.Audio.Close()
		<-pb.done
	}
}

// current returns the active stream, or nil.
func (p *Player) current() *playback {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.cur
}

// Control implements [controller.Media]. Without a stream, it is passed to
// Next.
func (p *Player) Control(ctx context.Context, op models.MediaOperation) error {
	pb := p.current()
	if pb == nil {
		if p.Next == nil {
			return errors.New("nothing is playing")
		}
		return p.Next.Control(ctx, op)
	}
	switch op {
	case models.MediaPlay:
		pb.setPaused(false)
	case models.MediaPause:
		pb.setPaused(true)
		pb.waitReleased(ctx)
	case models.MediaToggle:
		pb.setPaused(!pb.isPaused())
		if pb.isPaused() {
			pb.waitReleased(ctx)
		}
	case models.MediaStop:
		p.Stop()
	default:
		return fmt.Errorf("%s is not possible for %q: ask for another song instead", op, pb.title)
	}
	return nil
}

// Status implements [controller.Media].
func (p *Player) Status(ctx context.Context) (models.MediaStatus, error) {
	if pb := p.current(); pb != nil {
		return models.MediaStatus{Playing: !pb.isPaused(), Title: pb.title}, nil
	}
	if p.Next == nil {
		return models.MediaStatus{}, nil
	}
	return p.Next.Status(ctx)
}

func (p *Player) log() *slog.Logger {
	if p.Logger != nil {
		return p.Logger
	}
	return slog.Default()
}

// playback is one playing stream.
type playback struct {
	title string
	track *media.Track
	stop  context.CancelFunc
	done  chan struct{}
	wake  chan struct{} // pause state changed

	mu     sync.Mutex
	paused bool
	// output is set while the audio device is open.
	output atomic.Bool
}

// waitReleased waits briefly for the device to be released after a pause:
// the assistant's sounds may need it right away (without dmix only one
// stream can play at a time).
func (pb *playback) waitReleased(ctx context.Context) {
	for deadline := time.Now().Add(500 * time.Millisecond); pb.output.Load() && time.Now().Before(deadline) && ctx.Err() == nil; {
		time.Sleep(5 * time.Millisecond)
	}
}

func (pb *playback) isPaused() bool {
	pb.mu.Lock()
	defer pb.mu.Unlock()
	return pb.paused
}

func (pb *playback) setPaused(v bool) {
	pb.mu.Lock()
	pb.paused = v
	pb.mu.Unlock()
	select {
	case pb.wake <- struct{}{}:
	default:
	}
}

// run copies the track to the device in real time. While paused, the
// device is released (the assistant may need it) and the track is not
// read, which holds back the server.
func (p *Player) run(ctx context.Context, pb *playback) {
	defer close(pb.done)
	defer pb.track.Audio.Close()
	defer p.clear(pb)

	f := media.Format
	chunk := make([]byte, f.FrameSize(100*time.Millisecond))
	var (
		out     audio.Stream
		start   time.Time
		written time.Duration
	)
	release := func() {
		if out != nil {
			out.Abort()
			out = nil
			pb.output.Store(false)
		}
	}
	defer release()
	sleep := func(d time.Duration) bool {
		t := time.NewTimer(d)
		defer t.Stop()
		select {
		case <-ctx.Done():
			return false
		case <-pb.wake:
			return true
		case <-t.C:
			return true
		}
	}

	// Let the assistant finish its reply first.
	for p.Hold != nil && p.Hold() && !pb.isPaused() {
		if !sleep(200 * time.Millisecond) {
			return
		}
	}
	for {
		if pb.isPaused() {
			release()
			if !sleep(time.Hour) {
				return
			}
			continue
		}
		n, err := io.ReadFull(pb.track.Audio, chunk)
		if n > 0 {
			if out == nil {
				if out, err = p.Out.Open(ctx, f); err != nil {
					p.log().Warn("stream: open audio output", "err", err)
					return
				}
				pb.output.Store(true)
				start, written = time.Now(), 0
			}
			buf := make([]byte, n) // the queue keeps it
			copy(buf, chunk[:n])
			if werr := out.Write(buf); werr != nil {
				p.log().Warn("stream: write", "err", werr)
				release()
				continue
			}
			written += f.Duration(n)
			if ahead := written - time.Since(start); ahead > lead {
				if !sleep(ahead - lead/2) {
					return
				}
			} else if ahead < -time.Second {
				// The network fell behind; restart the clock.
				start, written = time.Now(), 0
			}
		}
		switch {
		case err == nil:
		case errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF):
			if out != nil {
				out.Drain()
				out = nil
				pb.output.Store(false)
			}
			p.log().Info("stream: finished", "title", pb.title)
			return
		default:
			if ctx.Err() == nil {
				p.log().Warn("stream: interrupted", "title", pb.title, "err", err)
			}
			return
		}
	}
}

// clear forgets pb if it is still the current stream.
func (p *Player) clear(pb *playback) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.cur == pb {
		p.cur = nil
	}
}
