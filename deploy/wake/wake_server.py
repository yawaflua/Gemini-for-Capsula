#!/usr/bin/env python3
# Copyright 2026 The Marusya-Gemini Authors.
# SPDX-License-Identifier: Apache-2.0
"""Wake word spotter for marusya-server.

marusya-server opens one WebSocket per speaker, sends the keywords as JSON
({"keywords": [...], "sample_rate": 16000}) and then the speaker's idle
microphone audio (16-bit mono PCM, binary frames). On a detection the
spotter answers {"keyword": "...", "engine": "...", "score": ...}.

Keywords are routed by script:

* Cyrillic -> Vosk (small Russian model). A keyword-only grammar spots
  candidates quickly (~0.3 s), then the full vocabulary re-decodes the last
  seconds of audio to confirm them. The grammar alone is not enough: it
  forces any speech into the keyword, so «сегодня хорошая погода» was heard
  as «маруся» with confidence 1.0. The words must be in the model's
  vocabulary; unknown ones are reported back as unsupported.
* Hebrew and everything else -> faster-whisper on short speech segments,
  fuzzy matched against the keywords. Slower (the utterance has to end), but
  any word in any of Whisper's languages works.

Tuning (environment): VOSK_MIN_CONF, VOSK_HOLD, WHISPER_MODEL,
WHISPER_COMPUTE_TYPE, WHISPER_DEVICE, WHISPER_THREADS, WHISPER_MATCH, WAKE_COOLDOWN.
"""

import asyncio
import collections
import concurrent.futures
import difflib
import json
import logging
import os
import re
import threading
import time
import unicodedata

import numpy as np
from websockets.asyncio.server import serve

log = logging.getLogger("wake")

VOSK_MODEL = os.environ.get("VOSK_MODEL", "/models/vosk-model-small-ru-0.22")
VOSK_MIN_CONF = float(os.environ.get("VOSK_MIN_CONF", "0.6"))
# A keyword must stay in Vosk's partial result this long before it counts.
VOSK_HOLD = float(os.environ.get("VOSK_HOLD", "0.25"))
WHISPER_MODEL = os.environ.get("WHISPER_MODEL", "small")
WHISPER_DEVICE = os.environ.get("WHISPER_DEVICE", "cpu")
WHISPER_COMPUTE = os.environ.get("WHISPER_COMPUTE_TYPE", "int8")
WHISPER_THREADS = int(os.environ.get("WHISPER_THREADS", "0")) or os.cpu_count() or 4
WHISPER_MATCH = float(os.environ.get("WHISPER_MATCH", "0.8"))
COOLDOWN = float(os.environ.get("WAKE_COOLDOWN", "1.5"))

# One Whisper inference at a time for all speakers: it is CPU heavy.
whisper_pool = concurrent.futures.ThreadPoolExecutor(max_workers=1)
# Candidate verification is short but must not block the event loop.
vosk_pool = concurrent.futures.ThreadPoolExecutor(max_workers=1)


def script_of(word: str) -> str:
    if any("֐" <= c <= "׿" for c in word):
        return "he"
    if any("Ѐ" <= c <= "ӿ" for c in word):
        return "cyrillic"
    return "other"


# ---------- models (loaded once, lazily) ----------

_vosk_model = None
_whisper_model = None
_whisper_error = None
_lock = threading.Lock()


def vosk_model():
    global _vosk_model
    with _lock:
        if _vosk_model is None:
            from vosk import Model, SetLogLevel

            SetLogLevel(-1)
            log.info("loading Vosk model %s", VOSK_MODEL)
            _vosk_model = Model(VOSK_MODEL)
        return _vosk_model


def whisper_model():
    """Returns the Whisper model, or None while it is being loaded."""
    global _whisper_model, _whisper_error
    with _lock:
        if _whisper_model is not None or _whisper_error is not None:
            return _whisper_model
        if getattr(whisper_model, "loading", False):
            return None
        whisper_model.loading = True

    def load():
        global _whisper_model, _whisper_error
        try:
            from faster_whisper import WhisperModel

            log.info("loading Whisper model %s (%s, %s); the first start downloads it",
                     WHISPER_MODEL, WHISPER_DEVICE, WHISPER_COMPUTE)
            m = WhisperModel(WHISPER_MODEL, device=WHISPER_DEVICE, compute_type=WHISPER_COMPUTE,
                             cpu_threads=WHISPER_THREADS)
            with _lock:
                _whisper_model = m
            log.info("Whisper model ready")
        except Exception as e:  # noqa: BLE001 - reported to the server
            log.exception("loading Whisper failed")
            with _lock:
                _whisper_error = e

    threading.Thread(target=load, daemon=True).start()
    return None


