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

// Package signal implements a [wakeword.Detector] triggered by SIGUSR1.
//
// It is handy for debugging and for integrating external triggers:
//
//	kill -USR1 $(pidof marusya-agent)
package signal

import (
	"context"
	"os"
	ossignal "os/signal"
	"syscall"
	"time"

	"github.com/yawaflua/marusya-gemini/internal/wakeword"
)

// Detector fires on SIGUSR1.
type Detector struct{}

var _ wakeword.Detector = Detector{}

// WantsAudio implements [wakeword.Detector].
func (Detector) WantsAudio() bool { return false }

// Run implements [wakeword.Detector].
func (Detector) Run(ctx context.Context, _ <-chan []byte, out chan<- wakeword.Detection) error {
	ch := make(chan os.Signal, 1)
	ossignal.Notify(ch, syscall.SIGUSR1)
	defer ossignal.Stop(ch)
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ch:
			select {
			case out <- wakeword.Detection{Source: "signal", At: time.Now()}:
			default:
			}
		}
	}
}
