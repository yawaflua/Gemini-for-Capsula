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

// Marusya plugin SDK for C++17: a single header without dependencies.
//
// Marusya starts the plugin and talks JSON-RPC 2.0 over its stdin and
// stdout, one message per line (see pkg/pluginsdk/protocol.go). Do not
// write to stdout yourself: use Plugin::log or std::cerr.
//
//   #include "marusya_plugin.hpp"
//   using marusya::Json;
//
//   int main() {
//     marusya::Plugin p;
//     p.tool({"coin_flip", "Flips a coin and returns heads or tails.", {}},
//            [](const marusya::Call&) { return Json::object({{"side", "heads"}}); });
//     return p.run();
//   }
//
// Build: g++ -std=c++17 -O2 -pthread plugin.cpp -o plugin
// (for the speaker: aarch64-linux-gnu-g++ ... -static).

#ifndef MARUSYA_PLUGIN_HPP
#define MARUSYA_PLUGIN_HPP

#include <atomic>
#include <cctype>
#include <chrono>
#include <cmath>
#include <condition_variable>
#include <cstdint>
#include <cstdio>
#include <functional>
#include <iostream>
#include <locale>
#include <map>
#include <memory>
#include <mutex>
#include <sstream>
#include <stdexcept>
#include <string>
#include <thread>
#include <utility>
#include <variant>
#include <vector>

namespace marusya {

constexpr int kProtocolVersion = 1;

// Json is a JSON value.
class Json {
 public:
  using Array = std::vector<Json>;
  using Object = std::map<std::string, Json>;

  Json() : v_(nullptr) {}
  Json(std::nullptr_t) : v_(nullptr) {}
  Json(bool b) : v_(b) {}
  Json(int n) : v_(static_cast<double>(n)) {}
  Json(long n) : v_(static_cast<double>(n)) {}
  Json(long long n) : v_(static_cast<double>(n)) {}
  Json(double n) : v_(n) {}
  Json(const char* s) : v_(std::string(s)) {}
  Json(std::string s) : v_(std::move(s)) {}
  Json(Array a) : v_(std::move(a)) {}
  Json(Object o) : v_(std::move(o)) {}

  static Json object(std::initializer_list<std::pair<const std::string, Json>> kv = {}) { return Json(Object(kv)); }
  static Json array(std::initializer_list<Json> xs = {}) { return Json(Array(xs)); }

  bool is_null() const { return std::holds_alternative<std::nullptr_t>(v_); }
  bool is_bool() const { return std::holds_alternative<bool>(v_); }
  bool is_number() const { return std::holds_alternative<double>(v_); }
  bool is_string() const { return std::holds_alternative<std::string>(v_); }
  bool is_array() const { return std::holds_alternative<Array>(v_); }
  bool is_object() const { return std::holds_alternative<Object>(v_); }

  bool as_bool(bool def = false) const { return is_bool() ? std::get<bool>(v_) : def; }
  double as_number(double def = 0) const { return is_number() ? std::get<double>(v_) : def; }
  long long as_int(long long def = 0) const { return is_number() ? static_cast<long long>(std::get<double>(v_)) : def; }
  std::string as_string(const std::string& def = "") const { return is_string() ? std::get<std::string>(v_) : def; }
  const Array& as_array() const {
    static const Array empty;
    return is_array() ? std::get<Array>(v_) : empty;
  }
  const Object& as_object() const {
    static const Object empty;
    return is_object() ? std::get<Object>(v_) : empty;
  }

  // Member access; a missing key reads as null.
  const Json& operator[](const std::string& key) const {
    static const Json null;
    if (!is_object()) return null;
    auto& o = std::get<Object>(v_);
    auto it = o.find(key);
    return it == o.end() ? null : it->second;
  }
  Json& operator[](const std::string& key) {
    if (!is_object()) v_ = Object{};
    return std::get<Object>(v_)[key];
  }
  bool contains(const std::string& key) const { return is_object() && std::get<Object>(v_).count(key) > 0; }
  void push_back(Json x) {
    if (!is_array()) v_ = Array{};
    std::get<Array>(v_).push_back(std::move(x));
  }

