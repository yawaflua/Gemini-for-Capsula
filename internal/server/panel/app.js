// Marusya web panel. No build step, no dependencies. Everything user- or
// device-provided is inserted with textContent.
"use strict";

const $ = (sel, root = document) => root.querySelector(sel);
const $$ = (sel, root = document) => [...root.querySelectorAll(sel)];

const state = {
  info: null,       // /api/state without devices and history
  devices: [],
  history: [],
  selected: null,   // session ID of the selected device
  filter: "all",
  deviceFilter: "",
  es: null,
  live: {},         // per session: {volume, muted, media, bt}
  providers: null,  // /api/providers
  editing: null,    // ID of the provider being edited, "" for a new one
};

// ---------- API ----------

class HTTPError extends Error {
  constructor(status, message) { super(message); this.status = status; }
}

async function api(method, path, body) {
  const opts = { method, headers: { "X-Marusya-Panel": "1" }, credentials: "same-origin" };
  if (body !== undefined) {
    opts.headers["Content-Type"] = "application/json";
    opts.body = JSON.stringify(body);
  }
  const resp = await fetch("api/" + path, opts);
  let data = null;
  try { data = await resp.json(); } catch { /* empty body */ }
  if (resp.status === 401 && path !== "login") {
    showLogin();
    throw new HTTPError(401, "нужно войти");
  }
  if (!resp.ok) {
    const e = new HTTPError(resp.status, (data && data.error) || resp.statusText);
    e.output = data && data.output; // what the speaker printed, see install
    throw e;
  }
  return data;
}

const command = (session, action, args = {}) =>
  api("POST", `devices/${encodeURIComponent(session)}/command`, { action, args });

// ---------- helpers ----------

function el(tag, attrs = {}, ...children) {
  const e = document.createElement(tag);
  for (const [k, v] of Object.entries(attrs)) {
    if (k === "class") e.className = v;
    else if (k === "text") e.textContent = v;
    else if (k.startsWith("on")) e.addEventListener(k.slice(2), v);
    else if (v !== false && v != null) e.setAttribute(k, v);
  }
  for (const c of children) if (c != null) e.append(c);
  return e;
}

let toastTimer;
function toast(msg, isErr = false) {
  const t = $("#toast");
  t.textContent = msg;
  t.classList.toggle("err", isErr);
  t.hidden = false;
  clearTimeout(toastTimer);
  toastTimer = setTimeout(() => (t.hidden = true), isErr ? 5000 : 2500);
}

const fmtTime = (iso) => new Date(iso).toLocaleTimeString("ru-RU", { hour: "2-digit", minute: "2-digit", second: "2-digit" });
const fmtDate = (iso) => {
  const d = new Date(iso);
  const today = new Date();
  return d.toDateString() === today.toDateString() ? "сегодня" : d.toLocaleDateString("ru-RU", { day: "numeric", month: "short" });
};
function ago(iso) {
  const s = Math.max(0, (Date.now() - new Date(iso)) / 1000);
  if (s < 60) return "только что";
  if (s < 3600) return `${Math.floor(s / 60)} мин назад`;
  if (s < 86400) return `${Math.floor(s / 3600)} ч ${Math.floor((s % 3600) / 60)} мин назад`;
  return `${Math.floor(s / 86400)} дн назад`;
}
const stateName = { idle: "ждёт «Маруся»", listening: "слушает", thinking: "думает", announcing: "объявляет" };

const actionNames = {
  "volume.get": "узнать громкость", "volume.set": "громкость", "volume.adjust": "громкость ±",
  "volume.mute": "без звука", "media.control": "музыка", "media.play_url": "включить поток",
  "media.status": "что играет", "media.play_search": "найти и включить", "bluetooth.pair": "Bluetooth: сопряжение", "bluetooth.connect": "Bluetooth: подключить",
  "bluetooth.disconnect": "Bluetooth: отключить", "bluetooth.devices": "Bluetooth: список",
  "light.night": "ночник", "conversation.end": "завершить разговор", "time_now": "время", "alarm_set": "будильник", "alarm_list": "будильники", "alarm_delete": "удалить будильник", "weather_forecast": "погода",
  "home_find": "умный дом: найти", "home_control": "умный дом",
  "ring.set": "кольцо", "ring.reset": "кольцо как обычно", "display.show": "экран", "display.reset": "экран как обычно",
  "sensors.get": "датчики", "ring_set": "кольцо",
};

function deviceName(sessionOrDevice) {
  const d = typeof sessionOrDevice === "string" ? state.devices.find((x) => x.session === sessionOrDevice) : sessionOrDevice;
  return d ? d.device_id : "";
}

// ---------- login ----------

function showLogin() {
  if (state.es) { state.es.close(); state.es = null; }
  $("#app").hidden = true;
  $("#login").hidden = false;
  $("#login-password").focus();
}

$("#login-form").addEventListener("submit", async (e) => {
  e.preventDefault();
  const err = $("#login-error");
  err.hidden = true;
  const btn = $("button", e.target);
  btn.disabled = true;
  try {
    await api("POST", "login", { password: $("#login-password").value });
    $("#login-password").value = "";
    await boot();
  } catch (ex) {
    err.textContent = ex.message;
    err.hidden = false;
  } finally {
    btn.disabled = false;
  }
});

$("#logout").addEventListener("click", async () => {
  try { await api("POST", "logout"); } catch { /* ignore */ }
  showLogin();
});

// ---------- tabs ----------

function selectTab(name) {
  for (const t of $$(".tab")) t.setAttribute("aria-selected", t.dataset.tab === name);
  for (const p of $$(".tabpane")) p.hidden = p.id !== "tab-" + name;
  try { localStorage.setItem("marusya-tab", name); } catch { /* private mode */ }
}
for (const t of $$(".tab")) t.addEventListener("click", () => selectTab(t.dataset.tab));

// ---------- boot and live updates ----------

async function boot() {
  const st = await api("GET", "state");
  state.devices = st.devices;
  state.history = st.history;
  delete st.devices;
  delete st.history;
  state.info = st;
  if (!state.devices.some((d) => d.session === state.selected)) state.selected = state.devices[0]?.session ?? null;

  $("#login").hidden = true;
  $("#app").hidden = false;
  document.body.classList.toggle("tunable", st.tunable);
  renderServerLine();
  renderInstall();
  renderDevices();
  renderHistory();
  renderSettings();
  await loadProviders().catch(() => {});
  await loadPlugins().catch(() => {});
  connectEvents();
  let tab = "devices";
  try { tab = localStorage.getItem("marusya-tab") || tab; } catch { /* private mode */ }
  if ($(`.tab[data-tab="${tab}"]`)?.hidden !== false) tab = "devices";
  selectTab(tab);
}

function setLive(on) {
  const l = $("#live");
  l.classList.toggle("on", on);
  l.classList.toggle("off", !on);
  $("#live-text").textContent = on ? "онлайн" : "нет связи";
}

