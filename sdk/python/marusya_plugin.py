# Copyright 2026 The Marusya-Gemini Authors.
#
# Licensed under the Apache License, Version 2.0 (the "License");
# you may not use this file except in compliance with the License.
# You may obtain a copy of the License at
#
#     https://www.apache.org/licenses/LICENSE-2.0
#
# Unless required by applicable law or agreed to in writing, software
# distributed under the License is distributed on an "AS IS" BASIS,
# WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
# See the License for the specific language governing permissions and
# limitations under the License.

"""Marusya plugin SDK for Python 3.8+, standard library only.

Marusya starts the plugin and talks JSON-RPC 2.0 over its stdin and stdout,
one message per line (see pkg/pluginsdk/protocol.go). Do not print to
stdout: use plugin.log() or stderr.

    from marusya_plugin import Plugin

    plugin = Plugin()

    @plugin.tool("coin_flip", "Flips a coin and returns heads or tails.")
    def coin_flip(call):
        return {"side": "heads"}

    plugin.run()
"""

import base64
import itertools
import json
import sys
import threading
import traceback

PROTOCOL_VERSION = 1

__all__ = ["Plugin", "Call", "Turn", "HTTPRequest", "RPCError", "PROTOCOL_VERSION"]


class RPCError(Exception):
    """An error answered by the host, or one to answer with a code."""

    def __init__(self, message, code=-32000):
        super().__init__(message)
        self.code = code


class Call:
    """A tool call: name, args (a dict) and device (a dict with id,
    language, location and timezone)."""

    def __init__(self, params):
        self.name = params.get("name", "")
        self.args = params.get("args") or {}
        self.device = params.get("device") or {}

    @property
    def device_id(self):
        return self.device.get("id", "")

    def get(self, key, default=None):
        return self.args.get(key, default)


class Turn:
    """A turn of a conversation with a model provider of the plugin (see
    Plugin.provider). text is what the user said (empty when the turn goes
    on with tool_results), session identifies the conversation, system is
    the system instruction, tools the functions the model may call (dicts
    as in Plugin.tool), language a BCP-47 code."""

    def __init__(self, plugin, params):
        self._plugin = plugin
        self.provider = params.get("provider", "")
        self.session = params.get("session", "")
        self.text = params.get("text", "")
        self.tool_results = params.get("tool_results") or []
        self.system = params.get("system", "")
        self.language = params.get("language", "")
        self.tools = params.get("tools") or []

    def write(self, text):
        """Streams part of the reply: the speaker starts speaking whole
        sentences before the turn ends."""
        if text:
            self._plugin._notify("provider.output", {"session": self.session, "text": text})


class HTTPRequest:
    """A request to /plugins/<id>/ on the server: method, path (below the
    prefix, e.g. "/"), query (a dict of lists), headers (a dict of lists),
    body (bytes) and remote_addr."""

    def __init__(self, params):
        self.method = params.get("method", "GET")
        self.path = params.get("path", "/")
        self.query = _parse_qs(params.get("query", ""))
        self.headers = {k.lower(): v for k, v in (params.get("header") or {}).items()}
        self.body = base64.b64decode(params.get("body") or "")
        self.remote_addr = params.get("remote_addr", "")

    def header(self, name, default=""):
        v = self.headers.get(name.lower())
        return v[0] if v else default

    def arg(self, name, default=""):
        v = self.query.get(name)
        return v[0] if v else default

    def json(self):
        return json.loads(self.body or b"null")


def _parse_qs(q):
    from urllib.parse import parse_qs

    return parse_qs(q, keep_blank_values=True)


