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

"""Plays music from YouTube Music on a speaker ("Маруся, включи Кино на
ютуб музыке"): finds tracks with yt-dlp on the server and starts the
stream on the speaker with the media.play_url command.

Shows a tool that drives a speaker (host.command), host.devices and an
HTTP page with a search form in the panel (/plugins/ytmusic/)."""

import html
import json
import os
import subprocess
import sys
import urllib.parse

try:
    from marusya_plugin import Plugin
except ImportError:  # running from the repository
    sys.path.insert(0, os.path.join(os.path.dirname(os.path.abspath(__file__)), "..", "..", "..", "sdk", "python"))
    from marusya_plugin import Plugin

plugin = Plugin()


def search(query, limit=5):
    """Returns up to limit tracks: dicts with id, title, artist and url."""
    yt_dlp = plugin.config.get("yt_dlp") or "yt-dlp"
    url = "https://music.youtube.com/search?q=" + urllib.parse.quote(query) + "#songs"
    out = subprocess.run(
        [yt_dlp, "--flat-playlist", "--dump-single-json", "--no-warnings", "--playlist-items", "1-%d" % limit, url],
        capture_output=True, text=True, timeout=60,
    )
    if out.returncode != 0:
        raise RuntimeError("yt-dlp: " + (out.stderr.strip().splitlines() or ["failed"])[-1])
    tracks = []
    for e in json.loads(out.stdout).get("entries") or []:
        if not e.get("id"):
            continue
        tracks.append({
            "id": e["id"],
            "title": e.get("title", ""),
            "artist": e.get("artist") or e.get("channel") or e.get("uploader") or "",
            "url": "https://www.youtube.com/watch?v=" + e["id"],
        })
    return tracks[:limit]


def play(track, device=""):
    plugin.command("media.play_url", {"url": track["url"]}, device=device)
    plugin.log("%s: %s — %s" % (device or "speaker", track["artist"], track["title"]))


@plugin.tool(
    "play_youtube_music",
    "Finds a song, artist or album on YouTube Music and plays it on the speaker. "
    "Use it when the user asks to play music from YouTube Music.",
    params=[{"name": "query", "type": "string", "required": True,
             "description": "What to look for, e.g. an artist and a song title."}],
)
def play_tool(call):
    tracks = search(call.get("query", ""), 1)
    if not tracks:
        return {"found": False}
    play(tracks[0], call.device_id)
    return {"found": True, "title": tracks[0]["title"], "artist": tracks[0]["artist"]}


@plugin.http()
def page(req):
    if req.method == "POST":
        body = req.json()
        play({"url": "https://www.youtube.com/watch?v=" + body["id"], "title": body.get("title", ""),
              "artist": ""}, body.get("device", ""))
        return {"ok": True}
    if req.path == "/search":
        return search(req.arg("q"), 10) if req.arg("q") else []
    q = req.arg("q")
    rows, error = "", ""
    if q:
        try:
            for t in search(q, 10):
                rows += '<li><button data-id="%s">▶</button> %s — %s</li>' % (
                    html.escape(t["id"]), html.escape(t["artist"]), html.escape(t["title"]))
        except Exception as e:  # noqa: BLE001 - show it on the page
            error = "<p>%s</p>" % html.escape(str(e))
    devices = "".join('<option>%s</option>' % html.escape(d.get("id", "")) for d in plugin.devices())
    return PAGE % (html.escape(q, quote=True), devices, error, rows)


PAGE = """<!doctype html><meta charset="utf-8"><title>YouTube Music</title>
<style>body{font:16px system-ui;margin:2em}li{margin:.3em 0;list-style:none}button,input,select{font:inherit}</style>
<h1>YouTube Music</h1>
<form><input name="q" value="%s" placeholder="Исполнитель или песня" autofocus>
<select id="device">%s</select> <button>Найти</button></form>%s
<ul>%s</ul>
<script>
document.querySelectorAll("li button").forEach(b => b.onclick = () =>
  fetch(location.pathname, {method: "POST", headers: {"X-Marusya-Panel": "1"},
    body: JSON.stringify({id: b.dataset.id, title: b.parentNode.textContent.slice(2),
      device: document.getElementById("device").value})}));
</script>"""

if __name__ == "__main__":
    plugin.run()