function connectEvents() {
  if (state.es) state.es.close();
  const es = new EventSource("api/events");
  state.es = es;
  es.onopen = () => setLive(true);
  es.onerror = async () => {
    setLive(false);
    if (es.readyState === EventSource.CLOSED) {
      // Probably logged out or the server restarted: reload everything.
      setTimeout(() => boot().catch(() => {}), 3000);
    }
  };
  es.addEventListener("devices", (e) => {
    state.devices = JSON.parse(e.data);
    if (!state.devices.some((d) => d.session === state.selected)) state.selected = state.devices[0]?.session ?? null;
    renderDevices();
    renderDeviceFilter();
  });
  es.addEventListener("history", (e) => {
    const ev = JSON.parse(e.data);
    state.history.push(ev);
    if (state.history.length > 500) state.history.shift();
    prependEvent(ev);
    // Settings changed in another tab: reload them unless being edited here.
    if (ev.kind === "settings" && $("#tab-settings").hidden) refreshSettings();
    if (ev.kind === "provider") { refreshInfo(); loadProviders().catch(() => {}); }
    if (["plugins", "connect", "disconnect"].includes(ev.kind)) loadPlugins().catch(() => {});
  });
}

// refreshInfo reloads what depends on the provider: its name, model, voice.
async function refreshInfo() {
  try {
    const st = await api("GET", "state");
    delete st.devices;
    delete st.history;
    state.info = st;
    document.body.classList.toggle("tunable", st.tunable);
    renderServerLine();
  renderInstall();
    renderSettings();
  } catch { /* ignore */ }
}

async function refreshSettings() {
  try {
    const st = await api("GET", "state");
    state.info.settings = st.settings;
    renderSettings();
  } catch { /* ignore */ }
}

function renderInstall() {
  $("#install-card").hidden = !state.info.install;
  const srv = $("#inst-server");
  const local = /^(localhost|127\.|\[::1\])/.test(location.hostname);
  srv.placeholder = `ws://<ip-сервера>:${location.port || 8080}/v1/stream`;
  // A speaker cannot reach this server as localhost.
  if (!srv.value && !local) srv.value = `${location.protocol === "https:" ? "wss" : "ws"}://${location.host}/v1/stream`;
  // Open the form once when there is nothing else to look at.
  if (!renderInstall.opened && !state.devices.length) $("#install-card").open = true;
  renderInstall.opened = true;
}

$("#install-form").addEventListener("submit", async (e) => {
  e.preventDefault();
  const err = $("#install-error"), log = $("#install-log"), status = $("#install-status");
  err.hidden = log.hidden = true;
  const body = {
    host: $("#inst-host").value,
    password: $("#inst-password").value,
    server: $("#inst-server").value,
    user: $("#inst-user").value,
    port: Number($("#inst-port").value) || 22,
    name: $("#inst-name").value,
    airplay: $("#inst-airplay").checked,
    spotify: $("#inst-spotify").checked,
  };
  const btn = $("button[type=submit]", e.target);
  btn.disabled = true;
  status.textContent = body.airplay || body.spotify
    ? "Устанавливаю… (несколько минут: колонка ставит пакеты)"
    : "Устанавливаю… (до минуты: колонка скачивает агента)";
  $("#spotify-login").hidden = true;
  status.hidden = false;
  try {
    const r = await api("POST", "install", body);
    $("#inst-password").value = "";
    status.textContent = `Готово, колонка скоро появится в списке. Ключ SSH колонки: ${r.host_key}`;
    if (r.output) { log.textContent = r.output; log.hidden = false; }
    if (r.spotify_login) {
      state.spotifyLogin = r.spotify_login.id;
      $("#sp-link").href = r.spotify_login.url;
      $("#sp-redirect").value = "";
      $("#sp-error").hidden = $("#sp-status").hidden = true;
      $("#spotify-login button[type=submit]").hidden = false;
      $("#spotify-login").hidden = false;
      $("#spotify-login").scrollIntoView({ behavior: "smooth", block: "nearest" });
    }
  } catch (ex) {
    status.hidden = true;
    err.textContent = ex.message;
    err.hidden = false;
    if (ex.output) { log.textContent = ex.output; log.hidden = false; }
  } finally {
    btn.disabled = false;
  }
});

const syncInstallName = () => {
  $("#inst-name-field").hidden = !$("#inst-airplay").checked && !$("#inst-spotify").checked;
};
$("#inst-airplay").addEventListener("change", syncInstallName);
$("#inst-spotify").addEventListener("change", syncInstallName);

$("#spotify-login").addEventListener("submit", async (e) => {
  e.preventDefault();
  const err = $("#sp-error"), status = $("#sp-status"), log = $("#install-log");
  err.hidden = true;
  const btn = $("button[type=submit]", e.target);
  btn.disabled = true;
  status.textContent = "Передаю вход колонке…";
  status.hidden = false;
  try {
    const r = await api("POST", "install/spotify", { id: state.spotifyLogin, redirect: $("#sp-redirect").value });
    status.textContent = "Готово: колонка вошла в Spotify.";
    if (r.output) { log.textContent += "\n\n== Вход в Spotify ==\n" + r.output; log.hidden = false; }
    btn.hidden = true;
  } catch (ex) {
    status.hidden = true;
    err.textContent = ex.message;
    err.hidden = false;
    if (ex.output) { log.textContent += "\n\n== Вход в Spotify ==\n" + ex.output; log.hidden = false; }
  } finally {
    btn.disabled = false;
  }
});

function renderServerLine() {
  const i = state.info;
  $("#server-line").textContent = `Сервер ${i.version || "dev"} · ${i.provider} · запущен ${fmtDate(i.started)} в ${fmtTime(i.started)}`;
}

// ---------- devices ----------

function renderDevices() {
  const list = $("#device-list");
  list.replaceChildren();
  for (const d of state.devices) {
    const item = el("button", { class: "device-item" + (d.session === state.selected ? " sel" : ""), onclick: () => selectDevice(d.session) },
      el("span", { class: "dot " + d.state }),
      el("strong", { text: d.device_id || "колонка" }),
      el("small", { text: `${stateName[d.state] || d.state} · ${d.remote.replace(/:\d+$/, "")}` }));
    list.append(item);
  }
  renderDetail();
}

function selectDevice(session) {
  if (state.selected === session) return;
  state.selected = session;
  $("#device-detail").replaceChildren(); // rebuild for the new device
  renderDevices();
}

