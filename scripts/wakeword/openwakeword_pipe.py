#!/usr/bin/env python3
# Copyright 2026 The Marusya-Gemini Authors.
# SPDX-License-Identifier: Apache-2.0
"""Keyword spotter for marusya-agent's -wake_command based on openWakeWord.

Reads 16 kHz mono S16LE PCM on stdin and prints the model name on every
detection. Useful on boards that can run Python/ONNX; on the stock speaker
firmware use a native engine with the same stdin/stdout contract.

    pip install openwakeword numpy
    marusya-agent -wake_command "python3 openwakeword_pipe.py --model hey_jarvis"
"""

import argparse
import sys

import numpy as np
from openwakeword.model import Model

CHUNK = 1280  # 80 ms at 16 kHz


def main() -> None:
    p = argparse.ArgumentParser()
    p.add_argument("--model", default="hey_jarvis", help="model name or .onnx/.tflite path")
    p.add_argument("--threshold", type=float, default=0.5)
    args = p.parse_args()

    model = Model(wakeword_models=[args.model], inference_framework="onnx")
    stdin = sys.stdin.buffer
    while True:
        buf = stdin.read(CHUNK * 2)
        if len(buf) < CHUNK * 2:
            return
        scores = model.predict(np.frombuffer(buf, dtype=np.int16))
        for name, score in scores.items():
            if score >= args.threshold:
                print(name, flush=True)
                model.reset()


if __name__ == "__main__":
    main()
