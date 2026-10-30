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

package registry

import (
	"context"
	"sync"

	"github.com/yawaflua/marusya-gemini/internal/models"
	"github.com/yawaflua/marusya-gemini/internal/provider"
)

// Switch is a [provider.Provider] that delegates to another one, which can
// be replaced while the server runs. Sessions already open keep their
// provider; new sessions use the current one.
type Switch struct {
	mu sync.RWMutex
	p  provider.Provider
}

var (
	_ provider.Provider        = (*Switch)(nil)
	_ provider.Tuner           = (*Switch)(nil)
	_ provider.TunableReporter = (*Switch)(nil)
	_ provider.VADDetector     = (*Switch)(nil)
	_ provider.TextProvider    = (*Switch)(nil)
	_ provider.VoiceLister     = (*Switch)(nil)
)

// NewSwitch returns a switch delegating to p.
func NewSwitch(p provider.Provider) *Switch { return &Switch{p: p} }

// Current returns the provider in use.
func (s *Switch) Current() provider.Provider {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.p
}

// Set replaces the provider.
func (s *Switch) Set(p provider.Provider) {
	s.mu.Lock()
	s.p = p
	s.mu.Unlock()
}

// Name implements [provider.Provider].
func (s *Switch) Name() string { return s.Current().Name() }

// OutputFormat implements [provider.Provider].
func (s *Switch) OutputFormat() models.AudioFormat { return s.Current().OutputFormat() }

// Connect implements [provider.Provider].
func (s *Switch) Connect(ctx context.Context, opts provider.SessionOptions) (provider.Session, error) {
	return s.Current().Connect(ctx, opts)
}

// Tunable implements [provider.TunableReporter].
func (s *Switch) Tunable() bool {
	_, ok := provider.AsTuner(s.Current())
	return ok
}

// Tuning implements [provider.Tuner].
func (s *Switch) Tuning() provider.Tuning {
	if t, ok := provider.AsTuner(s.Current()); ok {
		return t.Tuning()
	}
	return provider.Tuning{}
}

// Tune implements [provider.Tuner].
func (s *Switch) Tune(tn provider.Tuning) {
	if t, ok := provider.AsTuner(s.Current()); ok {
		t.Tune(tn)
	}
}

// SupportsServerVAD implements [provider.VADDetector].
func (s *Switch) SupportsServerVAD() bool { return provider.SupportsServerVAD(s.Current()) }

// AcceptsText implements [provider.TextProvider].
func (s *Switch) AcceptsText() bool { return provider.AcceptsText(s.Current()) }

// Voices implements [provider.VoiceLister].
func (s *Switch) Voices() []string { return provider.Voices(s.Current()) }
