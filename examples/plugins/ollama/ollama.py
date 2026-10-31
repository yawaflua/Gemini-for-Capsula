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

"""A model provider backed by a local Ollama (https://ollama.com): pick the
type "Плагин" and the model "ollama/chat" in the providers tab and Marusya
talks through it, with speech recognition and synthesis from the server.

Shows @plugin.provider: the plugin keeps the history of each conversation,
streams the reply with turn.write() so that the speaker starts speaking
early, and returns function calls that the host runs (volume, music, other
plugins' tools) before calling it again with turn.tool_results."""

import itertools
import json
import os
import sys
import threading
import urllib.request

try:
    from marusya_plugin import Plugin
except ImportError:  # running from the repository
    sys.path.insert(0, os.path.join(os.path.dirname(os.path.abspath(__file__)), "..", "..", "..", "sdk", "python"))
    from marusya_plugin import Plugin

plugin = Plugin()
lock = threading.Lock()
histories = {}  # session -> list of Ollama messages
ids = itertools.count(1)


def schema(tool):
    """Converts a Marusya ToolSpec into an Ollama (OpenAI-style) function."""
    props, required = {}, []
    for p in tool.get("params") or []:
        prop = {"type": p["type"]}
        for k in ("description", "enum", "minimum", "maximum"):
            if p.get(k) is not None:
                prop[k] = p[k]
        props[p["name"]] = prop
        if p.get("required"):
            required.append(p["name"])
    return {"type": "function", "function": {
        "name": tool["name"], "description": tool.get("description", ""),
        "parameters": {"type": "object", "properties": props, "required": required}}}


def close(session):
    with lock:
        histories.pop(session, None)


@plugin.provider("chat", "Ollama", "Локальная модель Ollama", close=close)
def chat(turn):
    keep = int(plugin.config.get("history", 40))
    with lock:
        history = histories.setdefault(turn.session, [])
        if turn.text:
            history.append({"role": "user", "content": turn.text})
        for r in turn.tool_results:
            out = {"error": r["error"]} if r.get("error") else r.get("output") or {}
            history.append({"role": "tool", "tool_name": r["name"], "content": json.dumps(out, ensure_ascii=False)})
        del history[:-keep]
        messages = [{"role": "system", "content": turn.system}] + list(history)

    body = {"model": plugin.config.get("model", "qwen3:8b"), "messages": messages, "stream": True,
            "tools": [schema(t) for t in turn.tools]}
    if "think" in plugin.config:  # thinking models: false answers at once
        body["think"] = bool(plugin.config["think"])
    req = urllib.request.Request(plugin.config.get("url", "http://localhost:11434").rstrip("/") + "/api/chat",
                                 json.dumps(body).encode(), {"Content-Type": "application/json"})
    text, calls = "", []
    with urllib.request.urlopen(req, timeout=float(plugin.config.get("timeout", 120))) as res:
        for line in res:  # one JSON object per line
            if not line.strip():
                continue
            chunk = json.loads(line)
            if chunk.get("error"):
                raise RuntimeError("ollama: " + chunk["error"])
            msg = chunk.get("message") or {}
            if msg.get("content"):
                text += msg["content"]
                turn.write(msg["content"])  # spoken while the rest is generated
            for c in msg.get("tool_calls") or []:
                f = c.get("function") or {}
                calls.append({"id": "c%d" % next(ids), "name": f.get("name", ""), "args": f.get("arguments") or {}})

    with lock:
        history.append({"role": "assistant", "content": text,
                        "tool_calls": [{"function": {"name": c["name"], "arguments": c["args"]}} for c in calls]})
    # The text is already written; return only the calls.
    return {"tool_calls": calls}


if __name__ == "__main__":
    plugin.run()