  std::string dump() const {
    std::string out;
    write(out);
    return out;
  }

  static Json parse(const std::string& s) {
    size_t i = 0;
    Json v = parse_value(s, i);
    skip_ws(s, i);
    if (i != s.size()) throw std::runtime_error("json: trailing data");
    return v;
  }

 private:
  std::variant<std::nullptr_t, bool, double, std::string, Array, Object> v_;

  static void write_string(std::string& out, const std::string& s) {
    out += '"';
    for (unsigned char c : s) {
      switch (c) {
        case '"': out += "\\\""; break;
        case '\\': out += "\\\\"; break;
        case '\n': out += "\\n"; break;
        case '\r': out += "\\r"; break;
        case '\t': out += "\\t"; break;
        default:
          if (c < 0x20) {
            char buf[8];
            std::snprintf(buf, sizeof buf, "\\u%04x", c);
            out += buf;
          } else {
            out += static_cast<char>(c);  // UTF-8 passes through
          }
      }
    }
    out += '"';
  }

  void write(std::string& out) const {
    if (is_null()) {
      out += "null";
    } else if (is_bool()) {
      out += std::get<bool>(v_) ? "true" : "false";
    } else if (is_number()) {
      double d = std::get<double>(v_);
      if (!std::isfinite(d)) {
        out += "null";
      } else if (d == std::floor(d) && std::fabs(d) < 1e15) {
        out += std::to_string(static_cast<long long>(d));
      } else {
        char buf[32];
        std::snprintf(buf, sizeof buf, "%.17g", d);
        out += buf;
      }
    } else if (is_string()) {
      write_string(out, std::get<std::string>(v_));
    } else if (is_array()) {
      out += '[';
      bool first = true;
      for (auto& x : std::get<Array>(v_)) {
        if (!first) out += ',';
        first = false;
        x.write(out);
      }
      out += ']';
    } else {
      out += '{';
      bool first = true;
      for (auto& [k, x] : std::get<Object>(v_)) {
        if (!first) out += ',';
        first = false;
        write_string(out, k);
        out += ':';
        x.write(out);
      }
      out += '}';
    }
  }

  static void skip_ws(const std::string& s, size_t& i) {
    while (i < s.size() && (s[i] == ' ' || s[i] == '\t' || s[i] == '\n' || s[i] == '\r')) i++;
  }

  static void expect(const std::string& s, size_t& i, const char* word) {
    for (const char* p = word; *p; p++, i++) {
      if (i >= s.size() || s[i] != *p) throw std::runtime_error("json: unexpected token");
    }
  }

  static void put_utf8(std::string& out, uint32_t cp) {
    if (cp < 0x80) {
      out += static_cast<char>(cp);
    } else if (cp < 0x800) {
      out += static_cast<char>(0xC0 | (cp >> 6));
      out += static_cast<char>(0x80 | (cp & 0x3F));
    } else if (cp < 0x10000) {
      out += static_cast<char>(0xE0 | (cp >> 12));
      out += static_cast<char>(0x80 | ((cp >> 6) & 0x3F));
      out += static_cast<char>(0x80 | (cp & 0x3F));
    } else {
      out += static_cast<char>(0xF0 | (cp >> 18));
      out += static_cast<char>(0x80 | ((cp >> 12) & 0x3F));
      out += static_cast<char>(0x80 | ((cp >> 6) & 0x3F));
      out += static_cast<char>(0x80 | (cp & 0x3F));
    }
  }

  static uint32_t hex4(const std::string& s, size_t& i) {
    if (i + 4 > s.size()) throw std::runtime_error("json: bad \\u escape");
    uint32_t v = 0;
    for (int k = 0; k < 4; k++, i++) {
      char c = s[i];
      v <<= 4;
      if (c >= '0' && c <= '9') v |= c - '0';
      else if (c >= 'a' && c <= 'f') v |= c - 'a' + 10;
      else if (c >= 'A' && c <= 'F') v |= c - 'A' + 10;
      else throw std::runtime_error("json: bad \\u escape");
    }
    return v;
  }