# ---------- engines ----------


class VoskSpotter:
    """Spots keywords with a small grammar, then verifies them.

    The grammar is fast but forces any speech into the keywords, so a
    candidate is re-decoded with the full Vosk vocabulary: unrelated phrases
    come out as themselves and are dropped. Verification runs only on
    candidates, in a worker thread, so idle audio stays cheap and the event
    loop keeps accepting connections.
    """

    engine = "vosk"
    BUFFER = 2.0  # seconds of audio kept for verification

    def __init__(self, keywords, rate):
        from vosk import KaldiRecognizer

        self.keywords = keywords
        self.norm = [normalize(k) for k in keywords]
        self.rate = rate
        rec = KaldiRecognizer(vosk_model(), rate, json.dumps(keywords + ["[unk]"], ensure_ascii=False))
        rec.SetWords(True)
        self.rec = rec
        self.since = None
        self.buf = collections.deque()
        self.buf_bytes = 0
        self.max_bytes = int(rate * self.BUFFER) * 2
        self.buf_lock = threading.Lock()
        self.verify_lock = threading.Lock()
        self.verifying = False
        self.mute_until = 0.0  # audio time; suppresses repeats of one phrase

    @staticmethod
    def supports(word: str):
        """Returns the spelling of word known to the model, or None."""
        m = vosk_model()
        for w in (word, word.replace("ё", "е")):
            if all(m.vosk_model_find_word(part) >= 0 for part in w.split()):
                return w
        return None

    def _hit(self, text):
        padded = f" {text} "
        return next((k for k in self.keywords if f" {k} " in padded), None)

    def _remember(self, pcm):
        with self.buf_lock:
            self.buf.append(pcm)
            self.buf_bytes += len(pcm)
            while self.buf_bytes > self.max_bytes and self.buf:
                self.buf_bytes -= len(self.buf.popleft())

    def _verify(self, kw, at, emit):
        try:
            from vosk import KaldiRecognizer

            with self.buf_lock:
                pcm = b"".join(self.buf)
            rec = KaldiRecognizer(vosk_model(), self.rate)
            rec.SetWords(True)
            rec.AcceptWaveform(pcm)
            res = json.loads(rec.FinalResult()).get("result", [])
            tokens, confs = [], []
            for w in res:
                for t in normalize(w["word"]):
                    tokens.append(t)
                    confs.append(w.get("conf", 0.0))
            toks = self.norm[self.keywords.index(kw)]
            n = len(toks)
            best = 0.0
            for i in range(len(tokens) - n + 1):
                if tokens[i:i + n] == toks:
                    best = max(best, min(confs[i:i + n]))
            log.debug("verified %r as %r (%.2f)", kw, " ".join(tokens), best)
            if best >= VOSK_MIN_CONF:
                # The grammar keeps forcing the rest of the phrase into the
                # keyword after Reset, so ignore its candidates for a while.
                with self.verify_lock:
                    self.mute_until = at + 2.0
                emit(kw, self.engine, round(best, 2), at)
        except Exception:  # noqa: BLE001 - never kill the stream
            log.exception("vosk verify")
        finally:
            with self.verify_lock:
                self.verifying = False

    def _submit(self, kw, at, emit):
        with self.verify_lock:
            if self.verifying or at < self.mute_until:
                return
            self.verifying = True
        vosk_pool.submit(self._verify, kw, at, emit)

    def feed(self, pcm, now, emit):
        self._remember(pcm)
        if self.rec.AcceptWaveform(pcm):
            self.since = None
            res = json.loads(self.rec.Result()).get("result", [])
            words = [w for w in res if w.get("conf", 0) >= VOSK_MIN_CONF]
            kw = self._hit(" ".join(w["word"] for w in words))
            if kw:
                self._submit(kw, now, emit)
            return
        kw = self._hit(json.loads(self.rec.PartialResult()).get("partial", ""))
        if not kw:
            self.since = None
        elif self.since is None:
            self.since = now
        elif now - self.since >= VOSK_HOLD:
            self.rec.Reset()
            self.since = None
            self._submit(kw, now, emit)


HEBREW_MARKS = re.compile("[֑-ׇ]")  # niqqud and cantillation
# Final forms, and letters that sound alike in modern Hebrew (Whisper often
# swaps them in a lone name).
FINAL_LETTERS = str.maketrans("ךםןףץעתק", "כמנפצאטכ")


