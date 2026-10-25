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

package logging

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/yawaflua/marusya-gemini/internal/models"
)

// Forwarder is a [slog.Handler] that passes every record to the next
// handler and additionally queues records at or above a level for delivery
// to the server. When the queue is full new records are dropped, so logging
// never blocks.
type Forwarder struct {
	next  slog.Handler
	min   slog.Level
	ch    chan models.Log
	attrs []slog.Attr
	group string
}

// NewForwarder wraps next, queuing up to size records at level min or above.
func NewForwarder(next slog.Handler, min slog.Level, size int) *Forwarder {
	return &Forwarder{next: next, min: min, ch: make(chan models.Log, size)}
}

// Records returns the queue of records to send.
func (f *Forwarder) Records() <-chan models.Log { return f.ch }

// Enabled implements [slog.Handler].
func (f *Forwarder) Enabled(ctx context.Context, l slog.Level) bool {
	return l >= f.min || f.next.Enabled(ctx, l)
}

// Handle implements [slog.Handler].
func (f *Forwarder) Handle(ctx context.Context, r slog.Record) error {
	if r.Level >= f.min {
		rec := models.Log{Time: r.Time, Level: r.Level.String(), Message: r.Message, Attrs: map[string]string{}}
		add := func(a slog.Attr) bool {
			k := a.Key
			if f.group != "" {
				k = f.group + "." + k
			}
			rec.Attrs[k] = fmt.Sprint(a.Value.Resolve().Any())
			return true
		}
		for _, a := range f.attrs {
			add(a)
		}
		r.Attrs(add)
		select {
		case f.ch <- rec:
		default:
		}
	}
	if f.next.Enabled(ctx, r.Level) {
		return f.next.Handle(ctx, r)
	}
	return nil
}

// WithAttrs implements [slog.Handler].
func (f *Forwarder) WithAttrs(as []slog.Attr) slog.Handler {
	c := *f
	c.next = f.next.WithAttrs(as)
	c.attrs = append(append([]slog.Attr(nil), f.attrs...), as...)
	return &c
}

// WithGroup implements [slog.Handler].
func (f *Forwarder) WithGroup(name string) slog.Handler {
	c := *f
	c.next = f.next.WithGroup(name)
	if c.group != "" {
		name = c.group + "." + name
	}
	c.group = name
	return &c
}
