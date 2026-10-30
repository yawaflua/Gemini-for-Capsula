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

// Package pluginsdk writes Marusya plugins in Go.
//
// A plugin is a program in its own directory next to a plugin.json manifest.
// Marusya starts it and talks JSON-RPC 2.0 over its stdin and stdout, one
// message per line; whatever it writes to stderr ends up in the log. The
// same plugin can run on the server, on the speaker, or on both, as said by
// the manifest. The types in this package describe the protocol for plugins
// written in other languages too.
//
//	p := pluginsdk.New()
//	p.Tool(pluginsdk.ToolSpec{
//		Name:        "coin_flip",
//		Description: "Flips a coin and returns heads or tails.",
//	}, func(ctx context.Context, c pluginsdk.Call) (any, error) {
//		return map[string]any{"side": "heads"}, nil
//	})
//	log.Fatal(p.Run())
package pluginsdk

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"sync"
	"syscall"

	"github.com/yawaflua/marusya-gemini/pkg/pluginsdk/jsonrpc"
)

// Call is a tool call.
type Call struct {
	Name   string
	Args   map[string]any
	Device Device
}

// String returns a string argument.
func (c Call) String(key string) string {
	s, _ := c.Args[key].(string)
	return s
}

// Int returns a numeric argument, or def.
func (c Call) Int(key string, def int) int {
	if f, ok := c.Args[key].(float64); ok {
		return int(f)
	}
	return def
}

// Bool returns a boolean argument.
func (c Call) Bool(key string) bool {
	b, _ := c.Args[key].(bool)
	return b
}

// ToolFunc runs a tool. The result is given to the model, so it should be a
// small JSON object (a map or a struct).
type ToolFunc func(ctx context.Context, c Call) (any, error)

// Init is what the host says about itself in [MethodInitialize].
type Init = InitializeParams

// Plugin is a plugin process. Register the tools and handlers, then call
// [Plugin.Run].
type Plugin struct {
	mu       sync.Mutex
	tools    []ToolSpec
	handlers map[string]ToolFunc
	events   []string
	onEvent  func(context.Context, Event)
	onMsg    func(context.Context, Message)
	onInit   func(context.Context, Init) error
	init     Init

	conn *jsonrpc.Conn
}

// New returns an empty plugin.
func New() *Plugin { return &Plugin{handlers: map[string]ToolFunc{}} }

// Tool registers a function for the language model.
func (p *Plugin) Tool(spec ToolSpec, f ToolFunc) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.tools = append(p.tools, spec)
	p.handlers[spec.Name] = f
}

// OnInit is called when the host initializes the plugin, before any other
// call. An error stops the plugin.
func (p *Plugin) OnInit(f func(ctx context.Context, in Init) error) { p.onInit = f }

// OnEvent subscribes to events of the given kinds (see the Event constants;
// "*" is every kind).
func (p *Plugin) OnEvent(f func(ctx context.Context, e Event), kinds ...string) {
	p.onEvent = f
	p.events = kinds
}

// OnMessage receives data sent by the other half of the plugin.
func (p *Plugin) OnMessage(f func(ctx context.Context, m Message)) { p.onMsg = f }

// Host returns what the host said when it initialized the plugin.
func (p *Plugin) Host() Init {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.init
}

// Config decodes the configuration from the manifest into v.
func (p *Plugin) Config(v any) error {
	c := p.Host().Config
	if len(c) == 0 {
		return nil
	}
	return json.Unmarshal(c, v)
}

// Run serves the host over stdin and stdout until the host closes them or
// asks the plugin to shut down.
func (p *Plugin) Run() error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	return p.Serve(ctx, os.Stdin, os.Stdout)
}

// Serve is [Plugin.Run] over any streams.
func (p *Plugin) Serve(ctx context.Context, r io.Reader, w io.Writer) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	p.mu.Lock()
	p.conn = jsonrpc.New(r, w, func(ctx context.Context, method string, params json.RawMessage) (any, error) {
		return p.handle(ctx, cancel, method, params)
	})
	conn := p.conn
	p.mu.Unlock()
	err := conn.Run(ctx)
	if errors.Is(err, context.Canceled) {
		return nil
	}
	return err
}

