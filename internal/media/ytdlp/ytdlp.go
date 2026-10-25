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

// Package ytdlp implements [media.Source] with yt-dlp, which finds and
// downloads the audio of YouTube videos (and of many other sites, or plain
// stream URLs), and ffmpeg, which decodes it to PCM.
package ytdlp

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/yawaflua/marusya-gemini/internal/media"
)

// Source runs yt-dlp piped into ffmpeg for every track.
type Source struct {
	// YTDLP and FFmpeg are the binaries; defaults "yt-dlp" and "ffmpeg".
	YTDLP  string
	FFmpeg string
	// Args are extra yt-dlp arguments, e.g. "--js-runtimes node" or
	// "--cookies /data/cookies.txt".
	Args []string
	// StartTimeout bounds the search and the start of the download.
	// Defaults to 25 s, under the command timeout of speakers.
	StartTimeout time.Duration
	Logger       *slog.Logger
}

var _ media.Source = (*Source)(nil)

// Available reports whether both binaries are installed.
func (s *Source) Available() error {
	for _, bin := range []string{s.ytdlp(), s.ffmpeg()} {
		if _, err := exec.LookPath(bin); err != nil {
			return err
		}
	}
	return nil
}

func (s *Source) ytdlp() string  { return cmpOr(s.YTDLP, "yt-dlp") }
func (s *Source) ffmpeg() string { return cmpOr(s.FFmpeg, "ffmpeg") }

func cmpOr(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

// infoTemplate is printed by yt-dlp to fd 3 right before the download.
const infoTemplate = "before_dl:%(title)s\t%(duration)s"

// Open implements [media.Source]. The returned track keeps both processes
// running until it is read to the end or closed.
func (s *Source) Open(ctx context.Context, r media.Request) (*media.Track, error) {
	if err := r.Validate(); err != nil {
		return nil, err
	}
	target := r.URL
	if r.Query != "" {
		target = "ytsearch1:" + r.Query
	}
	// The processes outlive Open: they stop when the track is closed.
	pctx, cancel := context.WithCancel(context.WithoutCancel(ctx))

	infoR, infoW, err := os.Pipe()
	if err != nil {
		cancel()
		return nil, err
	}
	defer infoR.Close()

	args := []string{
		"--quiet", "--no-warnings", "--no-progress", "--no-playlist", "--no-cache-dir", "--no-simulate",
		"--format", "bestaudio/best",
		"--print-to-file", infoTemplate, "/dev/fd/3",
		"--output", "-",
	}
	args = append(append(args, s.Args...), "--", target)
	dl := exec.CommandContext(pctx, s.ytdlp(), args...)
	dl.ExtraFiles = []*os.File{infoW}
	dlErr := &tail{}
	dl.Stderr = dlErr

	dec := exec.CommandContext(pctx, s.ffmpeg(),
		"-hide_banner", "-nostdin", "-loglevel", "error",
		"-i", "pipe:0", "-vn",
		"-ac", strconv.Itoa(media.Format.Channels), "-ar", strconv.Itoa(media.Format.SampleRateHertz),
		"-f", "s16le", "pipe:1")
	decErr := &tail{}
	dec.Stderr = decErr

	pipeR, pipeW, err := os.Pipe()
	if err != nil {
		infoW.Close()
		cancel()
		return nil, err
	}
	dl.Stdout = pipeW
	dec.Stdin = pipeR
	out, err := dec.StdoutPipe()
	if err != nil {
		infoW.Close()
		pipeR.Close()
		pipeW.Close()
		cancel()
		return nil, err
	}
	if err := dl.Start(); err != nil {
		infoW.Close()
		pipeR.Close()
		pipeW.Close()
		cancel()
		return nil, fmt.Errorf("start yt-dlp: %w", err)
	}
	infoW.Close()
	pipeW.Close()
	if err := dec.Start(); err != nil {
		pipeR.Close()
		cancel()
		dl.Wait()
		return nil, fmt.Errorf("start ffmpeg: %w", err)
	}
	pipeR.Close()

	t := &track{cancel: cancel, dl: dl, dec: dec, out: out, dlErr: dlErr, decErr: decErr}
	t.wg.Add(1)
	go func() {
		defer t.wg.Done()
		t.dlWait = dl.Wait()
	}()

	// Wait for the metadata line, which yt-dlp prints once it has found the
	// video and is about to download it.
	type info struct {
		line string
		err  error
	}
	got := make(chan info, 1)
	go func() {
		line, err := bufio.NewReader(infoR).ReadString('\n')
		got <- info{strings.TrimRight(line, "\r\n"), err}
	}()
	timeout := s.StartTimeout
	if timeout <= 0 {
		timeout = 25 * time.Second
	}
	var line string
	select {
	case i := <-got:
		if i.err != nil && i.line == "" {
			t.Close()
			msg := dlErr.String()
			if r.Query != "" && msg == "" {
				return nil, fmt.Errorf("%w: %s", media.ErrNotFound, r.Query)
			}
			return nil, fmt.Errorf("yt-dlp %s: %s", r, cmpOr(msg, "no result"))
		}
		line = i.line
	case <-time.After(timeout):
		t.Close()
		return nil, fmt.Errorf("yt-dlp %s: no answer in %v", r, timeout)
	case <-ctx.Done():
		t.Close()
		return nil, ctx.Err()
	}
	title, dur, _ := strings.Cut(line, "\t")
	tr := &media.Track{Title: title, Audio: t}
	if f, err := strconv.ParseFloat(dur, 64); err == nil && f > 0 {
		tr.Duration = time.Duration(f * float64(time.Second))
	}
	if s.Logger != nil {
		s.Logger.Info("media: streaming", "request", r.String(), "title", tr.Title, "duration", tr.Duration)
		t.log = s.Logger
	}
	return tr, nil
}

// track is the PCM output of a running yt-dlp | ffmpeg pipeline.
type track struct {
	cancel        context.CancelFunc
	dl, dec       *exec.Cmd
	out           io.ReadCloser
	dlErr, decErr *tail
	log           *slog.Logger

	wg      sync.WaitGroup
	dlWait  error
	decWait error
	once    sync.Once
	closed  atomic.Bool
}

func (t *track) Read(p []byte) (int, error) {
	n, err := t.out.Read(p)
	if errors.Is(err, io.EOF) {
		// Decoding ended: report a failure if no audio came out at all.
		t.finish()
		if t.failed() {
			return n, fmt.Errorf("media: %s", cmpOr(t.decErr.String(), t.dlErr.String()))
		}
	}
	return n, err
}

func (t *track) failed() bool {
	return !t.closed.Load() && (t.dlWait != nil || t.decWait != nil)
}

// finish waits for both processes. yt-dlp has normally exited by the time
// ffmpeg ends; if ffmpeg failed, yt-dlp has nobody to write to and is
// stopped.
func (t *track) finish() {
	t.once.Do(func() {
		t.decWait = t.dec.Wait()
		t.cancel()
		t.wg.Wait()
	})
}

// Close stops both processes.
func (t *track) Close() error {
	t.closed.Store(true)
	t.cancel()
	t.finish()
	return nil
}

// tail keeps the last few hundred bytes written to it, for error messages.
type tail struct {
	mu  sync.Mutex
	buf []byte
}

func (t *tail) Write(p []byte) (int, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.buf = append(t.buf, p...)
	if len(t.buf) > 600 {
		t.buf = t.buf[len(t.buf)-600:]
	}
	return len(p), nil
}

func (t *tail) String() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return string(bytes.TrimSpace(t.buf))
}
