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

// Package wakeword defines the trigger that activates the assistant.
//
// A [Detector] may analyse microphone audio (a keyword spotter) or ignore it
// entirely (a hardware button, a signal). Several detectors can be combined
// with [Multi].
package wakeword

import (
	"context"
	"errors"
	"sync"
	"time"
)

// Detection is a single activation.
type Detection struct {
	// Source names the detector that fired, e.g. "exec" or "button".
	Source string
	// Keyword is the detected phrase, if known.
	Keyword string
	At      time.Time
}

// Detector produces [Detection] events.
type Detector interface {
	// Run blocks until ctx is done, reading microphone frames from audio and
	// writing detections to out. Detectors that do not need audio must still
	// tolerate frames being offered (they are never blocked on).
	Run(ctx context.Context, audio <-chan []byte, out chan<- Detection) error
	// WantsAudio reports whether the detector consumes microphone frames.
	WantsAudio() bool
}

// Multi runs several detectors concurrently and fans microphone frames out to
// those that want them.
type Multi []Detector

var _ Detector = Multi(nil)

// WantsAudio implements [Detector].
func (m Multi) WantsAudio() bool {
	for _, d := range m {
		if d.WantsAudio() {
			return true
		}
	}
	return false
}

// Run implements [Detector].
func (m Multi) Run(ctx context.Context, audio <-chan []byte, out chan<- Detection) error {
	if len(m) == 0 {
		return errors.New("wakeword: no detectors configured")
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	var (
		wg    sync.WaitGroup
		errMu sync.Mutex
		errs  []error
		taps  []chan []byte
	)
	for _, d := range m {
		var in chan []byte
		if d.WantsAudio() {
			in = make(chan []byte, 64)
			taps = append(taps, in)
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := d.Run(ctx, in, out); err != nil && ctx.Err() == nil {
				errMu.Lock()
				errs = append(errs, err)
				errMu.Unlock()
				cancel()
			}
		}()
	}

loop:
	for {
		select {
		case <-ctx.Done():
			break loop
		case f, ok := <-audio:
			if !ok {
				audio = nil
				continue
			}
			for _, t := range taps {
				select {
				case t <- f:
				default:
				}
			}
		}
	}
	wg.Wait()
	return errors.Join(errs...)
}