function renderDetail() {
  const box = $("#device-detail");
  const d = state.devices.find((x) => x.session === state.selected);
  if (!d) {
    $("#device-list").hidden = true;
    box.replaceChildren(emptyDevices());
    return;
  }
  $("#device-list").hidden = false;

  // Build once per selected device, then only update the dynamic parts so
  // inputs keep their focus and values.
  if (box.dataset.session !== d.session || !box.firstElementChild || box.querySelector(".empty-state")) {
    box.replaceChildren($("#tpl-device-detail").content.cloneNode(true));
    box.dataset.session = d.session;
    wireDetail(box, d.session);
    fillProfile(box, d);
    loadAlarms(box, d.device_id);
    for (const r of $$("input[type=range]", box)) paintRange(r);
    loadLive(d.session);
  }
  const caps = new Set(d.capabilities);
  // data-cap lists actions; the element shows if the device has any of them.
  for (const c of $$("[data-cap]", box)) {
    c.hidden = c.dataset.cap === "say" ? false : !c.dataset.cap.split(" ").some((a) => caps.has(a));
  }
  const prof = profileOf(d);
  f(box, "name").textContent = prof.name ? `${prof.name} · ${d.device_id}` : d.device_id || "колонка";
  const badge = f(box, "state");
  badge.textContent = stateName[d.state] || d.state;
  badge.className = "badge " + d.state;

  const eff = d.effective || {};
  const langs = [eff.language || d.language || state.info.settings.language, eff.extra_languages].filter(Boolean).join(", ");
  const facts = [
    ["Адрес", d.remote.replace(/:\d+$/, "")],
    ["На связи", ago(d.connected)],
    ["Место", eff.location || d.location || "не задано"],
    ["Часовой пояс", eff.timezone || d.timezone || state.info.settings.default_timezone || "—"],
    ["Языки", langs],
    ["Вопросов", d.last_turn ? `${d.turns}, последний ${ago(d.last_turn)}` : String(d.turns)],
    ["Сессия модели", d.model_session ? "открыта (помнит разговор)" : "закрыта"],
  ];
  if (d.wake) {
    const words = state.info.settings.wake_words;
    facts.push(["Ключевое слово", {
      on: `слушает: ${words}`,
      off: "не задано",
      unavailable: "распознаватель недоступен",
    }[d.wake] || d.wake]);
  }
  f(box, "facts").replaceChildren(...facts.map(([k, v]) => el("div", {}, el("dt", { text: k }), el("dd", { text: v }))));

  const sayOff = f(box, "say-off");
  const sayForm = f(box, "say-form");
  if (!d.can_say) {
    sayOff.hidden = false;
    sayOff.textContent = !state.info.text
      ? "Этот провайдер не принимает текстовые вопросы."
      : "Агент на колонке старый и не умеет отвечать на вопросы из панели. Перезагрузите колонку, и она скачает новую версию с сервера.";
  } else {
    sayOff.hidden = true;
  }
  for (const c of sayForm.elements) c.disabled = !d.can_say || d.state !== "idle";

  const live = state.live[d.session] || {};
  if (live.volume != null) {
    const vol = f(box, "vol");
    if (document.activeElement !== vol) vol.value = live.volume;
    paintRange(vol);
    f(box, "vol-value").textContent = live.muted ? "без звука" : `${live.volume}%`;
  }
  const mute = f(box, "mute");
  mute.textContent = live.muted ? "Включить звук" : "Выключить звук";
  mute.classList.toggle("on", !!live.muted);
  f(box, "light-on").checked = !!live.light;
  if (live.media) {
    const via = live.media.source === "airplay" ? " · AirPlay" : "";
    f(box, "now").textContent = live.media.playing ? `▶ ${live.media.title || "играет"}${via}`
      : live.media.source === "airplay" ? `⏸ ${live.media.title || "AirPlay на паузе"}${via}` : "Ничего не играет";
  }
  renderBT(box, d.session, live.bt);
  renderSurface(box, live.surface);
}

const sceneNames = {
  solid: "ровный цвет", breathe: "дыхание", spin: "бегущий огонь", pulse: "пульс", rainbow: "радуга", off: "погашено",
  listening: "«слушаю»", thinking: "«думаю»", speaking: "«говорю»", announcing: "«объявляю»", error: "«ошибка»", disconnected: "«нет связи»",
};
const displayModeNames = { clock: "часы", off: "погашен", sleep: "спит", wake: "не засыпает", face: "лицо" };
const keyNames = { voice: "центральная", next: "следующий трек", mic: "микрофон", volume_up: "громче", volume_down: "тише" };

// left returns how long an override still lasts, e.g. "ещё 9 мин".
function left(o) {
  const s = Math.max(0, Math.round((Date.parse(o.until) - Date.now()) / 1000));
  if (s < 60) return `ещё ${s} с`;
  if (s < 3600) return `ещё ${Math.ceil(s / 60)} мин`;
  return `ещё ${Math.floor(s / 3600)} ч ${Math.round((s % 3600) / 60)} мин`;
}

// renderSurface shows the ring and display overrides and the sensors.
function renderSurface(box, s) {
  if (!s) return;
  const r = s.ring;
  f(box, "ring-now").textContent = r
    ? `Сейчас: ${sceneNames[r.scene] || r.scene}${r.color && ["solid", "breathe", "spin", "pulse"].includes(r.scene) ? " #" + r.color : ""}, ${left(r)}.`
    : "Сейчас кольцо показывает, что делает Маруся.";
  const dsp = s.display;
  f(box, "disp-now").textContent = dsp
    ? `Сейчас: ${dsp.mode === "text" ? `текст «${dsp.text}»` : displayModeNames[dsp.mode] || dsp.mode}${dsp.brightness ? `, яркость ${dsp.brightness}%` : ""}, ${left(dsp)}.`
    : "Сейчас на экране лицо Маруси.";
  const bri = f(box, "disp-bri");
  if (document.activeElement !== bri) {
    bri.value = dsp?.brightness || 0;
    paintRange(bri);
    f(box, "disp-bri-value").textContent = bri.value > 0 ? `${bri.value}%` : "по датчику";
  }
  f(box, "sf-asleep").textContent = s.has_display ? (s.asleep ? "лицо спит" : "") : "";
  const facts = [["Состояние", stateName[s.state] || s.state]];
  if (s.has_light) facts.push(["Освещённость", s.lux != null ? `${s.lux < 10 ? s.lux.toFixed(1) : Math.round(s.lux)} лк` : "—"]);
  if (s.has_display) facts.push(["Лицо", s.asleep ? "спит" : "не спит"]);
  facts.push(["Микрофон", s.mic_muted ? "выключен кнопкой" : "включён"]);
  if (s.key) facts.push(["Последняя кнопка", `${keyNames[s.key] || s.key}, ${ago(s.key_at)}`]);
  f(box, "sensors").replaceChildren(...facts.map(([k, v]) => el("div", {}, el("dt", { text: k }), el("dd", { text: v }))));
}

const f = (root, name) => root.querySelector(`[data-f="${name}"]`);

const profileOf = (d) => (state.info.profiles || {})[d.device_id] || {};

