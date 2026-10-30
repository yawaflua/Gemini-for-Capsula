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

"""The server half of speaker-stats, written with the Python SDK: collects
the wake counters sent by the speaker halves and counts turns per speaker."""

import os
import sys
import threading

try:
    from marusya_plugin import Plugin
except ImportError:  # running from the repository
    sys.path.insert(0, os.path.join(os.path.dirname(os.path.abspath(__file__)), "..", "..", "..", "sdk", "python"))
    from marusya_plugin import Plugin

plugin = Plugin()
lock = threading.Lock()
stats = {}  # device -> {"wakes": n, "turns": n}


def entry(device):
    return stats.setdefault(device or "?", {"wakes": 0, "turns": 0})


@plugin.on_message
def on_message(device, data):
    with lock:
        entry(device)["wakes"] = int(data.get("wakes", 0))


@plugin.on_event("turn")
def on_turn(event):
    with lock:
        entry(event.get("device"))["turns"] += 1


@plugin.tool(
    "speaker_stats",
    "Returns how many times each speaker was woken and how many conversation turns it had since the server started.",
)
def speaker_stats(call):
    with lock:
        return {"speakers": {k: dict(v) for k, v in stats.items()}, "asked_from": call.device_id}


if __name__ == "__main__":
    plugin.run()