def normalize(text: str) -> list[str]:
    text = HEBREW_MARKS.sub("", unicodedata.normalize("NFKC", text.lower()))
    text = text.translate(FINAL_LETTERS).replace("ё", "е")
    return re.findall(r"[^\W\d_]+(?:['׳״-][^\W\d_]+)*", text)


class WhisperSpotter:
    """Transcribes short speech segments and fuzzy matches the keywords."""

    engine = "whisper"
    FRAME = 0.02  # seconds per energy frame
    MAX_SEGMENT = 2.5
    MIN_SPEECH = 0.25
    END_SILENCE = 0.35

    def __init__(self, keywords, rate, language):
        self.keywords = keywords
        self.norm = [normalize(k) for k in keywords]
        self.rate = rate
        self.language = language
        self.floor = -60.0
        self.buf = []  # int16 arrays of the current segment
        self.speech = 0.0
        self.silence = 0.0
        self.skip = False  # the rest of a long utterance
        self.started = 0.0  # when the current segment began
        self.busy = False  # a segment is being transcribed
        self.queue = collections.deque(maxlen=2)
        self.lock = threading.Lock()
        self.pending = np.zeros(0, dtype=np.int16)

    def feed(self, pcm, now, emit):
        samples = np.concatenate([self.pending, np.frombuffer(pcm, dtype=np.int16)])
        n = int(self.rate * self.FRAME)
        whole = len(samples) // n * n
        self.pending = samples[whole:]
        for frame in samples[:whole].reshape(-1, n):
            self._frame(frame, now, emit)

    def _frame(self, frame, now, emit):
        rms = np.sqrt(np.mean(frame.astype(np.float32) ** 2)) + 1e-9
        db = 20 * np.log10(rms / 32768)
        # Noise floor: follows quiet frames at once, rises slowly.
        self.floor = db if db < self.floor else self.floor + 0.02
        voiced = db > max(self.floor + 10, -55)

        if self.skip:
            if voiced:
                self.silence = 0.0
            else:
                self.silence += self.FRAME
                if self.silence >= self.END_SILENCE:
                    self.skip = False
            return
        if voiced:
            self.speech += self.FRAME
            self.silence = 0.0
        elif self.buf:
            self.silence += self.FRAME
        if voiced and not self.buf:
            self.started = now
        if voiced or self.buf:
            self.buf.append(frame)
        total = len(self.buf) * self.FRAME
        if self.buf and (self.silence >= self.END_SILENCE or total >= self.MAX_SEGMENT):
            if total >= self.MAX_SEGMENT and self.silence < self.END_SILENCE:
                self.skip = True
            if self.speech >= self.MIN_SPEECH:
                self._submit(np.concatenate(self.buf), self.started, emit)
            self.buf, self.speech, self.silence = [], 0.0, 0.0

    def _submit(self, audio, started, emit):
        if whisper_model() is None:
            return
        with self.lock:
            # Transcription can be slower than speech: keep the newest segments
            # rather than the oldest, so a keyword after a long phrase is heard.
            self.queue.append((audio, started))
            if self.busy:
                return
            self.busy = True
        self._next(emit)

    def _next(self, emit):
        with self.lock:
            if not self.queue:
                self.busy = False
                return
            audio, started = self.queue.popleft()
        fut = whisper_pool.submit(self._transcribe, whisper_model(), audio)

        def done(f):
            try:
                hit = f.result()
            except Exception:  # noqa: BLE001
                log.exception("whisper")
                hit = None
            if hit:
                with self.lock:
                    self.queue.clear()  # the rest predates the wake
                emit(*hit, started)
            self._next(emit)

        fut.add_done_callback(done)

    def _transcribe(self, model, audio):
        segments, _ = model.transcribe(
            audio.astype(np.float32) / 32768.0,
            language=self.language,
            beam_size=1,
            without_timestamps=True,
            condition_on_previous_text=False,
            vad_filter=False,
            # No temperature fallback: retries on unclear audio take seconds.
            temperature=0.0,
            max_new_tokens=16,  # the keyword is at the start
        )
        text = ""
        for s in segments:
            if s.no_speech_prob > 0.6 or s.avg_logprob < -1.2:
                continue
            text += " " + s.text
        words = normalize(text)
        log.debug("whisper heard %r", text.strip())
        best = (0.0, None)
        for kw, toks in zip(self.keywords, self.norm):
            n = len(toks)
            for i in range(max(0, len(words) - n) + 1):
                ratio = difflib.SequenceMatcher(None, " ".join(words[i:i + n]), " ".join(toks)).ratio()
                best = max(best, (ratio, kw), key=lambda x: x[0])
        if best[1] and best[0] >= WHISPER_MATCH:
            return best[1], self.engine, round(best[0], 2)
        return None