// fillProfile puts the stored profile of the device into its form.
function fillProfile(box, d) {
  const p = profileOf(d);
  f(box, "profile-id").textContent = d.device_id;
  f(box, "p-name").value = p.name || "";
  f(box, "p-location").value = p.location || "";
  f(box, "p-timezone").value = p.timezone || "";
  f(box, "p-language").value = p.language || "";
  f(box, "p-extra").value = p.extra_languages || "";
  f(box, "p-token").value = p.token || "";
  f(box, "p-del").disabled = !Object.keys(p).length;
  const form = f(box, "profile-form");
  form.hidden = !d.device_id;
}

const dayNames = ["вс", "пн", "вт", "ср", "чт", "пт", "сб"];

async function loadAlarms(box, deviceID) {
  if (!deviceID) return;
  let list = [];
  try { list = await api("GET", `alarms/${encodeURIComponent(deviceID)}`); } catch { return; }
  const ul = f(box, "alarms");
  ul.replaceChildren(...list.map((a) => {
    const when = a.days?.length ? a.days.map((d) => dayNames[d]).join(", ") : a.date;
    const extra = [a.label, a.music && `♪ ${a.music}`, a.volume && `${a.volume}%`].filter(Boolean).join(" · ");
    return el("li", {},
      el("span", { text: `${a.time.slice(0, 5)} — ${when}${extra ? " · " + extra : ""}` }),
      el("button", { class: "btn text small", text: "Удалить", onclick: async (e) => {
        if (await run(e.target, () => api("DELETE", `alarms/${encodeURIComponent(deviceID)}/${a.id}`))) loadAlarms(box, deviceID);
      } }));
  }));
  if (!list.length) ul.replaceChildren(el("li", { class: "muted", text: "Будильников нет" }));
}

async function saveProfile(box, deviceID, token) {
  const body = {
    name: f(box, "p-name").value.trim(),
    location: f(box, "p-location").value.trim(),
    timezone: f(box, "p-timezone").value.trim(),
    language: f(box, "p-language").value.trim(),
    extra_languages: f(box, "p-extra").value.trim(),
    token: token ?? f(box, "p-token").value.trim(),
  };
  const p = await api("PUT", `profiles/${encodeURIComponent(deviceID)}`, body);
  state.info.profiles = { ...(state.info.profiles || {}), [deviceID]: p };
  if (!Object.values(p).some(Boolean)) delete state.info.profiles[deviceID];
  return p;
}

// paintRange fills the active part of a Material slider track.
function paintRange(r) {
  r.style.setProperty("--val", `${((r.value - r.min) / (r.max - r.min)) * 100}%`);
}
document.addEventListener("input", (e) => { if (e.target.type === "range") paintRange(e.target); });

function emptyDevices() {
  return el("div", { class: "card empty-state" },
    el("div", { class: "orb" }),
    el("h2", { text: "Нет подключённых колонок" }),
    el("p", { class: "muted", text: state.info.install
      ? "Добавьте колонку формой ниже или укажите адрес этого сервера в /etc/conf.d/marusya-agent:"
      : "Колонка подключается сама, когда в /etc/conf.d/marusya-agent указан адрес этого сервера:" }),
    el("pre", { text: `MARUSYA_SERVER=ws://${location.host}/v1/stream\nMARUSYA_AUTH_TOKEN=<тот же, что на сервере>` }));
}

async function run(btn, fn, okMsg) {
  if (btn) btn.disabled = true;
  try {
    const r = await fn();
    if (okMsg) toast(okMsg);
    return r;
  } catch (ex) {
    if (ex.status !== 401) toast(ex.message, true);
  } finally {
    if (btn) btn.disabled = false;
  }
}

async function loadLive(session) {
  const d = state.devices.find((x) => x.session === session);
  if (!d) return;
  const caps = new Set(d.capabilities);
  const live = (state.live[session] ||= {});
  const jobs = [];
  if (caps.has("volume.get")) {
    jobs.push(command(session, "volume.get").then((r) => { live.volume = r.data.level; live.muted = r.data.muted; }));
  }
  if (caps.has("media.status")) {
    jobs.push(command(session, "media.status").then((r) => { live.media = r.data; }));
  }
  if (caps.has("sensors.get")) jobs.push(loadSurface(session));
  await Promise.allSettled(jobs);
  if (state.selected === session) renderDetail();
}

