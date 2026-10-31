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

package plugin

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/yawaflua/marusya-gemini/pkg/pluginsdk"
	"github.com/yawaflua/marusya-gemini/pkg/pluginsdk/jsonrpc"
)

// States of a plugin.
const (
	StateDisabled = "disabled"
	StateStarting = "starting"
	StateRunning  = "running"
	StateFailed   = "failed" // exited; restarted after a pause
	StateStopped  = "stopped"
)

const (
	initTimeout     = 10 * time.Second
	shutdownTimeout = 3 * time.Second
	minBackoff      = time.Second
	maxBackoff      = time.Minute
	// stableAfter resets the backoff of a plugin that ran this long.
	stableAfter = time.Minute
	eventQueue  = 64
)

// instance runs one plugin and restarts it when it exits.
type instance struct {
	m    Manifest
	mgr  *Manager
	log  *slog.Logger
	side *Side

	mu       sync.Mutex
	conn     *jsonrpc.Conn
	tools    []pluginsdk.ToolSpec
	events   []string
	caps     capabilities
	state    string
	err      string
	restarts int
	started  time.Time
	cancel   context.CancelFunc
	done     chan struct{}
	ready    chan struct{} // closed after the first initialization attempt

	queue chan notification

	outMu   sync.Mutex
	outputs map[string]func(string) // running provider turns, by session
}

// capabilities are what a server plugin implements besides tools.
type capabilities struct {
	instructions string
	providers    []pluginsdk.ProviderSpec
	speech       []pluginsdk.SpeechSpec
	http         *pluginsdk.HTTPSpec
}

type notification struct {
	method string
	params any
}

func newInstance(m Manifest, mgr *Manager) *instance {
	return &instance{
		m: m, mgr: mgr, side: m.Side(mgr.cfg.Host),
		log:   mgr.log.With("plugin", m.ID),
		state: StateStopped,
	}
}

// start runs the plugin until stop. It is a no-op if it runs.
func (in *instance) start(ctx context.Context) {
	in.mu.Lock()
	defer in.mu.Unlock()
	if in.cancel != nil {
		return
	}
	ctx, in.cancel = context.WithCancel(ctx)
	in.done = make(chan struct{})
	in.ready = make(chan struct{})
	in.queue = make(chan notification, eventQueue)
	in.state = StateStarting
	in.err = ""
	go in.loop(ctx, in.done, in.ready)
}

// stop ends the plugin and waits for it.
func (in *instance) stop(state string) {
	in.mu.Lock()
	cancel, done := in.cancel, in.done
	in.cancel = nil
	in.mu.Unlock()
	if cancel == nil {
		in.setState(state, "")
		return
	}
	cancel()
	<-done
	in.setState(state, "")
}

// waitReady waits until the plugin was initialized or failed to start.
func (in *instance) waitReady() {
	in.mu.Lock()
	ready := in.ready
	in.mu.Unlock()
	if ready == nil {
		return
	}
	select {
	case <-ready:
	case <-time.After(initTimeout):
	}
}

func (in *instance) setState(state, err string) {
	in.mu.Lock()
	changed := in.state != state || in.err != err
	in.state, in.err = state, err
	if state != StateRunning {
		in.tools, in.events, in.conn, in.caps = nil, nil, nil, capabilities{}
	}
	in.mu.Unlock()
	if changed {
		in.mgr.changed()
	}
}

func (in *instance) loop(ctx context.Context, done, ready chan struct{}) {
	defer close(done)
	readyOnce := sync.OnceFunc(func() { close(ready) })
	defer readyOnce()
	backoff := minBackoff
	for {
		start := time.Now()
		err := in.run(ctx, readyOnce)
		if ctx.Err() != nil {
			return
		}
		readyOnce()
		if time.Since(start) > stableAfter {
			backoff = minBackoff
		}
		msg := "exited"
		if err != nil {
			msg = err.Error()
		}
		in.log.Warn("plugin stopped; restarting", "err", msg, "in", backoff)
		in.mu.Lock()
		in.restarts++
		in.mu.Unlock()
		in.setState(StateFailed, msg)
		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}
		backoff = min(backoff*2, maxBackoff)
		in.setState(StateStarting, "")
	}
}

