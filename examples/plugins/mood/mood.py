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

"""Changes how Marusya answers ("Маруся, будь пираткой"): the plugin adds a
mood to the system instruction of the model and tints the LED ring.

Shows the server-side SDK functions for the character of the assistant:
plugin.instructions and set_instructions(), a tool, a speaker command
(ring.set) and an HTTP page in the panel (/plugins/mood/)."""

import html
import os
import sys
import threading

try:
    from marusya_plugin import Plugin
except ImportError:  # running from the repository
    sys.path.insert(0, os.path.join(os.path.dirname(os.path.abspath(__file__)), "..", "..", "..", "sdk", "python"))
    from marusya_plugin import Plugin

# name -> (instruction, ring colour). Extended by the "moods" config.
MOODS = {
    "обычная": ("", ""),
    "весёлая": ("Будь очень весёлой и жизнерадостной, шути и радуйся.", "ffcc00"),
    "пиратская": ("Отвечай как пират: «Йо-хо-хо», «тысяча чертей», морские словечки.", "ff3300"),
    "ворчливая": ("Ворчи, как уставший старик, но всё равно помогай.", "8844ff"),
    "краткая": ("Отвечай одним коротким предложением.", "00aaff"),
}

# The tool lists the moods; on_init adds the ones from the config before the
# host reads the tool.
MOOD_PARAM = {"name": "mood", "type": "string", "required": True, "enum": list(MOODS)}

plugin = Plugin()
lock = threading.Lock()
current = "обычная"


@plugin.on_init
def on_init(host):
    global current
    for name, m in (plugin.config.get("moods") or {}).items():
        MOODS[name] = (m.get("instruction", ""), m.get("color", ""))
    MOOD_PARAM["enum"] = list(MOODS)
    current = plugin.config.get("mood") if plugin.config.get("mood") in MOODS else "обычная"
    plugin.instructions = instruction(current)


def instruction(mood):
    text = MOODS[mood][0]
    return "Твоё настроение: %s. %s" % (mood, text) if text else ""


def set_mood(mood, device=""):
    """Switches the mood: the next answer already follows it."""
    global current
    if mood not in MOODS:
        raise ValueError("unknown mood %r, known: %s" % (mood, ", ".join(MOODS)))
    with lock:
        current = mood
    plugin.set_instructions(instruction(mood))
    color = MOODS[mood][1]
    try:
        if color:
            plugin.command("ring.set", {"scene": "breathe", "color": color, "seconds": 10}, device=device)
        else:
            plugin.command("ring.reset", device=device)
    except Exception as e:  # noqa: BLE001 - the ring is optional
        plugin.log("ring: %s" % e, "warn")


@plugin.tool(
    "set_mood",
    "Changes the mood and manner in which the assistant answers from now on. "
    "Use it when the user asks the assistant to be cheerful, a pirate, grumpy, brief or normal again.",
    params=[MOOD_PARAM],
)
def set_mood_tool(call):
    set_mood(call.get("mood"), call.device_id)
    return {"mood": current}


@plugin.http()
def page(req):
    if req.method == "POST":
        set_mood(req.json().get("mood", ""))
        return {"mood": current}
    buttons = "".join(
        '<button%s onclick="pick(this)">%s</button> ' % (" disabled" if m == current else "", html.escape(m))
        for m in MOODS
    )
    return PAGE % buttons


PAGE = """<!doctype html><meta charset="utf-8"><title>Настроение</title>
<style>body{font:16px system-ui;margin:2em}button{font:inherit;padding:.4em 1em;margin:.2em}</style>
<h1>Настроение Маруси</h1><p>%s</p>
<script>
async function pick(b) {
  await fetch(location.pathname, {method: "POST", headers: {"X-Marusya-Panel": "1"},
    body: JSON.stringify({mood: b.textContent})});
  location.reload();
}
</script>"""

if __name__ == "__main__":
    plugin.run()