function wireDetail(box, session) {
  const live = () => (state.live[session] ||= {});

  f(box, "say-form").addEventListener("submit", async (e) => {
    e.preventDefault();
    const input = f(box, "say-text");
    const text = input.value.trim();
    if (!text) return;
    const r = await run($("button", e.target), () => api("POST", `devices/${encodeURIComponent(session)}/say`, { text }), "Спросила, слушайте колонку");
    if (r) input.value = "";
  });

  const deviceID = () => state.devices.find((x) => x.session === session)?.device_id;
  const refill = () => {
    const d = state.devices.find((x) => x.session === session);
    if (d) fillProfile(box, d);
    renderDetail();
  };
  f(box, "profile-form").addEventListener("submit", async (e) => {
    e.preventDefault();
    if (await run($("button[type=submit]", e.target), () => saveProfile(box, deviceID()), "Профиль сохранён")) refill();
  });
  f(box, "p-gen").addEventListener("click", async (e) => {
    const p = await run(e.target, () => saveProfile(box, deviceID(), "new"));
    if (p) {
      toast("Новый токен сохранён. Пропишите его в MARUSYA_AUTH_TOKEN на колонке — со старым она больше не войдёт");
      refill();
    }
  });
  f(box, "p-del").addEventListener("click", async (e) => {
    const id = deviceID();
    const ok = await run(e.target, () => api("DELETE", `profiles/${encodeURIComponent(id)}`), "Профиль удалён");
    if (ok !== undefined) {
      if (state.info.profiles) delete state.info.profiles[id];
      refill();
    }
  });

  f(box, "alarm-form").addEventListener("submit", async (e) => {
    e.preventDefault();
    const id = deviceID();
    const rep = f(box, "a-repeat").value;
    const body = {
      time: f(box, "a-time").value,
      days: rep ? rep.split(",").map(Number) : undefined,
      volume: Number(f(box, "a-volume").value) || 0,
      music: f(box, "a-music").value.trim(),
    };
    const a = await run($("button", e.target), () => api("POST", `alarms/${encodeURIComponent(id)}`, body));
    if (a) {
      toast(`Будильник на ${a.time} ${a.date ? a.date : "по расписанию"}`);
      loadAlarms(box, id);
    }
  });

  const vol = f(box, "vol");
  vol.addEventListener("input", () => (f(box, "vol-value").textContent = `${vol.value}%`));
  vol.addEventListener("change", async () => {
    const r = await run(null, () => command(session, "volume.set", { level: Number(vol.value) }));
    if (r) { live().volume = r.data.level; live().muted = false; renderDetail(); }
  });

  const act = {
    "vol-down": () => command(session, "volume.adjust", { delta: -10 }).then((r) => { live().volume = r.data.level; }),
    "vol-up": () => command(session, "volume.adjust", { delta: 10 }).then((r) => { live().volume = r.data.level; }),
    mute: () => command(session, "volume.mute", { muted: !live().muted }).then((r) => { live().muted = r.data.muted; }),
    "bt-pair": () => command(session, "bluetooth.pair", { timeout_seconds: 120 }).then(() => toast("Колонка видна по Bluetooth 2 минуты")),
    "bt-list": () => loadBT(session),
    "bt-off": () => command(session, "bluetooth.disconnect", {}).then(() => loadBT(session)),
    forget: () => api("POST", `devices/${encodeURIComponent(session)}/forget`).then(() => toast("Маруся забыла этот разговор")),
    refresh: () => loadLive(session),
  };
  for (const b of $$("[data-act]", box)) {
    b.addEventListener("click", async () => {
      await run(b, act[b.dataset.act]);
      renderDetail();
    });
  }
  // Night light: the switch turns it on or off; picking a colour or
  // brightness applies it at once (and turns the light on).
  const lightSwitch = f(box, "light-on");
  const setLight = (on) => {
    live().light = on;
    lightSwitch.checked = on;
    const args = on
      ? { on: true, color: f(box, "light-color").value.replace("#", ""), brightness: Number(f(box, "light-bri").value) }
      : { on: false };
    return run(null, () => command(session, "light.night", args));
  };
  let lightTimer;
  const lightSoon = () => {
    clearTimeout(lightTimer);
    lightTimer = setTimeout(() => setLight(true), 250);
  };
  lightSwitch.addEventListener("change", () => setLight(lightSwitch.checked));
  f(box, "light-color").addEventListener("input", lightSoon);
  f(box, "light-bri").addEventListener("input", (e) => {
    f(box, "light-bri-value").textContent = e.target.value;
    lightSoon();
  });

  // Ring and display: overrides that end by themselves after "Держать".
  const hold = () => Number(f(box, "sf-hold").value);
  const surfaceCmd = async (btn, action, args) => {
    const r = await run(btn, () => command(session, action, args));
    if (r) {
      await loadSurface(session).catch(() => {});
      renderDetail();
    }
    return r;
  };
  const ringArgs = () => ({
    scene: f(box, "ring-scene").value,
    color: f(box, "ring-color").value.replace("#", ""),
    brightness: Number(f(box, "ring-bri").value),
    seconds: hold(),
  });
  f(box, "ring-apply").addEventListener("click", (e) => surfaceCmd(e.target, "ring.set", ringArgs()));
  f(box, "ring-reset").addEventListener("click", (e) => surfaceCmd(e.target, "ring.reset", {}));
  // Like the night light, a new colour or brightness applies at once.
  let ringTimer;
  const ringSoon = () => {
    clearTimeout(ringTimer);
    ringTimer = setTimeout(() => surfaceCmd(null, "ring.set", ringArgs()), 250);
  };
  f(box, "ring-color").addEventListener("input", ringSoon);
  f(box, "ring-scene").addEventListener("change", ringSoon);
  f(box, "ring-bri").addEventListener("input", (e) => {
    f(box, "ring-bri-value").textContent = e.target.value;
    ringSoon();
  });

  const dispBri = () => Number(f(box, "disp-bri").value);
  f(box, "disp-form").addEventListener("submit", async (e) => {
    e.preventDefault();
    const text = f(box, "disp-text").value.trim();
    if (!text) return;
    if (/[^\x20-\x7e]/.test(text)) {
      toast("Экран показывает только латиницу, цифры и знаки", true);
      return;
    }
    await surfaceCmd($("button", e.target), "display.show", { mode: "text", text, brightness: dispBri(), seconds: hold() });
  });
  for (const b of $$("[data-disp]", box)) {
    b.addEventListener("click", () => surfaceCmd(b, "display.show", { mode: b.dataset.disp, brightness: dispBri(), seconds: hold() }));
  }
  f(box, "disp-reset").addEventListener("click", (e) => surfaceCmd(e.target, "display.reset", {}));
  const bri = f(box, "disp-bri");
  bri.addEventListener("input", () => (f(box, "disp-bri-value").textContent = bri.value > 0 ? `${bri.value}%` : "по датчику"));
  // Brightness keeps what the display shows now (the face if nothing).
  bri.addEventListener("change", () => {
    const cur = live().surface?.display;
    if (!cur && dispBri() === 0) return;
    surfaceCmd(null, "display.show", { mode: cur?.mode || "face", text: cur?.text, brightness: dispBri(), seconds: hold() });
  });

  for (const b of $$("[data-media]", box)) {
    b.addEventListener("click", async () => {
      await run(b, () => command(session, "media.control", { operation: b.dataset.media }));
      setTimeout(() => loadLive(session), 700);
    });
  }
  f(box, "url-form").addEventListener("submit", async (e) => {
    e.preventDefault();
    const text = f(box, "url").value.trim();
    if (!text) return;
    const isURL = /^(https?|spotify):/i.test(text);
    const d = state.devices.find((x) => x.session === session);
    if (!isURL && !d?.capabilities.includes("media.play_search")) {
      toast("Колонка не умеет искать: обновите агент или вставьте ссылку", true);
      return;
    }
    toast(isURL ? "Включаю…" : "Ищу на YouTube…");
    const r = await run($("button", e.target), () =>
      isURL ? command(session, "media.play_url", { url: text }) : command(session, "media.play_search", { query: text }));
    if (r) toast(r.data?.title ? `Играет: ${r.data.title}` : "Включила");
    setTimeout(() => loadLive(session), 1500);
  });
}

async function loadSurface(session) {
  const r = await command(session, "sensors.get");
  (state.live[session] ||= {}).surface = r.data;
}

// The sensors change on their own: refresh them while the device is shown.
setInterval(async () => {
  const d = state.devices.find((x) => x.session === state.selected);
  if (!d || !d.capabilities.includes("sensors.get") || document.hidden || $("#tab-devices")?.hidden) return;
  try { await loadSurface(d.session); } catch { return; }
  if (state.selected === d.session) renderDetail();
}, 5000);

async function loadBT(session) {
  const r = await command(session, "bluetooth.devices");
  (state.live[session] ||= {}).bt = r.data.devices || [];
}

