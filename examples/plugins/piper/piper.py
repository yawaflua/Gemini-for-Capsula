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

"""Speech synthesis with Piper (https://github.com/OHF-Voice/piper1-gpl),
offline on the server: in the providers tab pick the speech
"plugin:piper/piper" for a provider and choose a voice.

Shows @plugin.synthesizer: the host sends the text sentence by sentence and
plays the returned 16-bit mono PCM; set_speech() declares the sample rate
and the voices from the config."""

import array
import json
import os
import subprocess
import sys

try:
    from marusya_plugin import Plugin
except ImportError:  # running from the repository
    sys.path.insert(0, os.path.join(os.path.dirname(os.path.abspath(__file__)), "..", "..", "..", "sdk", "python"))
    from marusya_plugin import Plugin

plugin = Plugin()
voices = {}  # name -> path of the .onnx model
rate = 22050


@plugin.on_init
def on_init(host):
    global rate
    base = os.environ.get("MARUSYA_PLUGIN_DIR", ".")
    for name, path in (plugin.config.get("voices") or {}).items():
        voices[name] = os.path.join(base, path)  # relative to the plugin
    rate = int(plugin.config.get("sample_rate") or rate)
    plugin.set_speech("piper", sample_rate=rate, voices=list(voices))


def model_rate(model):
    try:
        with open(model + ".json") as f:
            return int(json.load(f)["audio"]["sample_rate"])
    except (OSError, ValueError, KeyError):
        return rate


def resample(pcm, src, dst):
    """Linear resampling of 16-bit mono PCM: voices differ in sample rate,
    the engine declares one."""
    if src == dst or not pcm:
        return pcm
    a = array.array("h", pcm[: len(pcm) // 2 * 2])
    if sys.byteorder == "big":
        a.byteswap()
    n = len(a) * dst // src
    out = array.array("h", bytes(2 * n))
    for i in range(n):
        x = i * src / dst
        j = int(x)
        k = min(j + 1, len(a) - 1)
        out[i] = int(a[j] + (a[k] - a[j]) * (x - j))
    if sys.byteorder == "big":
        out.byteswap()
    return out.tobytes()


@plugin.synthesizer("piper", "Piper (офлайн)")
def synthesize(text, language, voice):
    if not voices:
        raise RuntimeError("no voices: set voices in the plugin config")
    model = voices.get(voice) or voices.get(plugin.config.get("voice")) or next(iter(voices.values()))
    out = subprocess.run(
        [plugin.config.get("piper") or "piper", "--model", model, "--output-raw"],
        input=text.encode(), capture_output=True, timeout=60,
    )
    if out.returncode != 0:
        raise RuntimeError("piper: " + out.stderr.decode(errors="replace").strip()[-300:])
    return resample(out.stdout, model_rate(model), rate)


if __name__ == "__main__":
    plugin.run()