// command builds the process.
func (in *instance) command(ctx context.Context) *exec.Cmd {
	name := in.side.Command[0]
	if !filepath.IsAbs(name) {
		if p := filepath.Join(in.m.Dir, name); fileExists(p) {
			name = p
		}
	}
	cmd := exec.CommandContext(ctx, name, in.side.Command[1:]...)
	cmd.Dir = in.m.Dir
	cmd.Env = append(os.Environ(),
		"MARUSYA_PLUGIN_ID="+in.m.ID,
		"MARUSYA_PLUGIN_DIR="+in.m.Dir,
		"MARUSYA_PLUGIN_HOST="+in.mgr.cfg.Host,
	)
	for k, v := range in.side.Env {
		cmd.Env = append(cmd.Env, k+"="+v)
	}
	cmd.Cancel = func() error { return cmd.Process.Signal(syscall.SIGTERM) }
	cmd.WaitDelay = shutdownTimeout
	return cmd
}

func fileExists(p string) bool {
	st, err := os.Stat(p)
	return err == nil && !st.IsDir()
}

// run starts the process once and waits for it to exit.
func (in *instance) run(ctx context.Context, ready func()) error {
	pctx, kill := context.WithCancel(context.Background())
	defer kill()
	cmd := in.command(pctx)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return err
	}
	if err := cmd.Start(); err != nil {
		return err
	}
	go in.copyStderr(stderr)

	conn := jsonrpc.New(stdout, stdin, in.handle)
	// Streamed reply text must reach the turn before the reply itself.
	conn.Ordered(pluginsdk.MethodProviderOutput)
	served := make(chan error, 1)
	go func() { served <- conn.Run(context.Background()) }()
	exited := make(chan error, 1)
	go func() {
		<-served
		exited <- cmd.Wait()
	}()

	var res pluginsdk.InitializeResult
	ictx, cancel := context.WithTimeout(ctx, initTimeout)
	err = conn.Call(ictx, pluginsdk.MethodInitialize, pluginsdk.InitializeParams{
		Protocol:    pluginsdk.ProtocolVersion,
		Host:        in.mgr.cfg.Host,
		HostVersion: in.mgr.cfg.HostVersion,
		PluginDir:   in.m.Dir,
		Config:      in.mgr.config(in.m),
		Device:      in.mgr.cfg.Device,
	}, &res)
	cancel()
	if err == nil {
		err = in.accept(conn, res)
	}
	ready()
	if err != nil {
		stdin.Close()
		kill()
		<-exited
		return fmt.Errorf("initialize: %w", err)
	}
	in.mu.Lock()
	in.started = time.Now()
	queue := in.queue
	in.mu.Unlock()
	in.log.Info("plugin started", "tools", len(res.Tools), "events", res.Events)
	in.mgr.changed()

	for {
		select {
		case n := <-queue:
			if err := conn.Notify(n.method, n.params); err != nil {
				in.log.Debug("notify", "method", n.method, "err", err)
			}
		case err := <-exited:
			if err == nil {
				err = errors.New("exited")
			}
			return err
		case <-ctx.Done():
			sctx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
			conn.Call(sctx, pluginsdk.MethodShutdown, struct{}{}, nil)
			cancel()
			stdin.Close()
			select {
			case <-exited:
			case <-time.After(shutdownTimeout):
				kill()
				<-exited
			}
			return nil
		}
	}
}

