# Плагины Марусии

Плагин — это программа в своей папке рядом с файлом `plugin.json`. Сервер (`MARUSYA_PLUGINS_DIR`
в `server.env`) или агент на колонке (`MARUSYA_PLUGINS_DIR` в `/etc/conf.d/marusya-agent`)
запускает её с рабочим каталогом в этой папке. Затем он общается с программой по **JSON-RPC 2.0**:
одно JSON-сообщение на строку в stdin и stdout. Всё, что программа пишет в stderr, попадает в лог.
В stdout нельзя писать ничего, кроме сообщений протокола.

SDK:

| Язык | Где | Подключение |
|---|---|---|
| Go | [pkg/pluginsdk](../pkg/pluginsdk) | `import "github.com/yawaflua/marusya-gemini/pkg/pluginsdk"` |
| C++17 | [cpp/marusya_plugin.hpp](cpp/marusya_plugin.hpp) | один заголовок, без зависимостей, `-pthread` |
| Python 3.8+ | [python/marusya_plugin.py](python/marusya_plugin.py) | положить файл рядом с плагином |

Go и Python SDK умеют всё, что описано ниже. C++ SDK умеет то, что работает на обеих сторонах
(функции, события, `host.command`, `plugin.send`, `log`): серверных возможностей из раздела
[«Только на сервере»](#только-на-сервере-модель-речь-http-инструкции) в нём пока нет.

Примеры: [examples/plugins](../examples/plugins); самый простой — `coin` («подбрось монетку»).
Провайдер модели — `ollama`, синтез речи — `piper`, HTTP и системная инструкция — `mood` и
`ytmusic`, две половины на Go — `airplay`.

## plugin.json

```json
{
  "id": "weather-station",
  "name": "Метеостанция",
  "version": "1.0.0",
  "description": "Показывает данные домашней метеостанции",
  "server": {"command": ["./weather-station", "-v"], "env": {"STATION": "192.168.1.9"}},
  "agent":  {"command": ["./station-agent"]},
  "config": {"units": "metric"}
}
```

- `id`: строчные латинские буквы, цифры, `.`, `_`, `-`, не длиннее 64 символов.
- `server` и `agent` задают, где работает плагин; нужен хотя бы один из них. Программа ищется
  сначала в папке плагина, затем в `PATH` (например, `python3`).
- `config` — настройки по умолчанию. Их можно поменять в веб-панели, и плагин получит их в
  `initialize`.
- Переменные окружения процесса: `MARUSYA_PLUGIN_ID`, `MARUSYA_PLUGIN_DIR`,
  `MARUSYA_PLUGIN_HOST` (`server` или `agent`).

## Протокол (версия 1)

Обе стороны могут отправлять запросы (`id` есть, ждут ответа) и уведомления (`id` нет).

### Хост → плагин

**`initialize`** — первый запрос; на него нужно ответить за 10 секунд.

```json
{"jsonrpc":"2.0","id":1,"method":"initialize","params":{
  "protocol":1,"host":"server","host_version":"1.4.0","plugin_dir":"/plugins/dice",
  "config":{"max_dice":10},
  "device":{"id":"kitchen","language":"ru-RU"}}}
```

`device` передаётся только на колонке. Ответ:

```json
{"jsonrpc":"2.0","id":1,"result":{
  "protocol":1,
  "tools":[{"name":"roll_dice","description":"Rolls dice and returns the values.",
            "params":[{"name":"count","type":"integer","description":"How many dice","required":false,
                       "minimum":1,"maximum":10}]}],
  "events":["turn"]}}
```

- Имя функции: `^[a-z][a-z0-9_]{0,47}$`. Оно не должно совпадать со встроенными функциями
  (`volume_set`, `media_play_search`, ...), иначе функция отклоняется. Описание пишется
  по-английски и говорит модели, когда вызывать функцию.
- Типы параметров: `string`, `integer`, `number`, `boolean`; также доступны `enum`, `minimum`,
  `maximum`.
- `events` — какие события присылать; `"*"` означает все.

**`tool.call`** — модель вызвала функцию. Результатом должен быть небольшой JSON-объект, который
увидит модель. Если вернуть ошибку, модель её тоже увидит и скажет о ней.

```json
{"jsonrpc":"2.0","id":7,"method":"tool.call","params":{"name":"roll_dice","args":{"count":2},
  "device":{"id":"kitchen","language":"ru-RU","location":"Ashdod","timezone":"Asia/Jerusalem"}}}
```

**`event`** (уведомление) — `{"kind":"turn","device":"kitchen","data":{...}}`.
События сервера:
- `connect`, `disconnect` — колонка подключилась или отключилась;
- `turn` — завершена реплика; `data.user` и `data.model` содержат тексты;
- `command`, `skill` — выполнено действие колонки или серверная функция;
- `wake` — сработало ключевое слово.

На колонке есть одно событие, `state`: `data.state` принимает значения `idle`, `listening`,
`thinking`, `speaking`, `error`, `disconnected`, `announcing`.

**`message`** (уведомление) — данные от другой половины плагина: `{"device":"kitchen","data":...}`.

**`shutdown`** — нужно ответить и завершиться. После ответа хост закрывает stdin. Если процесс
не завершился за 3 секунды, хост посылает SIGTERM, а ещё через 3 секунды — SIGKILL.

### Плагин → хост

| Метод | Параметры | Ответ | Где |
|---|---|---|---|
| `log` (уведомление) | `{"level":"info","message":"..."}` | — | везде |
| `host.command` | `{"device":"kitchen","action":"volume.set","args":{"level":30}}` | данные действия | везде |
| `host.say` | `{"device":"kitchen","text":"Готово","listen":false}` | `{"answer":"..."}` | сервер |
| `host.devices` | `{}` | `{"devices":[{"id":"kitchen",...}]}` | сервер |
| `plugin.send` | `{"device":"kitchen","data":{...}}` | — | везде |
| `host.request` | `{"device":"kitchen","text":"Какая погода?"}` | — | сервер |
| `host.instructions` | `{"text":"Отвечай как пират."}` | — | сервер |
| `provider.output` (уведомление) | `{"session":"...","text":"часть ответа"}` | — | сервер |

- `host.command` выполняет действие колонки, перечисленное в её `hello.capabilities`
  (`internal/models/command.go`): `volume.set`, `media.control`, `media.play_search`,
  `ring.set` и другие, а также функции плагинов колонки. Если `device` пустой, действие
  выполняется на единственной подключённой колонке; на самой колонке поле `device` ни на что
  не влияет.
- `host.say` требует распознавания и синтеза речи на сервере (как `/v1/ask`). С `listen`
  колонка ждёт ответа и возвращает его текстом.
- `plugin.send` на сервере с пустым `device` рассылает данные всем колонкам с этим плагином.
- `host.request` передаёт текст Марусе на колонке так, будто его сказали вслух: модель
  отвечает, может вызывать функции, и ответ звучит на колонке. Возвращается сразу, не дожидаясь
  ответа. До 1000 символов. Если колонка сейчас разговаривает или провайдер не принимает
  текст, запрос вернёт ошибку.
- `host.instructions` заменяет дополнение плагина к системной инструкции (см. ниже).

Плагин может вызывать хост прямо во время обработки `tool.call`: все SDK обрабатывают запросы
параллельно.

## Только на сервере: модель, речь, HTTP, инструкции

Серверный плагин может добавить в ответ на `initialize` необязательные поля. Агент на колонке
их пропускает и пишет предупреждение в лог. Имена провайдеров и движков подчиняются правилу
`^[a-z0-9][a-z0-9_-]{0,31}$`.

```json
{"protocol":1,"tools":[],
 "instructions":"Твоё настроение: весёлое.",
 "providers":[{"name":"chat","label":"Ollama","description":"Локальная модель"}],
 "speech":[{"name":"piper","label":"Piper","synthesize":true,"transcribe":false,
            "sample_rate":22050,"voices":["irina","dmitri"]}],
 "http":{"public":false}}
```

### Дополнение к системной инструкции

`instructions` — текст, который сервер добавляет к системной инструкции модели на всех колонках.
Так задают характер, манеру речи или знания о доме. Поменять его на ходу можно запросом
`host.instructions`: беседы обновятся к следующей реплике. Пустая строка убирает дополнение.

### Провайдер модели

Провайдер `chat` плагина `ollama` выбирается во вкладке «Провайдеры»: тип «Плагин сервера»,
модель `ollama/chat`. Сервер распознаёт речь сам (или движком из поля «Речь»), а затем
вызывает плагин:

```json
{"jsonrpc":"2.0","id":5,"method":"provider.turn","params":{
  "provider":"chat","session":"kitchen-3f2a","text":"какая громкость?",
  "system":"Ты Маруся, голосовой ассистент...","language":"ru-RU",
  "tools":[{"name":"volume_get","description":"...","params":[]}]}}
```

- `session` — беседа. Историю плагин хранит сам; когда беседа заканчивается, приходит
  уведомление `provider.close` `{"provider":"chat","session":"kitchen-3f2a"}`.
- Ответ: `{"text":"...","tool_calls":[{"id":"c1","name":"volume_get","args":{}}]}`. Если есть
  `tool_calls`, сервер выполняет функции и снова вызывает `provider.turn` с пустым `text` и
  `"tool_results":[{"id":"c1","name":"volume_get","output":{"level":42}}]` (или `"error"`).
  Так повторяется до 8 раз за реплику.
- Чтобы колонка заговорила раньше, ответ можно отправлять кусками уведомлениями
  `provider.output` `{"session":"...","text":"..."}` до ответа на запрос. Сервер озвучивает
  их по предложениям, затем `text` из ответа. Уведомления обрабатываются по порядку.

### Синтез и распознавание речи

Движок `piper` плагина `piper` выбирается в поле «Речь» провайдера как
`plugin:piper/piper`. Звук передаётся в base64 как 16-битный little-endian моно PCM.

```json
{"method":"speech.synthesize","params":{"engine":"piper","text":"Привет!","language":"ru-RU","voice":"irina"}}
→ {"audio":"<base64 PCM с частотой sample_rate>"}
{"method":"speech.transcribe","params":{"engine":"whisper","audio":"<base64 PCM>","sample_rate":16000,"language":"ru-RU"}}
→ {"text":"привет"}
```

`sample_rate` синтеза по умолчанию 24000. Если движок умеет только одно из двух или плагин не
запущен, второе делает Gemini сервера.

### HTTP

С полем `http` запросы к `/plugins/<id>/<путь>` на сервере приходят плагину:

```json
{"method":"http.request","params":{"method":"POST","path":"/hook","query":"a=1",
  "header":{"Content-Type":["application/json"]},"body":"<base64>","remote_addr":"10.0.0.5:51234"}}
→ {"status":200,"header":{"Content-Type":["text/html; charset=utf-8"]},"body":"<base64>"}
```

- Без `public` нужен вход в веб-панель (для методов кроме GET и HEAD ещё заголовок
  `X-Marusya-Panel: 1`) или `Authorization: Bearer <MARUSYA_AUTH_TOKEN>`. С `public` адрес
  открыт всем, и проверять доступ плагин должен сам.
- Cookie и (без `public`) `Authorization` плагину не передаются. Тело запроса — до 4 МБ,
  ответить нужно за минуту.
- Если `Content-Type` не задан, сервер определяет его сам и всегда добавляет
  `X-Content-Type-Options: nosniff`.

### В SDK

| | Go | Python |
|---|---|---|
| инструкция | `p.Instructions(ctx, text)` | `plugin.instructions = ...`, `plugin.set_instructions(text)` |
| вопрос Марусе | `p.Request(ctx, device, text)` | `plugin.request(text, device)` |
| провайдер | `p.Provider(spec, ProviderHandler{Turn, Close})`, `turn.Write(text)` | `@plugin.provider(name, label, close=fn)`, `turn.write(text)` |
| речь | `p.Speech(spec, SpeechHandler{Synthesize, Transcribe})` | `@plugin.synthesizer(...)`, `@plugin.transcriber(...)`, `plugin.set_speech(...)` |
| HTTP | `p.HTTP(http.Handler, public)` | `@plugin.http(public=False)` |

В Go обработчик HTTP — обычный `http.Handler`. В Python функция получает `HTTPRequest` (`method`,
`path`, `query`, `headers`, `body`, `arg()`, `json()`) и возвращает строку (HTML), dict/list
(JSON), bytes или кортеж `(status, headers, body)`.

## Жизненный цикл

- Упавший процесс перезапускается с паузой от 1 секунды до 1 минуты. Если процесс проработал
  дольше минуты, пауза снова становится короткой.
- Модель видит набор функций, который был на момент открытия её сессии. Когда он меняется,
  сервер обновляет сессии колонок к следующей реплике, а агент переподключается к серверу.
- Во вкладке «Плагины» веб-панели плагин можно выключить, перезапустить или изменить его
  настройки. Это состояние хранится в `<каталог плагинов>/.state.json` с правами 0600.

## Сборка для колонки

Агент — статический бинарник для `linux/arm64`, Python на колонке обычно нет. Поэтому плагины
для колонки удобнее всего собирать статически:

```sh
# Go
CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -o my-plugin .
# C++
aarch64-linux-gnu-g++ -std=c++17 -O2 -pthread -static -I sdk/cpp plugin.cpp -o my-plugin
```

Плагины работают с правами того, кто их запустил: на колонке это root. Ставьте только те
плагины, которым доверяете.
