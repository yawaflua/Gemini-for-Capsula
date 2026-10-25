# Copyright 2026 The Marusya-Gemini Authors.
# SPDX-License-Identifier: Apache-2.0

# Wake word spotter for marusya-server (MARUSYA_WAKE_URL=ws://marusya-wake:2700).
# Russian keywords: Vosk small-ru with a one-word grammar (bundled, 45 MB).
# Hebrew and Latin keywords: faster-whisper, downloaded into /cache on first use.

FROM python:3.12-slim
ARG VOSK_MODEL=vosk-model-small-ru-0.22
RUN apt-get update && apt-get install -y --no-install-recommends curl unzip ca-certificates \
    && rm -rf /var/lib/apt/lists/*
WORKDIR /app
COPY requirements.txt .
RUN pip install --no-cache-dir -r requirements.txt
RUN mkdir -p /models && curl -fsSL "https://alphacephei.com/vosk/models/${VOSK_MODEL}.zip" -o /tmp/model.zip \
    && unzip -q /tmp/model.zip -d /models && rm /tmp/model.zip
COPY wake_server.py .
RUN useradd --uid 10001 --create-home wake && mkdir -p /cache && chown wake /cache
USER wake
ENV VOSK_MODEL=/models/${VOSK_MODEL} \
    HF_HOME=/cache/hf \
    WAKE_LISTEN=0.0.0.0:2700
EXPOSE 2700
CMD ["python", "-u", "wake_server.py"]