// accept checks the answer to initialize and makes the plugin usable.
func (in *instance) accept(conn *jsonrpc.Conn, res pluginsdk.InitializeResult) error {
	if res.Protocol > pluginsdk.ProtocolVersion {
		return fmt.Errorf("protocol %d is newer than %d", res.Protocol, pluginsdk.ProtocolVersion)
	}
	var tools []pluginsdk.ToolSpec
	for _, t := range res.Tools {
		if err := ValidTool(t); err != nil {
			in.log.Warn("tool rejected", "err", err)
			continue
		}
		if in.mgr.cfg.Reserved != nil && in.mgr.cfg.Reserved(t.Name) {
			in.log.Warn("tool rejected: the name is taken by a built-in function", "tool", t.Name)
			continue
		}
		tools = append(tools, t)
	}
	caps := capabilities{instructions: strings.TrimSpace(res.Instructions)}
	if in.mgr.cfg.Host == pluginsdk.HostServer {
		for _, p := range res.Providers {
			if !nameRe.MatchString(p.Name) {
				in.log.Warn("provider rejected: bad name", "provider", p.Name)
				continue
			}
			caps.providers = append(caps.providers, p)
		}
		for _, sp := range res.Speech {
			if !nameRe.MatchString(sp.Name) || (!sp.Synthesize && !sp.Transcribe) {
				in.log.Warn("speech engine rejected: bad name or nothing to do", "engine", sp.Name)
				continue
			}
			if sp.SampleRate <= 0 {
				sp.SampleRate = pluginsdk.DefaultSampleRate
			}
			caps.speech = append(caps.speech, sp)
		}
		caps.http = res.HTTP
	} else if caps.instructions != "" || len(res.Providers)+len(res.Speech) > 0 || res.HTTP != nil {
		in.log.Warn("instructions, providers, speech and HTTP work only on the server; ignored")
		caps = capabilities{}
	}
	in.mu.Lock()
	in.conn, in.tools, in.events, in.caps = conn, tools, res.Events, caps
	in.state, in.err = StateRunning, ""
	in.mu.Unlock()
	return nil
}