  static std::string parse_string(const std::string& s, size_t& i) {
    i++;  // opening quote
    std::string out;
    while (true) {
      if (i >= s.size()) throw std::runtime_error("json: unterminated string");
      char c = s[i++];
      if (c == '"') return out;
      if (c != '\\') {
        out += c;
        continue;
      }
      if (i >= s.size()) throw std::runtime_error("json: bad escape");
      char e = s[i++];
      switch (e) {
        case '"': out += '"'; break;
        case '\\': out += '\\'; break;
        case '/': out += '/'; break;
        case 'b': out += '\b'; break;
        case 'f': out += '\f'; break;
        case 'n': out += '\n'; break;
        case 'r': out += '\r'; break;
        case 't': out += '\t'; break;
        case 'u': {
          uint32_t cp = hex4(s, i);
          if (cp >= 0xD800 && cp < 0xDC00 && i + 1 < s.size() && s[i] == '\\' && s[i + 1] == 'u') {
            i += 2;
            uint32_t lo = hex4(s, i);
            cp = 0x10000 + ((cp - 0xD800) << 10) + (lo - 0xDC00);
          }
          put_utf8(out, cp);
          break;
        }
        default: throw std::runtime_error("json: bad escape");
      }
    }
  }

  static Json parse_value(const std::string& s, size_t& i) {
    skip_ws(s, i);
    if (i >= s.size()) throw std::runtime_error("json: unexpected end");
    char c = s[i];
    if (c == '{') {
      i++;
      Object o;
      skip_ws(s, i);
      if (i < s.size() && s[i] == '}') { i++; return Json(std::move(o)); }
      while (true) {
        skip_ws(s, i);
        if (i >= s.size() || s[i] != '"') throw std::runtime_error("json: expected key");
        std::string k = parse_string(s, i);
        skip_ws(s, i);
        if (i >= s.size() || s[i] != ':') throw std::runtime_error("json: expected ':'");
        i++;
        o[k] = parse_value(s, i);
        skip_ws(s, i);
        if (i < s.size() && s[i] == ',') { i++; continue; }
        if (i < s.size() && s[i] == '}') { i++; return Json(std::move(o)); }
        throw std::runtime_error("json: expected ',' or '}'");
      }
    }
    if (c == '[') {
      i++;
      Array a;
      skip_ws(s, i);
      if (i < s.size() && s[i] == ']') { i++; return Json(std::move(a)); }
      while (true) {
        a.push_back(parse_value(s, i));
        skip_ws(s, i);
        if (i < s.size() && s[i] == ',') { i++; continue; }
        if (i < s.size() && s[i] == ']') { i++; return Json(std::move(a)); }
        throw std::runtime_error("json: expected ',' or ']'");
      }
    }
    if (c == '"') return Json(parse_string(s, i));
    if (c == 't') { expect(s, i, "true"); return Json(true); }
    if (c == 'f') { expect(s, i, "false"); return Json(false); }
    if (c == 'n') { expect(s, i, "null"); return Json(); }
    size_t start = i;
    while (i < s.size() && (std::isdigit(static_cast<unsigned char>(s[i])) || s[i] == '-' || s[i] == '+' ||
                            s[i] == '.' || s[i] == 'e' || s[i] == 'E'))
      i++;
    if (start == i) throw std::runtime_error("json: unexpected character");
    std::istringstream in(s.substr(start, i - start));
    in.imbue(std::locale::classic());
    double d;
    in >> d;
    if (in.fail()) throw std::runtime_error("json: bad number");
    return Json(d);
  }
};

// Error is answered to the host with its code.
struct Error : std::runtime_error {
  int code;
  explicit Error(const std::string& msg, int code = -32000) : std::runtime_error(msg), code(code) {}
};

// Param is one argument of a tool. type is "string", "integer", "number"
// or "boolean".
struct Param {
  std::string name;
  std::string type;
  std::string description;
  bool required = false;
  std::vector<std::string> enum_values = {};
};

// ToolSpec declares a function for the language model.
struct ToolSpec {
  std::string name;
  std::string description;
  std::vector<Param> params;
};

// Call is a tool call.
struct Call {
  std::string name;
  Json args;
  Json device;  // id, language, location, timezone
  std::string device_id() const { return device["id"].as_string(); }
};

class Plugin {
 public:
  using ToolFunc = std::function<Json(const Call&)>;
  using EventFunc = std::function<void(const Json& event)>;  // kind, device, data
  using MessageFunc = std::function<void(const std::string& device, const Json& data)>;
  using InitFunc = std::function<void(const Json& host)>;