function renderBT(box, session, list) {
  const ul = f(box, "bt");
  if (!list) { ul.replaceChildren(); return; }
  if (!list.length) { ul.replaceChildren(el("li", {}, el("span", { class: "muted", text: "Нет сопряжённых устройств" }))); return; }
  ul.replaceChildren(...list.map((dv) => {
    const btn = el("button", { class: "btn small " + (dv.connected ? "outlined" : "tonal"), text: dv.connected ? "Отключить" : "Подключить" });
    btn.addEventListener("click", async () => {
      const action = dv.connected ? "bluetooth.disconnect" : "bluetooth.connect";
      await run(btn, () => command(session, action, { device: dv.address }).then(() => loadBT(session)));
      renderDetail();
    });
    return el("li", { class: dv.connected ? "connected" : "" },
      el("div", { class: "bt-text" },
        el("span", { class: "bt-name", text: dv.name || dv.address, title: dv.address }),
        el("small", { text: dv.connected ? "Подключено" : "Сопряжено" })),
      btn);
  }));
}

// ---------- history ----------

const groups = {
  turn: ["turn", "ask"],
  action: ["command", "skill"],
  system: ["connect", "disconnect", "error", "settings", "wake"],
};

function visible(ev) {
  if (state.filter !== "all" && !groups[state.filter].includes(ev.kind)) return false;
  if (state.deviceFilter && ev.device !== state.deviceFilter) return false;
  return true;
}

function renderDeviceFilter() {
  const sel = $("#history-device");
  const names = new Set(state.history.map((e) => e.device).filter(Boolean));
  for (const d of state.devices) names.add(d.device_id);
  const cur = state.deviceFilter;
  sel.replaceChildren(el("option", { value: "", text: "Все колонки" }), ...[...names].sort().map((n) => el("option", { value: n, text: n })));
  sel.value = names.has(cur) ? cur : "";
  sel.hidden = names.size < 2;
}

function renderHistory() {
  renderDeviceFilter();
  const feed = $("#feed");
  const items = state.history.filter(visible).reverse().map(renderEvent);
  feed.replaceChildren(...items);
  $("#feed-empty").hidden = items.length > 0;
}

function prependEvent(ev) {
  if (!visible(ev)) return;
  const feed = $("#feed");
  feed.prepend(renderEvent(ev));
  while (feed.children.length > 500) feed.lastElementChild.remove();
  $("#feed-empty").hidden = true;
  if (ev.device) renderDeviceFilter();
}

function argsText(args) {
  if (!args) return "";
  return Object.entries(args).map(([k, v]) => `${k}=${typeof v === "object" ? JSON.stringify(v) : v}`).join(", ");
}

function renderEvent(ev) {
  const meta = el("div", { class: "ev-meta" },
    el("span", { text: `${fmtDate(ev.time)}, ${fmtTime(ev.time)}` }),
    ev.device ? el("span", { class: "tag", text: ev.device }) : null,
    ev.source === "panel" ? el("span", { class: "tag panel", text: "из панели" }) : null,
    ev.error ? el("span", { class: "tag err", text: "ошибка" }) : null,
    ev.latency_ms ? el("span", { text: `${ev.latency_ms} мс` }) : null);

  switch (ev.kind) {
    case "turn":
      return el("li", { class: "ev" }, meta,
        ev.user ? el("div", { class: "bubble user" }, el("b", { text: ev.source === "panel" ? "Вы (текстом)" : "Человек" }), ev.user) : null,
        ev.model ? el("div", { class: "bubble model" }, el("b", { text: "Маруся" + (ev.cancelled ? " (прервали)" : "") }), ev.model) : null);
    case "ask": {
      const who = { claude: "Claude", homeassistant: "Home Assistant", api: "API" }[ev.source] || ev.source || "API";
      return el("li", { class: "ev" }, meta,
        el("div", { class: "bubble model" }, el("b", { text: `${who} через колонку` }), ev.model),
        ev.user ? el("div", { class: "bubble user" }, el("b", { text: "Ответ голосом" }), ev.user) : null,
        ev.error ? el("div", { class: "error", text: ev.error }) : null);
    }
    case "command":
    case "skill": {
      const name = actionNames[ev.name] || ev.name;
      const line = el("div", { class: "ev-line" },
        el("span", { text: (ev.kind === "skill" ? "Навык: " : "Команда: ") + name + " " }),
        ev.args && Object.keys(ev.args).length ? el("code", { text: argsText(ev.args) }) : null,
        ev.error ? el("div", { class: "error", text: ev.error }) : null);
      return el("li", { class: "ev" }, meta, line);
    }
    default: {
      if (ev.kind === "wake") {
        return el("li", { class: "ev compact" }, el("div", { class: "ev-meta" }, ...meta.childNodes,
          el("span", { text: `· Услышала «${ev.name}»` + (ev.result && ev.result.engine ? ` (${ev.result.engine})` : "") })));
      }
      const text = {
        connect: "Колонка подключилась",
        disconnect: "Колонка отключилась",
        settings: "Настройки изменены",
        provider: `Модель сменилась: ${ev.name || ""}`,
        error: ev.error || "Ошибка",
      }[ev.kind] || ev.kind;
      return el("li", { class: "ev compact" }, el("div", { class: "ev-meta" }, ...meta.childNodes, el("span", { text: "· " + text })));
    }
  }
}

for (const b of $$("#history-filter button")) {
  b.addEventListener("click", () => {
    state.filter = b.dataset.filter;
    for (const x of $$("#history-filter button")) x.classList.toggle("on", x === b);
    renderHistory();
  });
}
$("#history-device").addEventListener("change", (e) => {
  state.deviceFilter = e.target.value;
  renderHistory();
});

// ---------- settings ----------

function renderSettings() {
  const s = state.info.settings;
  $("#set-prompt").value = s.system_instruction || "";
  $("#set-location").value = s.default_location || "";
  $("#set-timezone").value = s.default_timezone || "";
  $("#set-language").value = s.language || "";
  $("#set-extra-languages").value = s.extra_languages || "";
  $("#set-voice").value = s.voice || "";
  $("#set-model").value = s.model || "";
  $("#set-wake").value = s.wake_words || "";
  $("#set-wake").disabled = !state.info.wake;
  $("#wake-hint").textContent = state.info.wake
    ? "Русские слова ищет Vosk (быстро), остальные (иврит, латиница) ищет Whisper. Пустое поле — слушать только кнопку и штатный детектор колонки."
    : "Распознаватель на сервере не подключён: задайте MARUSYA_WAKE_URL и запустите контейнер marusya-wake.";
  $("#voices").replaceChildren(...state.info.voices.map((v) => el("option", { value: v })));
  if (!$("#timezones").children.length && Intl.supportedValuesOf) {
    $("#timezones").append(...Intl.supportedValuesOf("timeZone").map((z) => el("option", { value: z })));
  }
  $("#settings-note").textContent = state.info.saved
    ? "Изменения сохраняются на сервере и переживут перезапуск."
    : "Изменения действуют до перезапуска сервера. Чтобы они сохранялись, задайте MARUSYA_SETTINGS_FILE.";
}