class Plugin:
    def __init__(self):
        self._tools = []
        self._handlers = {}
        self._events = []
        self._on_event = None
        self._on_message = None
        self._on_init = None
        self.host = {}
        self.config = {}
        self._out = sys.stdout
        self._in = sys.stdin
        self._wlock = threading.Lock()
        self._ids = itertools.count(1)
        self._pending = {}
        self._plock = threading.Lock()
        self._stop = threading.Event()
        # Server only.
        self.instructions = ""
        self._providers = []
        self._prov = {}
        self._speech = {}
        self._http = None
        self._http_public = False

    # Registration.

    def tool(self, name, description, params=None):
        """Decorator registering a function for the language model.

        params is a list of dicts: {"name", "type" ("string", "integer",
        "number" or "boolean"), "description", "required", "enum",
        "minimum", "maximum"}. The function gets a Call and returns a dict.
        """

        def register(fn):
            spec = {"name": name, "description": description}
            if params:
                spec["params"] = params
            self._tools.append(spec)
            self._handlers[name] = fn
            return fn

        return register

    def on_init(self, fn):
        """Decorator: fn(host) runs before anything else; host is the
        initialize request (host, plugin_dir, config, device)."""
        self._on_init = fn
        return fn

    def on_event(self, *kinds):
        """Decorator: fn(event) receives events of the given kinds ("*" for
        all); event is a dict with kind, device and data."""

        def register(fn):
            self._events = list(kinds) or ["*"]
            self._on_event = fn
            return fn

        return register

    def on_message(self, fn):
        """Decorator: fn(device, data) receives data from the other half."""
        self._on_message = fn
        return fn

    def provider(self, name, label="", description="", close=None):
        """Decorator registering a model provider (server only); it shows up
        as the type "Плагин" in the providers tab. fn(turn) gets a Turn and
        returns the reply: a str, or a dict with "text" and "tool_calls"
        (a list of {"id", "name", "args"}). The host runs the calls and calls
        fn again with turn.tool_results. close(session), if given, is called
        when a conversation ends. The plugin keeps the history itself."""

        def register(fn):
            self._providers.append({"name": name, "label": label, "description": description})
            self._prov[name] = (fn, close)
            return fn

        return register

    def synthesizer(self, name, label="", sample_rate=24000, voices=None):
        """Decorator registering speech synthesis (server only):
        fn(text, language, voice) returns 16-bit little-endian mono PCM
        bytes at sample_rate."""

        def register(fn):
            e = self._engine(name, label)
            e["spec"].update({"synthesize": True, "sample_rate": sample_rate, "voices": voices or []})
            e["synthesize"] = fn
            return fn

        return register

    def transcriber(self, name, label=""):
        """Decorator registering speech recognition (server only):
        fn(audio, sample_rate, language) gets 16-bit little-endian mono PCM
        bytes and returns the text, or "" if nothing was said."""

        def register(fn):
            e = self._engine(name, label)
            e["spec"]["transcribe"] = True
            e["transcribe"] = fn
            return fn

        return register

    def set_speech(self, name, sample_rate=None, voices=None):
        """Changes the sample rate or the voices of a registered engine, e.g.
        in on_init from the config: the host reads them after on_init."""
        spec = self._speech[name]["spec"]
        if sample_rate:
            spec["sample_rate"] = int(sample_rate)
        if voices is not None:
            spec["voices"] = list(voices)

    def _engine(self, name, label):
        e = self._speech.setdefault(name, {"spec": {"name": name}})
        if label:
            e["spec"]["label"] = label
        return e

    def http(self, public=False):
        """Decorator: fn(request) serves /plugins/<id>/ on the server (server
        only). request is an HTTPRequest. Return a str (HTML), a dict or
        list (JSON), bytes, or a tuple (status, headers, body). Without
        public a request needs the panel login or the server token."""

        def register(fn):
            self._http = fn
            self._http_public = public
            return fn

        return register

    # Calls to the host.

    def request(self, text, device="", timeout=30):
        """Asks the assistant on a speaker as if the user had said text; the
        answer is spoken there (server only)."""
        self._call("host.request", {"device": device, "text": text}, timeout)

    def set_instructions(self, text, timeout=10):
        """Replaces what the plugin adds to the system instruction of the
        model (server only). Before run() just set plugin.instructions."""
        self.instructions = text
        self._call("host.instructions", {"text": text}, timeout)

    def command(self, action, args=None, device="", timeout=30):
        """Runs a speaker action, e.g. command("volume.set", {"level": 30})."""
        return self._call("host.command", {"device": device, "action": action, "args": args or {}}, timeout)

    def say(self, text, device="", listen=False, timeout=120):
        """Speaks text on a speaker (server only); with listen returns the answer."""
        res = self._call("host.say", {"device": device, "text": text, "listen": listen}, timeout) or {}
        return res.get("answer", "")

    def devices(self, timeout=10):
        """Lists the connected speakers (server only)."""
        return (self._call("host.devices", {}, timeout) or {}).get("devices", [])

    def send(self, data, device="", timeout=10):
        """Sends JSON-able data to the other half of the plugin."""
        self._call("plugin.send", {"device": device, "data": data}, timeout)

    def log(self, message, level="info"):
        self._notify("log", {"level": level, "message": str(message)})

    def _notify(self, method, params):
        self._write({"jsonrpc": "2.0", "method": method, "params": params})

    # Serving.

    def run(self):
        """Serves the host until it closes stdin or asks to shut down."""
        for line in self._in:
            if self._stop.is_set():
                break
            line = line.strip()
            if not line:
                continue
            try:
                msg = json.loads(line)
            except ValueError as e:
                self._write({"jsonrpc": "2.0", "id": None, "error": {"code": -32700, "message": str(e)}})
                continue
            if msg.get("method"):
                threading.Thread(target=self._serve, args=(msg,), daemon=True).start()
            elif msg.get("id") is not None:
                with self._plock:
                    waiter = self._pending.pop(msg["id"], None)
                if waiter:
                    waiter[1].append(msg)
                    waiter[0].set()
            if self._stop.is_set():
                break

    def _serve(self, msg):
        mid = msg.get("id")
        try:
            result = self._handle(msg["method"], msg.get("params") or {})
            if mid is not None:
                self._write({"jsonrpc": "2.0", "id": mid, "result": result})
        except Exception as e:  # noqa: BLE001 - answer every error
            if mid is None:
                traceback.print_exc(file=sys.stderr)
                return
            code = e.code if isinstance(e, RPCError) else -32000
            self._write({"jsonrpc": "2.0", "id": mid, "error": {"code": code, "message": str(e)}})

    def _handle(self, method, params):
        if method == "initialize":
            self.host = params
            self.config = params.get("config") or {}
            if self._on_init:
                self._on_init(params)
            res = {"protocol": PROTOCOL_VERSION, "tools": self._tools, "events": self._events}
            if self.instructions:
                res["instructions"] = self.instructions
            if self._providers:
                res["providers"] = self._providers
            if self._speech:
                res["speech"] = [e["spec"] for e in self._speech.values()]
            if self._http:
                res["http"] = {"public": self._http_public}
            return res
        if method == "tool.call":
            call = Call(params)
            fn = self._handlers.get(call.name)
            if fn is None:
                raise RPCError("unknown tool %r" % call.name)
            return fn(call)
        if method == "event":
            if self._on_event:
                self._on_event(params)
            return None
        if method == "message":
            if self._on_message:
                self._on_message(params.get("device", ""), params.get("data"))
            return None
        if method == "provider.turn":
            fn, _ = self._prov.get(params.get("provider"), (None, None))
            if fn is None:
                raise RPCError("unknown provider %r" % params.get("provider"))
            reply = fn(Turn(self, params))
            if reply is None:
                return {}
            if isinstance(reply, str):
                return {"text": reply}
            return reply
        if method == "provider.close":
            _, close = self._prov.get(params.get("provider"), (None, None))
            if close:
                close(params.get("session", ""))
            return None
        if method == "speech.synthesize":
            fn = self._speech.get(params.get("engine"), {}).get("synthesize")
            if fn is None:
                raise RPCError("engine %r does not synthesize speech" % params.get("engine"))
            pcm = fn(params.get("text", ""), params.get("language", ""), params.get("voice", ""))
            return {"audio": base64.b64encode(pcm or b"").decode()}
        if method == "speech.transcribe":
            fn = self._speech.get(params.get("engine"), {}).get("transcribe")
            if fn is None:
                raise RPCError("engine %r does not recognize speech" % params.get("engine"))
            audio = base64.b64decode(params.get("audio") or "")
            return {"text": fn(audio, params.get("sample_rate", 16000), params.get("language", "")) or ""}
        if method == "http.request":
            if self._http is None:
                return {"status": 404}
            return _http_response(self._http(HTTPRequest(params)))
        if method == "shutdown":
            self._stop.set()
            return None
        raise RPCError("method not found: " + method, -32601)

    def _call(self, method, params, timeout):
        mid = next(self._ids)
        done, box = threading.Event(), []
        with self._plock:
            self._pending[mid] = (done, box)
        self._write({"jsonrpc": "2.0", "id": mid, "method": method, "params": params})
        if not done.wait(timeout):
            with self._plock:
                self._pending.pop(mid, None)
            raise RPCError("%s: timed out" % method)
        msg = box[0]
        if msg.get("error"):
            raise RPCError(msg["error"].get("message", "error"), msg["error"].get("code", -32000))
        return msg.get("result")

    def _write(self, msg):
        data = json.dumps(msg, ensure_ascii=False, separators=(",", ":"))
        with self._wlock:
            self._out.write(data + "\n")
            self._out.flush()


def _http_response(r):
    """Converts what an HTTP handler returned into an http.request answer."""
    status, headers = 200, {}
    if isinstance(r, tuple):
        status, headers, r = (list(r) + [None, None])[:3]
        headers = dict(headers or {})
    if isinstance(r, str):
        body = r.encode()
        headers.setdefault("Content-Type", "text/html; charset=utf-8")
    elif isinstance(r, (bytes, bytearray)):
        body = bytes(r)
        headers.setdefault("Content-Type", "application/octet-stream")
    elif r is None:
        body = b""
    else:
        body = json.dumps(r, ensure_ascii=False).encode()
        headers.setdefault("Content-Type", "application/json; charset=utf-8")
    header = {k: (v if isinstance(v, list) else [str(v)]) for k, v in headers.items()}
    return {"status": int(status or 200), "header": header, "body": base64.b64encode(body).decode()}