  // tool registers a function for the language model.
  void tool(ToolSpec spec, ToolFunc f) {
    std::lock_guard<std::mutex> l(mu_);
    handlers_[spec.name] = std::move(f);
    tools_.push_back(std::move(spec));
  }
  // on_init runs before anything else; host is the initialize request.
  void on_init(InitFunc f) { on_init_ = std::move(f); }
  // on_event subscribes to events of kinds ("*" for all).
  void on_event(EventFunc f, std::vector<std::string> kinds = {"*"}) {
    on_event_ = std::move(f);
    events_ = std::move(kinds);
  }
  // on_message receives data from the other half of the plugin.
  void on_message(MessageFunc f) { on_message_ = std::move(f); }

  // host and config are known after initialization.
  const Json& host() const { return host_; }
  const Json& config() const { return host_["config"]; }

  // command runs a speaker action, e.g. command("volume.set", {{"level", 30}}).
  Json command(const std::string& action, Json args = Json::object(), const std::string& device = "") {
    return call("host.command", Json::object({{"device", device}, {"action", action}, {"args", std::move(args)}}));
  }
  // say speaks text (server only); with listen returns the answer.
  std::string say(const std::string& text, const std::string& device = "", bool listen = false) {
    Json r = call("host.say", Json::object({{"device", device}, {"text", text}, {"listen", listen}}),
                  std::chrono::seconds(120));
    return r["answer"].as_string();
  }
  // send sends data to the other half of the plugin.
  void send(Json data, const std::string& device = "") {
    call("plugin.send", Json::object({{"device", device}, {"data", std::move(data)}}));
  }
  // log writes to the host log; level is debug, info, warn or error.
  void log(const std::string& message, const std::string& level = "info") {
    write(Json::object({{"jsonrpc", "2.0"}, {"method", "log"},
                        {"params", Json::object({{"level", level}, {"message", message}})}}));
  }

  // call sends a request to the host and waits for its result.
  Json call(const std::string& method, Json params,
            std::chrono::milliseconds timeout = std::chrono::seconds(30)) {
    auto w = std::make_shared<Waiter>();
    long long id;
    {
      std::lock_guard<std::mutex> l(mu_);
      id = next_id_++;
      pending_[id] = w;
    }
    write(Json::object({{"jsonrpc", "2.0"}, {"id", id}, {"method", method}, {"params", std::move(params)}}));
    std::unique_lock<std::mutex> l(w->mu);
    if (!w->cv.wait_for(l, timeout, [&] { return w->done; })) {
      std::lock_guard<std::mutex> g(mu_);
      pending_.erase(id);
      throw Error(method + ": timed out");
    }
    if (w->reply.contains("error") && !w->reply["error"].is_null()) {
      const Json& e = w->reply["error"];
      throw Error(e["message"].as_string("error"), static_cast<int>(e["code"].as_int(-32000)));
    }
    return w->reply["result"];
  }

  // run serves the host until it closes stdin or asks to shut down.
  int run() {
    std::ios::sync_with_stdio(false);
    std::string line;
    while (!stop_ && std::getline(std::cin, line)) {
      if (line.empty()) continue;
      Json msg;
      try {
        msg = Json::parse(line);
      } catch (const std::exception& e) {
        write(Json::object({{"jsonrpc", "2.0"}, {"id", nullptr},
                            {"error", Json::object({{"code", -32700}, {"message", e.what()}})}}));
        continue;
      }
      if (msg["method"].is_string()) {
        running_++;
        std::thread([this, msg] {
          serve(msg);
          running_--;
        }).detach();
      } else if (msg["id"].is_number()) {
        std::shared_ptr<Waiter> w;
        {
          std::lock_guard<std::mutex> l(mu_);
          auto it = pending_.find(msg["id"].as_int());
          if (it != pending_.end()) {
            w = it->second;
            pending_.erase(it);
          }
        }
        if (w) {
          std::lock_guard<std::mutex> l(w->mu);
          w->reply = msg;
          w->done = true;
          w->cv.notify_all();
        }
      }
    }
    // Let the handlers finish (shutdown answers before we get here).
    for (int i = 0; i < 300 && running_ > 0; i++) std::this_thread::sleep_for(std::chrono::milliseconds(10));
    return 0;
  }