$("#settings-form").addEventListener("submit", async (e) => {
  e.preventDefault();
  const err = $("#settings-error");
  err.hidden = true;
  const body = {
    system_instruction: $("#set-prompt").value,
    default_location: $("#set-location").value,
    default_timezone: $("#set-timezone").value,
    language: $("#set-language").value,
    extra_languages: $("#set-extra-languages").value,
    voice: $("#set-voice").value,
    model: $("#set-model").value,
    wake_words: $("#set-wake").value,
  };
  const btn = $("button[type=submit]", e.target);
  btn.disabled = true;
  try {
    state.info.settings = await api("PUT", "settings", body);
    renderSettings();
    const ok = $("#settings-saved");
    ok.hidden = false;
    setTimeout(() => (ok.hidden = true), 2500);
  } catch (ex) {
    err.textContent = ex.message;
    err.hidden = false;
  } finally {
    btn.disabled = false;
  }
});

// Two-step reset instead of confirm(), which may be blocked.
const resetBtn = $("#settings-reset");
let resetArmed = null;
resetBtn.addEventListener("click", async () => {
  if (!resetArmed) {
    resetBtn.textContent = "Точно сбросить? Нажмите ещё раз";
    resetArmed = setTimeout(() => { resetArmed = null; resetBtn.textContent = "Вернуть как при запуске"; }, 4000);
    return;
  }
  clearTimeout(resetArmed);
  resetArmed = null;
  resetBtn.textContent = "Вернуть как при запуске";
  const r = await run(resetBtn, () => api("DELETE", "settings"), "Настройки сброшены");
  if (r) { state.info.settings = r; renderSettings(); }
});

// ---------- providers ----------

async function loadProviders() {
  const p = await api("GET", "providers");
  state.providers = p;
  $("#providers-tab").hidden = !p.enabled;
  if (p.enabled) renderProviders();
}

const kindOf = (k) => (state.providers.kinds || []).find((x) => x.kind === k);

function renderProviders() {
  const p = state.providers;
  $("#providers-note").textContent = (p.saved
    ? "Провайдеры и их ключи хранятся на сервере."
    : "Провайдеры действуют до перезапуска сервера. Чтобы они сохранялись, задайте MARUSYA_PROVIDERS_FILE.") +
    " Голосовые модели (Gemini Live, OpenAI Realtime) слушают и отвечают сами; текстовым нужны распознавание и синтез речи.";
  const list = $("#prov-list");
  list.replaceChildren();
  for (const v of p.providers) {
    const active = v.id === p.active;
    const kind = v.read_only ? "задан в server.env" : (kindOf(v.kind)?.label || v.kind);
    const details = v.read_only ? kind : [kind, v.model, v.base_url].filter(Boolean).join(" · ");
    const actions = el("div", { class: "row" },
      active ? el("span", { class: "badge idle", text: "используется" }) :
        el("button", { class: "btn primary small", type: "button", text: "Использовать", onclick: (e) => activateProvider(e.target, v.id) }),
      v.read_only ? null : el("button", { class: "btn text small", type: "button", text: "Изменить", onclick: () => editProvider(v) }),
      v.read_only || active ? null : el("button", { class: "btn text small", type: "button", text: "Удалить", onclick: (e) => deleteProvider(e.target, v) }));
    list.append(el("li", { class: "prov" + (active ? " active" : "") },
      el("div", { class: "prov-main" }, el("b", { text: v.name }), el("small", { text: details }),
        !v.read_only && !v.has_key ? el("small", { text: "без ключа" }) : null),
      actions));
  }
}

async function activateProvider(btn, id) {
  const r = await run(btn, () => api("POST", `providers/${encodeURIComponent(id)}/activate`), "Модель переключена");
  if (r) { state.providers = r; renderProviders(); refreshInfo(); }
}

// Two-step delete instead of confirm(), which may be blocked.
async function deleteProvider(btn, v) {
  if (btn.dataset.armed !== "1") {
    btn.dataset.armed = "1";
    btn.textContent = "Точно удалить?";
    setTimeout(() => { btn.dataset.armed = ""; btn.textContent = "Удалить"; }, 4000);
    return;
  }
  const r = await run(btn, () => api("DELETE", `providers/${encodeURIComponent(v.id)}`), "Провайдер удалён");
  if (r) { state.providers = r; renderProviders(); }
}

function fillKinds() {
  const sel = $("#prov-kind");
  if (sel.children.length) return;
  sel.append(...state.providers.kinds.map((k) => el("option", { value: k.kind, text: k.label })));
}

// updateProviderForm adapts the form to the chosen kind.
function updateProviderForm() {
  const k = kindOf($("#prov-kind").value);
  if (!k) return;
  $("#prov-url").placeholder = k.base_url;
  $("#prov-model").placeholder = k.model;
  $("#prov-speech-field").hidden = !k.pipeline;
  const cur = $("#prov-speech").value;
  const opts = [el("option", { value: "", text: "Gemini сервера (GEMINI_API_KEY)" })];
  if (k.kind === "openai") opts.push(el("option", { value: "self", text: "Этот же API (/audio/transcriptions и /audio/speech)" }));
  for (const v of state.providers.providers) {
    if (v.read_only || v.id === state.editing || !["gemini", "openai", "openai-realtime"].includes(v.kind)) continue;
    opts.push(el("option", { value: v.id, text: `Ключ провайдера «${v.name}»` }));
  }
  $("#prov-speech").replaceChildren(...opts);
  $("#prov-speech").value = [...$("#prov-speech").options].some((o) => o.value === cur) ? cur : "";
}

function editProvider(v) {
  fillKinds();
  state.editing = v ? v.id : "";
  $("#prov-form-title").textContent = v ? `Изменить «${v.name}»` : "Новый провайдер";
  $("#prov-id").value = v ? v.id : "";
  $("#prov-id").disabled = !!v;
  $("#prov-name").value = v ? v.name : "";
  $("#prov-kind").value = v ? v.kind : "openai";
  $("#prov-url").value = v ? v.base_url || "" : "";
  $("#prov-key").value = "";
  $("#prov-key").placeholder = v && v.has_key ? "Сохранён — оставьте пустым, чтобы не менять" : "";
  $("#prov-model").value = v ? v.model || "" : "";
  $("#prov-voice").value = v ? v.voice || "" : "";
  updateProviderForm();
  $("#prov-speech").value = v ? v.speech || "" : "";
  $("#prov-error").hidden = true;
  $("#prov-form").hidden = false;
  $("#prov-name").focus();
}

$("#prov-add").addEventListener("click", () => editProvider(null));
$("#prov-cancel").addEventListener("click", () => { $("#prov-form").hidden = true; state.editing = null; });
$("#prov-kind").addEventListener("change", updateProviderForm);

