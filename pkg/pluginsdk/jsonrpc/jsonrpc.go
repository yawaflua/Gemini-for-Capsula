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

// Package jsonrpc is a symmetric JSON-RPC 2.0 connection over a byte stream
// with one message per line, as used between Marusya and its plugins. Both
// sides can send requests and notifications.
package jsonrpc

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strconv"
	"sync"
	"sync/atomic"
)

// MaxMessage bounds one line.
const MaxMessage = 16 << 20

// Standard error codes.
const (
	CodeParse          = -32700
	CodeInvalidRequest = -32600
	CodeMethodNotFound = -32601
	CodeInvalidParams  = -32602
	CodeInternal       = -32603
	// CodeServer is used for errors returned by handlers.
	CodeServer = -32000
)

// Error is a JSON-RPC error object. Handlers may return one to choose the
// code.
type Error struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

func (e *Error) Error() string { return e.Message }

// ErrMethodNotFound is returned by handlers for unknown methods.
func ErrMethodNotFound(method string) error {
	return &Error{Code: CodeMethodNotFound, Message: "method not found: " + method}
}

// ErrClosed is returned by calls on a closed connection.
var ErrClosed = errors.New("jsonrpc: connection closed")

// Handler serves incoming requests and notifications. For a notification
// the result is discarded. Handlers run in their own goroutines.
type Handler func(ctx context.Context, method string, params json.RawMessage) (any, error)

type message struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method,omitempty"`
	Params  json.RawMessage `json:"params,omitempty"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *Error          `json:"error,omitempty"`
}

// Conn is one end of a connection.
type Conn struct {
	r       *bufio.Reader
	handler Handler

	wmu sync.Mutex
	w   io.Writer

	nextID  atomic.Int64
	mu      sync.Mutex
	pending map[string]chan *message
	closed  chan struct{}
	once    sync.Once
	err     error
}

// New returns a connection reading from r and writing to w. Call
// [Conn.Run] to start serving.
func New(r io.Reader, w io.Writer, h Handler) *Conn {
	if h == nil {
		h = func(_ context.Context, method string, _ json.RawMessage) (any, error) {
			return nil, ErrMethodNotFound(method)
		}
	}
	return &Conn{
		r: bufio.NewReaderSize(r, 64<<10), w: w, handler: h,
		pending: map[string]chan *message{}, closed: make(chan struct{}),
	}
}

// Run reads messages until the stream ends or ctx is done, then closes the
// connection. It returns nil at the end of the stream.
func (c *Conn) Run(ctx context.Context) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	go func() {
		select {
		case <-ctx.Done():
			c.close(ctx.Err())
		case <-c.closed:
		}
	}()
	var wg sync.WaitGroup
	defer wg.Wait()
	for {
		line, err := c.readLine()
		if err != nil {
			if errors.Is(err, io.EOF) {
				err = nil
			}
			c.close(err)
			return err
		}
		if len(line) == 0 {
			continue
		}
		var m message
		if err := json.Unmarshal(line, &m); err != nil {
			c.reply(nil, nil, &Error{Code: CodeParse, Message: err.Error()})
			continue
		}
		switch {
		case m.Method != "":
			wg.Add(1)
			go func() {
				defer wg.Done()
				c.serve(ctx, &m)
			}()
		case len(m.ID) > 0:
			c.mu.Lock()
			ch := c.pending[string(m.ID)]
			delete(c.pending, string(m.ID))
			c.mu.Unlock()
			if ch != nil {
				ch <- &m
			}
		}
	}
}

func (c *Conn) readLine() ([]byte, error) {
	var buf []byte
	for {
		chunk, isPrefix, err := c.r.ReadLine()
		if err != nil {
			return nil, err
		}
		buf = append(buf, chunk...)
		if len(buf) > MaxMessage {
			return nil, fmt.Errorf("jsonrpc: message longer than %d bytes", MaxMessage)
		}
		if !isPrefix {
			return buf, nil
		}
	}
}

func (c *Conn) serve(ctx context.Context, m *message) {
	res, err := c.handler(ctx, m.Method, m.Params)
	if len(m.ID) == 0 {
		return // notification
	}
	if err != nil {
		var e *Error
		if !errors.As(err, &e) {
			e = &Error{Code: CodeServer, Message: err.Error()}
		}
		c.reply(m.ID, nil, e)
		return
	}
	b, err := json.Marshal(res)
	if err != nil {
		c.reply(m.ID, nil, &Error{Code: CodeInternal, Message: err.Error()})
		return
	}
	c.reply(m.ID, b, nil)
}

func (c *Conn) reply(id json.RawMessage, result json.RawMessage, e *Error) {
	if id == nil {
		id = json.RawMessage("null")
	}
	if e == nil && result == nil {
		result = json.RawMessage("null")
	}
	c.write(&message{JSONRPC: "2.0", ID: id, Result: result, Error: e})
}

func (c *Conn) write(m *message) error {
	b, err := json.Marshal(m)
	if err != nil {
		return err
	}
	c.wmu.Lock()
	defer c.wmu.Unlock()
	select {
	case <-c.closed:
		return ErrClosed
	default:
	}
	_, err = c.w.Write(append(b, '\n'))
	return err
}

// Call sends a request and decodes its result into result (if not nil).
func (c *Conn) Call(ctx context.Context, method string, params, result any) error {
	p, err := json.Marshal(params)
	if err != nil {
		return err
	}
	id := json.RawMessage(strconv.FormatInt(c.nextID.Add(1), 10))
	ch := make(chan *message, 1)
	c.mu.Lock()
	c.pending[string(id)] = ch
	c.mu.Unlock()
	defer func() {
		c.mu.Lock()
		delete(c.pending, string(id))
		c.mu.Unlock()
	}()
	if err := c.write(&message{JSONRPC: "2.0", ID: id, Method: method, Params: p}); err != nil {
		return err
	}
	select {
	case m := <-ch:
		if m.Error != nil {
			return m.Error
		}
		if result != nil && len(m.Result) > 0 {
			return json.Unmarshal(m.Result, result)
		}
		return nil
	case <-ctx.Done():
		return ctx.Err()
	case <-c.closed:
		return ErrClosed
	}
}

// Notify sends a notification.
func (c *Conn) Notify(method string, params any) error {
	p, err := json.Marshal(params)
	if err != nil {
		return err
	}
	return c.write(&message{JSONRPC: "2.0", Method: method, Params: p})
}

// Done is closed when the connection ends.
func (c *Conn) Done() <-chan struct{} { return c.closed }

// Err reports why the connection ended.
func (c *Conn) Err() error {
	<-c.closed
	return c.err
}

func (c *Conn) close(err error) {
	c.once.Do(func() {
		c.err = err
		c.wmu.Lock()
		close(c.closed)
		c.wmu.Unlock()
	})
}