 private:
  struct Waiter {
    std::mutex mu;
    std::condition_variable cv;
    bool done = false;
    Json reply;
  };

  void write(const Json& msg) {
    std::string s = msg.dump();
    s += '\n';
    std::lock_guard<std::mutex> l(wmu_);
    std::fwrite(s.data(), 1, s.size(), stdout);
    std::fflush(stdout);
  }

  void serve(const Json& msg) {
    const Json& id = msg["id"];
    try {
      Json result = handle(msg["method"].as_string(), msg["params"]);
      if (!id.is_null()) write(Json::object({{"jsonrpc", "2.0"}, {"id", id}, {"result", result}}));
    } catch (const Error& e) {
      if (!id.is_null()) reply_error(id, e.code, e.what());
      else std::cerr << e.what() << std::endl;
    } catch (const std::exception& e) {
      if (!id.is_null()) reply_error(id, -32000, e.what());
      else std::cerr << e.what() << std::endl;
    }
  }

  void reply_error(const Json& id, int code, const std::string& message) {
    write(Json::object({{"jsonrpc", "2.0"}, {"id", id},
                        {"error", Json::object({{"code", code}, {"message", message}})}}));
  }

  static Json spec_json(const ToolSpec& t) {
    Json params = Json::array();
    for (auto& p : t.params) {
      Json j = Json::object({{"name", p.name}, {"type", p.type}});
      if (!p.description.empty()) j["description"] = p.description;
      if (p.required) j["required"] = true;
      if (!p.enum_values.empty()) {
        Json e = Json::array();
        for (auto& v : p.enum_values) e.push_back(v);
        j["enum"] = e;
      }
      params.push_back(j);
    }
    return Json::object({{"name", t.name}, {"description", t.description}, {"params", params}});
  }

  Json handle(const std::string& method, const Json& params) {
    if (method == "initialize") {
      host_ = params;
      if (on_init_) on_init_(params);
      Json tools = Json::array(), events = Json::array();
      {
        std::lock_guard<std::mutex> l(mu_);
        for (auto& t : tools_) tools.push_back(spec_json(t));
      }
      for (auto& e : events_) events.push_back(e);
      return Json::object({{"protocol", kProtocolVersion}, {"tools", tools}, {"events", events}});
    }
    if (method == "tool.call") {
      Call c{params["name"].as_string(), params["args"], params["device"]};
      ToolFunc f;
      {
        std::lock_guard<std::mutex> l(mu_);
        auto it = handlers_.find(c.name);
        if (it == handlers_.end()) throw Error("unknown tool " + c.name);
        f = it->second;
      }
      return f(c);
    }
    if (method == "event") {
      if (on_event_) on_event_(params);
      return Json();
    }
    if (method == "message") {
      if (on_message_) on_message_(params["device"].as_string(), params["data"]);
      return Json();
    }
    if (method == "shutdown") {
      stop_ = true;
      return Json();
    }
    throw Error("method not found: " + method, -32601);
  }

  std::mutex mu_, wmu_;
  std::vector<ToolSpec> tools_;
  std::map<std::string, ToolFunc> handlers_;
  std::vector<std::string> events_;
  EventFunc on_event_;
  MessageFunc on_message_;
  InitFunc on_init_;
  Json host_;
  long long next_id_ = 1;
  std::map<long long, std::shared_ptr<Waiter>> pending_;
  std::atomic<bool> stop_{false};
  std::atomic<int> running_{0};
};

}  // namespace marusya

#endif  // MARUSYA_PLUGIN_HPP
