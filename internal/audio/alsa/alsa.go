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

// Package alsa implements [audio.Capturer] and [audio.Player] by driving the
// alsa-utils arecord/aplay binaries.
//
// Using the command line tools instead of libasound keeps the agent a static
// pure-Go binary (CGO_ENABLED=0), which makes cross-compiling for the
// speaker's arm64 SoC trivial.
package alsa

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os/exec"
	"strconv"
	"sync"
	"time"

	"github.com/yawaflua/marusya-gemini/internal/audio"
	"github.com/yawaflua/marusya-gemini/internal/models"
)

// Config configures the ALSA backend.
type Config struct {
	// CaptureDevice and PlaybackDevice are ALSA PCM names, e.g. "default",
	// "plughw:0,0" or a dsnoop/dmix device from asound.conf.
	CaptureDevice  string
	PlaybackDevice string
	// ArecordPath and AplayPath override the binaries' locations.
	ArecordPath string
	AplayPath   string
	// FrameDuration is the size of captured frames. Defaults to 20 ms.
	FrameDuration time.Duration
	Logger        *slog.Logger
}

func (c *Config) setDefaults() {
	if c.CaptureDevice == "" {
		c.CaptureDevice = "default"
	}
	if c.PlaybackDevice == "" {
		c.PlaybackDevice = "default"
	}
	if c.ArecordPath == "" {
		c.ArecordPath = "arecord"
	}
	if c.AplayPath == "" {
		c.AplayPath = "aplay"
	}
	if c.FrameDuration == 0 {
		c.FrameDuration = 20 * time.Millisecond
	}
	if c.Logger == nil {
		c.Logger = slog.Default()
	}
}

func formatArgs(dev string, f models.AudioFormat) []string {
	return []string{
		"-q", "-D", dev, "-t", "raw", "-f", "S16_LE",
		"-r", strconv.Itoa(f.SampleRateHertz), "-c", strconv.Itoa(f.Channels),
	}
}

// Capture records from an ALSA device.
type Capture struct {
	cfg    Config
	format models.AudioFormat
}

var _ audio.Capturer = (*Capture)(nil)

// NewCapture creates a capturer producing audio in format f.
func NewCapture(cfg Config, f models.AudioFormat) *Capture {
	cfg.setDefaults()
	return &Capture{cfg: cfg, format: f}
}

// Format implements [audio.Capturer].
func (c *Capture) Format() models.AudioFormat { return c.format }

// Run implements [audio.Capturer]. The recorder is restarted if it exits
// unexpectedly.
func (c *Capture) Run(ctx context.Context, out chan<- []byte) error {
	backoff := time.Second
	for {
		err := c.record(ctx, out)
		if ctx.Err() != nil {
			return nil
		}
		c.cfg.Logger.Error("capture stopped, restarting", "err", err, "in", backoff)
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(backoff):
		}
		backoff = min(2*backoff, 30*time.Second)
	}
}

func (c *Capture) record(ctx context.Context, out chan<- []byte) error {
	cmd := exec.CommandContext(ctx, c.cfg.ArecordPath, formatArgs(c.cfg.CaptureDevice, c.format)...)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start %s: %w", c.cfg.ArecordPath, err)
	}
	defer cmd.Wait()

	size := c.format.FrameSize(c.cfg.FrameDuration)
	for {
		frame := make([]byte, size)
		if _, err := io.ReadFull(stdout, frame); err != nil {
			cmd.Process.Kill()
			return fmt.Errorf("read capture: %w", err)
		}
		select {
		case out <- frame:
		default: // consumer is behind; drop the frame rather than stall ALSA
		}
	}
}

// Player plays to an ALSA device.
type Player struct {
	cfg Config
}

var _ audio.Player = (*Player)(nil)

// NewPlayer creates a player.
func NewPlayer(cfg Config) *Player {
	cfg.setDefaults()
	return &Player{cfg: cfg}
}

// Open implements [audio.Player].
func (p *Player) Open(_ context.Context, f models.AudioFormat) (audio.Stream, error) {
	cmd := exec.Command(p.cfg.AplayPath, formatArgs(p.cfg.PlaybackDevice, f)...)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start %s: %w", p.cfg.AplayPath, err)
	}
	s := &stream{cmd: cmd, stdin: stdin, queue: make(chan []byte, 2048), done: make(chan struct{})}
	go s.loop()
	return s, nil
}

// ErrOverflow is returned when the playback queue is full.
var ErrOverflow = errors.New("alsa: playback queue overflow")

// errStreamClosed is returned by Write after Drain or Abort.
var errStreamClosed = errors.New("alsa: stream closed")

type stream struct {
	cmd   *exec.Cmd
	stdin io.WriteCloser
	queue chan []byte
	done  chan struct{}
	err   error

	mu     sync.Mutex
	closed bool
}

func (s *stream) loop() {
	defer close(s.done)
	var werr error
	for b := range s.queue {
		if werr != nil {
			continue // discard the rest after a failure or abort
		}
		_, werr = s.stdin.Write(b)
	}
	s.stdin.Close()
	err := s.cmd.Wait()
	s.err = errors.Join(werr, err)
}

func (s *stream) Write(pcm []byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return errStreamClosed
	}
	select {
	case s.queue <- pcm:
		return nil
	default:
		return ErrOverflow
	}
}

func (s *stream) closeQueue() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return false
	}
	s.closed = true
	close(s.queue)
	return true
}

func (s *stream) Drain() error {
	s.closeQueue()
	<-s.done
	return s.err
}

func (s *stream) Abort() error {
	s.closeQueue()
	s.cmd.Process.Kill() // harmless if the process already exited
	<-s.done
	return nil
}