def build(keywords, rate):
    """Creates spotters for keywords; returns (spotters, engines, unsupported)."""
    groups = {}
    for kw in keywords:
        groups.setdefault(script_of(kw), []).append(kw)
    spotters, engines, unsupported = [], {}, []

    cyr = groups.pop("cyrillic", [])
    if cyr:
        known = []
        for kw in cyr:
            spelling = VoskSpotter.supports(kw)
            if spelling:
                known.append(spelling)
                engines[kw] = "vosk"
            else:
                groups.setdefault("ru-whisper", []).append(kw)  # out of vocabulary
        if known:
            spotters.append(VoskSpotter(known, rate))
    for group, kws in groups.items():
        language = {"he": "he", "ru-whisper": "ru"}.get(group)  # None: detect
        spotters.append(WhisperSpotter(kws, rate, language))
        for kw in kws:
            engines[kw] = "whisper"
    if any(isinstance(s, WhisperSpotter) for s in spotters):
        whisper_model()  # start loading in the background
        if _whisper_error is not None:
            unsupported = [k for k, e in engines.items() if e == "whisper"]
    return spotters, engines, unsupported


def feed_all(spotters, pcm, now, emit):
    """Feeds one audio frame to every spotter.

    Runs on a worker thread: a Vosk decode of a busy box can take tens of
    milliseconds, and blocking the event loop stalls the keepalive ping/pong
    until the client drops the stream.
    """
    for spotter in spotters:
        spotter.feed(pcm, now, emit)


async def handler(ws):
    loop = asyncio.get_running_loop()
    peer = ws.remote_address
    spotters = []
    last = -COOLDOWN
    # One worker per connection: frames stay in order, and a slow verify in
    # vosk_pool cannot delay the audio path.
    feed_pool = concurrent.futures.ThreadPoolExecutor(max_workers=1, thread_name_prefix="wake-feed")

    def emit(keyword, engine, score, at):
        """Reports a keyword heard in the audio received at time at."""

        def send():
            # Engines report the same word at different delays (Whisper can
            # lag by seconds, and also hears a Russian «Антон» as «אנטון»):
            # compare when the audio was heard, not when the result came.
            nonlocal last
            if at < last + COOLDOWN:
                return
            last = at
            log.info("%s: heard %r (%s, %s)", peer, keyword, engine, score)
            msg = {"keyword": keyword, "engine": engine}
            if score is not None:
                msg["score"] = score
            asyncio.ensure_future(ws.send(json.dumps(msg, ensure_ascii=False)))

        loop.call_soon_threadsafe(send)

    try:
        async for msg in ws:
            if isinstance(msg, str):
                try:
                    cfg = json.loads(msg)
                    keywords = [str(k).strip().lower() for k in cfg.get("keywords", []) if str(k).strip()]
                    rate = int(cfg.get("sample_rate", 16000))
                    spotters, engines, unsupported = await loop.run_in_executor(None, build, keywords, rate)
                except Exception as e:  # noqa: BLE001 - reported to the server
                    log.exception("configure")
                    await ws.send(json.dumps({"error": str(e)}))
                    return
                log.info("%s: keywords %s", peer, engines)
                await ws.send(json.dumps({"ready": True, "engines": engines, "unsupported": unsupported},
                                         ensure_ascii=False))
                continue
            now = time.monotonic()
            await loop.run_in_executor(feed_pool, feed_all, spotters, msg, now, emit)
    finally:
        feed_pool.shutdown(wait=False, cancel_futures=True)


async def main():
    host, _, port = os.environ.get("WAKE_LISTEN", "0.0.0.0:2700").rpartition(":")
    vosk_model()  # fail fast on a broken image
    async with serve(handler, host or "0.0.0.0", int(port), max_size=1 << 20,
                      ping_interval=20, ping_timeout=60):
        log.info("listening on %s:%s", host, port)
        await asyncio.get_running_loop().create_future()


if __name__ == "__main__":
    logging.basicConfig(level=os.environ.get("WAKE_LOG_LEVEL", "INFO"),
                        format="%(asctime)s %(levelname)s %(message)s")
    asyncio.run(main())
