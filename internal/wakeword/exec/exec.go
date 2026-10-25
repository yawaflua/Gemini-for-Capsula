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

// Package exec implements a [wakeword.Detector] backed by an external
// keyword-spotting process.
//
// The process receives raw microphone PCM (LINEAR16, 16 kHz, mono) on stdin
// and must print one line to stdout for every detection, e.g. the detected
// keyword. This makes any engine usable — openWakeWord, Porcupine, Vosk,
// microWakeWord or a vendor binary — without linking it into the agent.
package exec

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	osexec "os/exec"
	"strings"
	"time"

	"github.com/yawaflua/marusya-gemini/internal/wakeword"
)

// Detector runs an external keyword spotter.
type Detector struct {
	// Command is the program and its arguments.
	Command []string
	// Cooldown suppresses repeated detections of the same utterance.
	Cooldown time.Duration
	Logger   *slog.Logger
}

var _ wakeword.Detector = (*Detector)(nil)

// WantsAudio implements [wakeword.Detector].
func (d *Detector) WantsAudio() bool { return true }

// Run implements [wakeword.Detector]. The process is restarted if it exits.
func (d *Detector) Run(ctx context.Context, audio <-chan []byte, out chan<- wakeword.Detection) error {
	if len(d.Command) == 0 {
		return fmt.Errorf("wakeword/exec: empty command")
	}
	log := d.Logger
	if log == nil {
		log = slog.Default()
	}
	for {
		err := d.runOnce(ctx, audio, out)
		if ctx.Err() != nil {
			return nil
		}
		log.Error("wake word process exited, restarting", "cmd", d.Command[0], "err", err)
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(2 * time.Second):
		}
	}
}

func (d *Detector) runOnce(ctx context.Context, audio <-chan []byte, out chan<- wakeword.Detection) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	cmd := osexec.CommandContext(ctx, d.Command[0], d.Command[1:]...)
	cmd.Stderr = os.Stderr
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	if err := cmd.Start(); err != nil {
		return err
	}

	go feed(ctx, stdin, audio)

	var last time.Time
	sc := bufio.NewScanner(stdout)
	for sc.Scan() {
		kw := strings.TrimSpace(sc.Text())
		if kw == "" {
			continue
		}
		now := time.Now()
		if now.Sub(last) < d.Cooldown {
			continue
		}
		last = now
		select {
		case out <- wakeword.Detection{Source: "exec", Keyword: kw, At: now}:
		default:
		}
	}
	cancel()
	return cmd.Wait()
}

func feed(ctx context.Context, w io.WriteCloser, audio <-chan []byte) {
	defer w.Close()
	for {
		select {
		case <-ctx.Done():
			return
		case f, ok := <-audio:
			if !ok {
				return
			}
			if _, err := w.Write(f); err != nil {
				return
			}
		}
	}
}
