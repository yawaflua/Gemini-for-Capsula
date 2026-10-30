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

"""The simplest plugin: flips a coin ("Маруся, подбрось монетку")."""

import os
import random
import sys

try:
    from marusya_plugin import Plugin
except ImportError:  # running from the repository
    sys.path.insert(0, os.path.join(os.path.dirname(os.path.abspath(__file__)), "..", "..", "..", "sdk", "python"))
    from marusya_plugin import Plugin

plugin = Plugin()


@plugin.tool(
    "coin_flip",
    "Flips a coin and returns heads (орёл) or tails (решка). "
    "Use it whenever the user asks to flip or toss a coin, or asks heads or tails.",
)
def coin_flip(call):
    return {"side": random.choice(["орёл", "решка"])}


if __name__ == "__main__":
    plugin.run()
