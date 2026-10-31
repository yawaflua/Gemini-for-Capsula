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
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
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

	instructions string
	providers    []ProviderSpec
	provHandlers map[string]ProviderHandler
	speech       []SpeechSpec
	speechFuncs  map[string]SpeechHandler
	httpSpec     *HTTPSpec
	httpHandler  http.Handler

	conn *jsonrpc.Conn
}

// New returns an empty plugin.
func New() *Plugin {
	return &Plugin{
		handlers: map[string]ToolFunc{}, provHandlers: map[string]ProviderHandler{},
		speechFuncs: map[string]SpeechHandler{},
	}
}

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
		return InitializeResult{
			Protocol: ProtocolVersion, Tools: p.tools, Events: p.events,
			Instructions: p.instructions, Providers: p.providers, Speech: p.speech, HTTP: p.httpSpec,
		}, nil

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

	case MethodProviderTurn:
		var tp ProviderTurnParams
		if err := json.Unmarshal(params, &tp); err != nil {
			return nil, &jsonrpc.Error{Code: jsonrpc.CodeInvalidParams, Message: err.Error()}
		}
		return p.providerTurn(ctx, tp)

	case MethodProviderClose:
		var cp ProviderCloseParams
		if json.Unmarshal(params, &cp) == nil {
			p.mu.Lock()
			h := p.provHandlers[cp.Provider]
			p.mu.Unlock()
			if h.Close != nil {
				h.Close(cp.Session)
			}
		}
		return nil, nil

	case MethodSynthesize:
		var sp SynthesizeParams
		if err := json.Unmarshal(params, &sp); err != nil {
			return nil, &jsonrpc.Error{Code: jsonrpc.CodeInvalidParams, Message: err.Error()}
		}
		p.mu.Lock()
		h := p.speechFuncs[sp.Engine]
		p.mu.Unlock()
		if h.Synthesize == nil {
			return nil, fmt.Errorf("engine %q does not synthesize speech", sp.Engine)
		}
		audio, err := h.Synthesize(ctx, sp)
		return SynthesizeResult{Audio: audio}, err

	case MethodTranscribe:
		var tp TranscribeParams
		if err := json.Unmarshal(params, &tp); err != nil {
			return nil, &jsonrpc.Error{Code: jsonrpc.CodeInvalidParams, Message: err.Error()}
		}
		p.mu.Lock()
		h := p.speechFuncs[tp.Engine]
		p.mu.Unlock()
		if h.Transcribe == nil {
			return nil, fmt.Errorf("engine %q does not recognize speech", tp.Engine)
		}
		text, err := h.Transcribe(ctx, tp)
		return TranscribeResult{Text: text}, err

	case MethodHTTP:
		var hr HTTPRequest
		if err := json.Unmarshal(params, &hr); err != nil {
			return nil, &jsonrpc.Error{Code: jsonrpc.CodeInvalidParams, Message: err.Error()}
		}
		p.mu.Lock()
		h := p.httpHandler
		p.mu.Unlock()
		if h == nil {
			return HTTPResponse{Status: http.StatusNotFound}, nil
		}
		return serveHTTP(ctx, h, hr), nil

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

// Request makes a request to the assistant on a speaker as if the user had
// said text; the answer is spoken there. device may be empty for the only
// speaker. Server only.
func (p *Plugin) Request(ctx context.Context, device, text string) error {
	c, err := p.rpc()
	if err != nil {
		return err
	}
	return c.Call(ctx, MethodRequest, RequestParams{Device: device, Text: text}, nil)
}

// Instructions sets what the plugin adds to the system instruction of the
// model, e.g. "Отвечай как пират". Before [Plugin.Run] it is the initial
// value; later it replaces it on the host. Server only.
func (p *Plugin) Instructions(ctx context.Context, text string) error {
	p.mu.Lock()
	p.instructions = text
	conn := p.conn
	p.mu.Unlock()
	if conn == nil {
		return nil
	}
	return conn.Call(ctx, MethodInstructions, InstructionsParams{Text: text}, nil)
}

// Turn is one turn of a conversation with a model provider of the plugin.
type Turn struct {
	ProviderTurnParams
	p *Plugin
}

// Write streams part of the reply, so the speaker starts speaking before
// the turn ends. Whole sentences are spoken as they arrive.
func (t *Turn) Write(text string) {
	if c, err := t.p.rpc(); err == nil && text != "" {
		c.Notify(MethodProviderOutput, ProviderOutputParams{Session: t.Session, Text: text})
	}
}

// ProviderHandler implements a model provider. Turn answers a turn: text
// to speak (streamed with [Turn.Write] or returned) and functions to call.
// Close, if set, is called when the conversation ends.
type ProviderHandler struct {
	Turn  func(ctx context.Context, t *Turn) (ProviderTurnResult, error)
	Close func(session string)
}

// Provider registers a model provider. Server only.
func (p *Plugin) Provider(spec ProviderSpec, h ProviderHandler) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.providers = append(p.providers, spec)
	p.provHandlers[spec.Name] = h
}

func (p *Plugin) providerTurn(ctx context.Context, tp ProviderTurnParams) (any, error) {
	p.mu.Lock()
	h := p.provHandlers[tp.Provider]
	p.mu.Unlock()
	if h.Turn == nil {
		return nil, fmt.Errorf("unknown provider %q", tp.Provider)
	}
	return h.Turn(ctx, &Turn{ProviderTurnParams: tp, p: p})
}

// SpeechHandler implements a speech engine; either function may be nil.
// Synthesize returns 16-bit little-endian mono PCM at the sample rate of
// the [SpeechSpec].
type SpeechHandler struct {
	Synthesize func(ctx context.Context, s SynthesizeParams) ([]byte, error)
	Transcribe func(ctx context.Context, t TranscribeParams) (string, error)
}

// Speech registers a speech engine. Server only.
func (p *Plugin) Speech(spec SpeechSpec, h SpeechHandler) {
	spec.Synthesize = h.Synthesize != nil
	spec.Transcribe = h.Transcribe != nil
	p.mu.Lock()
	defer p.mu.Unlock()
	p.speech = append(p.speech, spec)
	p.speechFuncs[spec.Name] = h
}

// HTTP serves requests to /plugins/<id>/ on the server with h; the request
// path is the part after the prefix. public routes need no login. Server
// only.
func (p *Plugin) HTTP(h http.Handler, public bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.httpHandler = h
	p.httpSpec = &HTTPSpec{Public: public}
}

// serveHTTP runs h on a request from the host.
func serveHTTP(ctx context.Context, h http.Handler, hr HTTPRequest) (resp HTTPResponse) {
	// httptest.NewRequest panics on a malformed method or path.
	defer func() {
		if recover() != nil {
			resp = HTTPResponse{Status: http.StatusBadRequest}
		}
	}()
	target := hr.Path
	if target == "" {
		target = "/"
	}
	if hr.Query != "" {
		target += "?" + hr.Query
	}
	req := httptest.NewRequest(hr.Method, target, bytes.NewReader(hr.Body)).WithContext(ctx)
	for k, vs := range hr.Header {
		req.Header[http.CanonicalHeaderKey(k)] = vs
	}
	req.RemoteAddr = hr.RemoteAddr
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return HTTPResponse{Status: rec.Code, Header: rec.Header(), Body: rec.Body.Bytes()}
}
