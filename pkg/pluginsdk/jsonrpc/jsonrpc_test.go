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

package jsonrpc

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"testing"
	"time"
)

// pair connects two Conns with pipes.
func pair(t *testing.T, ha, hb Handler) (*Conn, *Conn) {
	ar, bw := io.Pipe()
	br, aw := io.Pipe()
	a, b := New(ar, aw, ha), New(br, bw, hb)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(func() {
		cancel()
		aw.Close()
		bw.Close()
	})
	go a.Run(ctx)
	go b.Run(ctx)
	return a, b
}

func TestCallsBothWays(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var b *Conn
	a, b := pair(t,
		func(ctx context.Context, method string, p json.RawMessage) (any, error) {
			if method != "add" {
				return nil, ErrMethodNotFound(method)
			}
			var xs []int
			json.Unmarshal(p, &xs)
			return xs[0] + xs[1], nil
		},
		func(ctx context.Context, method string, p json.RawMessage) (any, error) {
			switch method {
			case "double_via_add":
				var x int
				json.Unmarshal(p, &x)
				var sum int
				// A handler may call back into the other side.
				err := b.Call(ctx, "add", []int{x, x}, &sum)
				return sum, err
			case "fail":
				return nil, errors.New("boom")
			}
			return nil, ErrMethodNotFound(method)
		})

	var got int
	if err := a.Call(ctx, "double_via_add", 21, &got); err != nil || got != 42 {
		t.Fatalf("double_via_add = %d, %v", got, err)
	}
	err := a.Call(ctx, "fail", nil, nil)
	var e *Error
	if !errors.As(err, &e) || e.Code != CodeServer || e.Message != "boom" {
		t.Fatalf("fail = %v", err)
	}
	err = a.Call(ctx, "nope", nil, nil)
	if !errors.As(err, &e) || e.Code != CodeMethodNotFound {
		t.Fatalf("nope = %v", err)
	}
}

func TestNotify(t *testing.T) {
	got := make(chan string, 1)
	a, _ := pair(t, nil, func(_ context.Context, method string, p json.RawMessage) (any, error) {
		got <- method + " " + string(p)
		return nil, nil
	})
	a.Notify("log", map[string]string{"m": "hi"})
	select {
	case s := <-got:
		if s != `log {"m":"hi"}` {
			t.Fatalf("got %q", s)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no notification")
	}
}

func TestEndOfStream(t *testing.T) {
	r, w := io.Pipe()
	c := New(strings.NewReader(`{"jsonrpc":"2.0","method":"x"}`+"\n"), w, nil)
	go io.Copy(io.Discard, r)
	if err := c.Run(context.Background()); err != nil {
		t.Fatalf("Run = %v", err)
	}
	if err := c.Call(context.Background(), "x", nil, nil); !errors.Is(err, ErrClosed) {
		t.Fatalf("call after close = %v", err)
	}
}

func TestParseErrorIsAnswered(t *testing.T) {
	r, w := io.Pipe()
	c := New(strings.NewReader("not json\n"), w, nil)
	go c.Run(context.Background())
	line := make([]byte, 200)
	n, _ := r.Read(line)
	if !strings.Contains(string(line[:n]), `"code":-32700`) {
		t.Fatalf("reply = %s", line[:n])
	}
}
