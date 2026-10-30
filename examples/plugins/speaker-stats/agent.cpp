// Copyright 2026 The Marusya-Gemini Authors.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     https://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

// The speaker half of speaker-stats, written with the C++ SDK. It tells the
// model how long the speaker has been up and counts how often it was woken,
// reporting the count to the server half.

#include <atomic>
#include <fstream>

#include "marusya_plugin.hpp"

using marusya::Json;

static double read_first_number(const char* path) {
  std::ifstream in(path);
  double v = 0;
  in >> v;
  return v;
}

int main() {
  marusya::Plugin p;
  std::atomic<long long> wakes{0};

  p.tool({"speaker_uptime", "Returns how long the speaker has been running since boot and its load average.", {}},
         [&](const marusya::Call&) {
           double up = read_first_number("/proc/uptime");
           return Json::object({{"uptime_minutes", static_cast<long long>(up / 60)},
                                {"load_average", read_first_number("/proc/loadavg")},
                                {"wakes_since_start", static_cast<long long>(wakes)}});
         });

  p.on_event(
      [&](const Json& e) {
        if (e["data"]["state"].as_string() != "listening") return;
        long long n = ++wakes;
        try {
          p.send(Json::object({{"wakes", n}}));
        } catch (const std::exception& err) {
          p.log(std::string("send to server: ") + err.what(), "debug");
        }
      },
      {"state"});

  return p.run();
}