func (p *Plugin) handle(ctx context.Context, stop context.CancelFunc, method string, params json.RawMessage) (any, error) {
	switch method {
	case MethodInitialize:
		var in Init
		if err := json.Unmarshal(params, &in); err != nil {
			return nil, &jsonrpc.Error{Code: jsonrpc.CodeInvalidParams, Message: err.Error()}
		}
		p.mu.Lock()
		p.init = in
		p.mu.Unlock()
		if p.onInit != nil {
			if err := p.onInit(ctx, in); err != nil {
				return nil, err
			}
		}
		p.mu.Lock()
		defer p.mu.Unlock()
		return InitializeResult{Protocol: ProtocolVersion, Tools: p.tools, Events: p.events}, nil

	case MethodToolCall:
		var c ToolCallParams
		if err := json.Unmarshal(params, &c); err != nil {
			return nil, &jsonrpc.Error{Code: jsonrpc.CodeInvalidParams, Message: err.Error()}
		}
		p.mu.Lock()
		f := p.handlers[c.Name]
		p.mu.Unlock()
		if f == nil {
			return nil, fmt.Errorf("unknown tool %q", c.Name)
		}
		if c.Args == nil {
			c.Args = map[string]any{}
		}
		return f(ctx, Call(c))

	case MethodEvent:
		if p.onEvent != nil {
			var e Event
			if json.Unmarshal(params, &e) == nil {
				p.onEvent(ctx, e)
			}
		}
		return nil, nil

	case MethodMessage:
		if p.onMsg != nil {
			var m Message
			if json.Unmarshal(params, &m) == nil {
				p.onMsg(ctx, m)
			}
		}
		return nil, nil

	case MethodShutdown:
		// Answer first, then stop reading.
		go stop()
		return nil, nil
	}
	return nil, jsonrpc.ErrMethodNotFound(method)
}

func (p *Plugin) rpc() (*jsonrpc.Conn, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.conn == nil {
		return nil, errors.New("pluginsdk: the plugin is not running")
	}
	return p.conn, nil
}

// Command runs an action on a speaker, e.g. "volume.set" with
// {"level": 30}. device may be empty on the speaker itself.
func (p *Plugin) Command(ctx context.Context, device, action string, args map[string]any) (map[string]any, error) {
	c, err := p.rpc()
	if err != nil {
		return nil, err
	}
	var out map[string]any
	err = c.Call(ctx, MethodCommand, CommandParams{Device: device, Action: action, Args: args}, &out)
	return out, err
}

// Say speaks text on a speaker; with listen it returns the spoken answer.
// Server only.
func (p *Plugin) Say(ctx context.Context, device, text string, listen bool) (string, error) {
	c, err := p.rpc()
	if err != nil {
		return "", err
	}
	var out SayResult
	err = c.Call(ctx, MethodSay, SayParams{Device: device, Text: text, Listen: listen}, &out)
	return out.Answer, err
}

// Devices lists the connected speakers. Server only.
func (p *Plugin) Devices(ctx context.Context) ([]Device, error) {
	c, err := p.rpc()
	if err != nil {
		return nil, err
	}
	var out DevicesResult
	err = c.Call(ctx, MethodDevices, struct{}{}, &out)
	return out.Devices, err
}

// Send sends data, marshaled to JSON, to the other half of the plugin. On
// the server device chooses the speaker; empty sends to every speaker.
func (p *Plugin) Send(ctx context.Context, device string, data any) error {
	c, err := p.rpc()
	if err != nil {
		return err
	}
	b, err := json.Marshal(data)
	if err != nil {
		return err
	}
	return c.Call(ctx, MethodSend, SendParams{Device: device, Data: b}, nil)
}

// Log writes to the host log.
func (p *Plugin) Log(level, format string, args ...any) {
	c, err := p.rpc()
	if err != nil {
		fmt.Fprintf(os.Stderr, format+"\n", args...)
		return
	}
	c.Notify(MethodLog, LogParams{Level: level, Message: fmt.Sprintf(format, args...)})
}