$("#prov-form").addEventListener("submit", async (e) => {
  e.preventDefault();
  const err = $("#prov-error");
  err.hidden = true;
  const id = $("#prov-id").value.trim().toLowerCase();
  const body = {
    name: $("#prov-name").value,
    kind: $("#prov-kind").value,
    base_url: $("#prov-url").value,
    api_key: $("#prov-key").value,
    model: $("#prov-model").value,
    voice: $("#prov-voice").value,
    speech: $("#prov-speech-field").hidden ? "" : $("#prov-speech").value,
  };
  const btn = $("button[type=submit]", e.target);
  btn.disabled = true;
  try {
    state.providers = await api("PUT", `providers/${encodeURIComponent(id)}`, body);
    renderProviders();
    $("#prov-form").hidden = true;
    state.editing = null;
    toast("Провайдер сохранён");
    if (state.providers.active === id) refreshInfo();
  } catch (ex) {
    err.textContent = ex.message;
    err.hidden = false;
  } finally {
    btn.disabled = false;
  }
});

// ---------- plugins ----------

const pluginStates = {
  running: ["idle", "работает"],
  starting: ["thinking", "запускается"],
  failed: ["failed", "упал, перезапуск"],
  disabled: ["off", "выключен"],
  stopped: ["off", "остановлен"],
};

async function loadPlugins() {
  const p = await api("GET", "plugins");
  state.plugins = p;
  // Do not redraw under an open configuration editor.
  if (!$("#plugins-list .plugin-config")) renderPlugins();
  renderDevicePlugins();
}

function renderPlugins() {
  const p = state.plugins;
  $("#plugins-note").textContent = p.enabled
    ? `Каталог ${p.dir}: в нём по папке на плагин с файлом plugin.json. Плагины работают отдельными процессами и общаются с сервером по JSON-RPC (SDK для Go, C++ и Python).`
    : "Плагины выключены. Чтобы включить, задайте каталог MARUSYA_PLUGINS_DIR (флаг -plugins_dir) и перезапустите сервер.";
  $("#plugins-rescan").hidden = !p.enabled;
  const errs = $("#plugins-errors");
  errs.replaceChildren(...p.errors.map((e) => el("li", { text: e })));
  errs.hidden = !p.errors.length;
  const list = $("#plugins-list");
  list.replaceChildren();
  if (p.enabled && !p.plugins.length) list.append(el("li", { class: "muted", text: "Серверных плагинов пока нет." }));
  for (const v of p.plugins) {
    const [cls, label] = pluginStates[v.state] || ["off", v.state];
    const sides = v.sides.map((s) => (s === "server" ? "сервер" : "колонка")).join(" + ");
    const meta = [v.id, v.version && "v" + v.version, sides, v.restarts ? `перезапусков: ${v.restarts}` : ""].filter(Boolean).join(" · ");
    const actions = el("div", { class: "row" },
      el("span", { class: "badge " + cls, text: label }),
      el("button", { class: "btn text small", type: "button", text: v.enabled ? "Выключить" : "Включить",
        onclick: (e) => pluginAction(e.target, `plugins/${encodeURIComponent(v.id)}/enabled`, { enabled: !v.enabled }, v.enabled ? "Плагин выключен" : "Плагин включён") }),
      v.enabled ? el("button", { class: "btn text small", type: "button", text: "Перезапустить",
        onclick: (e) => pluginAction(e.target, `plugins/${encodeURIComponent(v.id)}/restart`, {}, "Плагин перезапущен") }) : null,
      el("button", { class: "btn text small", type: "button", text: "Настройки", onclick: (e) => editPluginConfig(e.target.closest("li"), v) }));
    list.append(el("li", { class: "prov plugin" },
      el("div", { class: "prov-main" },
        el("b", { text: v.name || v.id }),
        el("small", { text: meta }),
        v.description ? el("small", { text: v.description }) : null,
        v.error ? el("small", { class: "error", text: v.error }) : null,
        v.tools.length ? el("div", { class: "tools" }, ...v.tools.map((t) => el("code", { text: t }))) : null),
      actions));
  }
}

function renderDevicePlugins() {
  const list = $("#plugins-devices");
  list.replaceChildren();
  const devs = state.plugins?.devices || [];
  if (!devs.length) list.append(el("li", { class: "muted", text: "Нет подключённых колонок." }));
  for (const d of devs) {
    let body;
    if (!d.plugins) body = el("small", { text: "агент без плагинов: каталог не задан или агент устарел" });
    else if (!d.tools.length) body = el("small", { text: "плагины не объявили функций" });
    else body = el("div", { class: "tools" }, ...d.tools.map((t) => el("code", { text: `${t.plugin}: ${t.name}`, title: t.description })));
    list.append(el("li", { class: "prov plugin" }, el("div", { class: "prov-main" }, el("b", { text: d.device }), body)));
  }
}

async function pluginAction(btn, path, body, okMsg) {
  const r = await run(btn, () => api("POST", path, body), okMsg);
  if (r) { state.plugins = r; renderPlugins(); renderDevicePlugins(); }
}

function editPluginConfig(li, v) {
  if ($(".plugin-config", li)) return;
  const area = el("textarea", { spellcheck: "false", rows: 6 });
  area.value = v.config ? JSON.stringify(v.config, null, 2) : "{}";
  const err = el("p", { class: "error", hidden: true });
  const close = () => { box.remove(); renderPlugins(); };
  const save = async (btn, value) => {
    err.hidden = true;
    let cfg = null;
    if (value !== null) {
      try { cfg = JSON.parse(value); } catch (ex) { err.textContent = "Неверный JSON: " + ex.message; err.hidden = false; return; }
    }
    btn.disabled = true;
    try {
      state.plugins = await api("PUT", `plugins/${encodeURIComponent(v.id)}/config`, cfg);
      toast("Настройки сохранены, плагин перезапущен");
      close();
      renderDevicePlugins();
    } catch (ex) {
      err.textContent = ex.message;
      err.hidden = false;
    } finally {
      btn.disabled = false;
    }
  };
  const box = el("div", { class: "plugin-config" },
    el("small", { class: "muted", text: "Настройки передаются плагину при запуске (поле config). Сохраняются на сервере." }),
    area, err,
    el("div", { class: "row" },
      el("button", { class: "btn primary small", type: "button", text: "Сохранить", onclick: (e) => save(e.target, area.value) }),
      el("button", { class: "btn text small", type: "button", text: "Как в plugin.json", onclick: (e) => save(e.target, null) }),
      el("button", { class: "btn text small", type: "button", text: "Отмена", onclick: close })));
  li.append(box);
  area.focus();
}

$("#plugins-rescan").addEventListener("click", (e) => pluginAction(e.target, "plugins/rescan", {}, "Каталог плагинов перечитан"));

// Refresh relative times.
setInterval(() => { if (!$("#app").hidden) renderDetail(); }, 30000);

boot().catch((ex) => { if (ex.status !== 401) toast(ex.message, true); });