// nameRe checks the names of providers and speech engines.
var nameRe = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,31}$`)

// capabilities returns what the running plugin implements.
func (in *instance) capabilities() capabilities {
	in.mu.Lock()
	defer in.mu.Unlock()
	return in.caps
}

func (in *instance) copyStderr(r io.Reader) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64<<10), 1<<20)
	for sc.Scan() {
		in.log.Info(sc.Text(), "stream", "stderr")
	}
}

// handle serves requests from the plugin.
func (in *instance) handle(ctx context.Context, method string, params json.RawMessage) (any, error) {
	h := in.mgr.cfg.Handler
	bad := func(err error) error { return &jsonrpc.Error{Code: jsonrpc.CodeInvalidParams, Message: err.Error()} }
	switch method {
	case pluginsdk.MethodLog:
		var p pluginsdk.LogParams
		if err := json.Unmarshal(params, &p); err != nil {
			return nil, bad(err)
		}
		in.log.Log(ctx, logLevel(p.Level), p.Message)
		return nil, nil

	case pluginsdk.MethodCommand:
		var p pluginsdk.CommandParams
		if err := json.Unmarshal(params, &p); err != nil {
			return nil, bad(err)
		}
		if h == nil {
			return nil, errors.New("no speaker to run commands on")
		}
		out, err := h.Command(ctx, in.m.ID, p.Device, p.Action, p.Args)
		if out == nil && err == nil {
			out = map[string]any{}
		}
		return out, err

	case pluginsdk.MethodSay:
		var p pluginsdk.SayParams
		if err := json.Unmarshal(params, &p); err != nil {
			return nil, bad(err)
		}
		if h == nil {
			return nil, errors.New("speech is not available here")
		}
		answer, err := h.Say(ctx, in.m.ID, p.Device, p.Text, p.Listen)
		return pluginsdk.SayResult{Answer: answer}, err

	case pluginsdk.MethodDevices:
		var devs []pluginsdk.Device
		if h != nil {
			devs = h.Devices(ctx)
		}
		if devs == nil {
			devs = []pluginsdk.Device{}
		}
		return pluginsdk.DevicesResult{Devices: devs}, nil

	case pluginsdk.MethodSend:
		var p pluginsdk.SendParams
		if err := json.Unmarshal(params, &p); err != nil {
			return nil, bad(err)
		}
		if h == nil {
			return nil, errors.New("there is no other half to send to")
		}
		return nil, h.Send(ctx, in.m.ID, p.Device, p.Data)

	case pluginsdk.MethodRequest:
		var p pluginsdk.RequestParams
		if err := json.Unmarshal(params, &p); err != nil {
			return nil, bad(err)
		}
		sh, ok := h.(ServerHandler)
		if !ok {
			return nil, errors.New("requests to the assistant are sent only from the server")
		}
		return nil, sh.Request(ctx, in.m.ID, p.Device, p.Text)

	case pluginsdk.MethodInstructions:
		var p pluginsdk.InstructionsParams
		if err := json.Unmarshal(params, &p); err != nil {
			return nil, bad(err)
		}
		if in.mgr.cfg.Host != pluginsdk.HostServer {
			return nil, errors.New("instructions are set only on the server")
		}
		in.mu.Lock()
		changed := in.caps.instructions != strings.TrimSpace(p.Text)
		in.caps.instructions = strings.TrimSpace(p.Text)
		in.mu.Unlock()
		if changed {
			in.mgr.changed()
		}
		return nil, nil

	case pluginsdk.MethodProviderOutput:
		var p pluginsdk.ProviderOutputParams
		if err := json.Unmarshal(params, &p); err != nil {
			return nil, bad(err)
		}
		in.outMu.Lock()
		f := in.outputs[p.Session]
		in.outMu.Unlock()
		if f != nil {
			f(p.Text)
		}
		return nil, nil
	}
	return nil, jsonrpc.ErrMethodNotFound(method)
}

func logLevel(s string) slog.Level {
	switch strings.ToLower(s) {
	case "debug":
		return slog.LevelDebug
	case "warn", "warning":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	}
	return slog.LevelInfo
}

// notify queues a notification; it is dropped if the plugin is not running
// or does not keep up.
func (in *instance) notify(method string, params any) {
	in.mu.Lock()
	q, running := in.queue, in.state == StateRunning
	in.mu.Unlock()
	if !running || q == nil {
		return
	}
	select {
	case q <- notification{method, params}:
	default:
		in.log.Debug("plugin is not keeping up; notification dropped", "method", method)
	}
}

// wants reports whether the plugin subscribed to events of kind.
func (in *instance) wants(kind string) bool {
	in.mu.Lock()
	defer in.mu.Unlock()
	for _, k := range in.events {
		if k == "*" || k == kind {
			return true
		}
	}
	return false
}

// call runs a tool.
func (in *instance) call(ctx context.Context, name string, args map[string]any, dev pluginsdk.Device) (map[string]any, error) {
	in.mu.Lock()
	conn := in.conn
	in.mu.Unlock()
	if conn == nil {
		return nil, fmt.Errorf("plugin %s is not running", in.m.ID)
	}
	if args == nil {
		args = map[string]any{}
	}
	var raw json.RawMessage
	if err := conn.Call(ctx, pluginsdk.MethodToolCall, pluginsdk.ToolCallParams{Name: name, Args: args, Device: dev}, &raw); err != nil {
		return nil, err
	}
	var out map[string]any
	if json.Unmarshal(raw, &out) == nil && out != nil {
		return out, nil
	}
	// Not an object: wrap it so the model gets something.
	var v any
	json.Unmarshal(raw, &v)
	return map[string]any{"result": v}, nil
}

// rpc returns the connection of the running plugin.
func (in *instance) rpc() (*jsonrpc.Conn, error) {
	in.mu.Lock()
	conn := in.conn
	in.mu.Unlock()
	if conn == nil {
		return nil, fmt.Errorf("plugin %s is not running", in.m.ID)
	}
	return conn, nil
}

// turn runs one call of a provider turn; output receives the streamed text.
func (in *instance) turn(ctx context.Context, p pluginsdk.ProviderTurnParams, output func(string)) (pluginsdk.ProviderTurnResult, error) {
	var res pluginsdk.ProviderTurnResult
	conn, err := in.rpc()
	if err != nil {
		return res, err
	}
	in.outMu.Lock()
	if in.outputs == nil {
		in.outputs = map[string]func(string){}
	}
	in.outputs[p.Session] = output
	in.outMu.Unlock()
	defer func() {
		in.outMu.Lock()
		delete(in.outputs, p.Session)
		in.outMu.Unlock()
	}()
	err = conn.Call(ctx, pluginsdk.MethodProviderTurn, p, &res)
	return res, err
}

// request calls a method of the running plugin.
func (in *instance) request(ctx context.Context, method string, params, result any) error {
	conn, err := in.rpc()
	if err != nil {
		return err
	}
	return conn.Call(ctx, method, params, result)
}
