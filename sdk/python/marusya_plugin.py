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

import itertools
import json
import sys
import threading
import traceback

PROTOCOL_VERSION = 1

__all__ = ["Plugin", "Call", "RPCError", "PROTOCOL_VERSION"]


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

    # Calls to the host.

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
        self._write({"jsonrpc": "2.0", "method": "log", "params": {"level": level, "message": str(message)}})

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
            return {"protocol": PROTOCOL_VERSION, "tools": self._tools, "events": self._events}
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
