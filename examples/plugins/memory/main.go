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

// Command memory is a server plugin that gives Marusya a long-term memory
// ("Маруся, запомни, что меня зовут Дима"). The model saves and forgets
// facts with functions; the facts are added to the system instruction of
// every conversation and can be edited on the plugin page in the panel.
//
// The system instruction of a plugin is the same for every speaker, so the
// memory is shared by all speakers of the server.
//
//	CGO_ENABLED=0 go build -o examples/plugins/memory/memory ./examples/plugins/memory
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"html/template"
	"log"
	"net/http"
	"path/filepath"
	"strings"

	"github.com/yawaflua/marusya-gemini/pkg/pluginsdk"
)

type config struct {
	// File is the store, relative to the plugin directory. The default
	// is outside it, so updating the plugin keeps the memory.
	File          string `json:"file"`
	MaxFacts      int    `json:"max_facts"`
	MaxFactLength int    `json:"max_fact_length"`
}

func main() {
	p := pluginsdk.New()
	var store *Store
	p.OnInit(func(ctx context.Context, in pluginsdk.Init) error {
		cfg := config{File: "../.memory.json", MaxFacts: 100, MaxFactLength: 300}
		if err := p.Config(&cfg); err != nil {
			return err
		}
		path := cfg.File
		if !filepath.IsAbs(path) {
			path = filepath.Join(in.PluginDir, path)
		}
		s, err := Open(path, max(cfg.MaxFacts, 1), max(cfg.MaxFactLength, 20))
		if err != nil {
			return err
		}
		store = s
		// Before the reply to initialize this only sets the initial value.
		return p.Instructions(ctx, Instructions(s.Facts()))
	})
	register(p, func() *Store { return store })
	if err := p.Run(); err != nil {
		log.Fatal(err)
	}
}

// register adds the functions and the page of the plugin; store is set by
// the time they are called.
func register(p *pluginsdk.Plugin, store func() *Store) {
	update := func(ctx context.Context) {
		if err := p.Instructions(ctx, Instructions(store().Facts())); err != nil {
			p.Log("warn", "instructions: %v", err)
		}
	}

	p.Tool(pluginsdk.ToolSpec{
		Name: "memory_save",
		Description: "Saves a fact to the long-term memory, kept across conversations. Use it when the user asks " +
			"to remember something, or tells something worth knowing later: names, birthdays, preferences, " +
			"family, pets, habits. One short self-contained fact per call, in the user's language, in the " +
			"third person, e.g. \"Пользователя зовут Дима\". Do not save passwords, card numbers or one-off requests.",
		Params: []pluginsdk.Param{
			{Name: "fact", Type: pluginsdk.TypeString, Required: true, Description: "The fact, one sentence."},
		},
	}, func(ctx context.Context, c pluginsdk.Call) (any, error) {
		f, err := store().Add(c.String("fact"), c.Device.ID)
		if err != nil {
			return nil, err
		}
		update(ctx)
		return map[string]any{"saved": f.Text, "id": f.ID}, nil
	})

	p.Tool(pluginsdk.ToolSpec{
		Name: "memory_forget",
		Description: "Removes a fact from the long-term memory. Use it when the user asks to forget something or " +
			"a remembered fact is no longer true (then save the new one). Pass the number of the fact from the " +
			"memory list, or words it contains.",
		Params: []pluginsdk.Param{
			{Name: "id", Type: pluginsdk.TypeInteger, Description: "Number of the fact."},
			{Name: "text", Type: pluginsdk.TypeString, Description: "Words of the fact, if the number is unknown."},
		},
	}, func(ctx context.Context, c pluginsdk.Call) (any, error) {
		s := store()
		id := c.Int("id", 0)
		if id == 0 {
			found := s.Find(c.String("text"))
			switch len(found) {
			case 0:
				return nil, fmt.Errorf("no fact matches %q", c.String("text"))
			case 1:
				id = found[0].ID
			default:
				return map[string]any{"error": "several facts match, pass the id", "matches": found}, nil
			}
		}
		f, err := s.Remove(id)
		if err != nil {
			return nil, err
		}
		update(ctx)
		return map[string]any{"forgotten": f.Text}, nil
	})

	p.HTTP(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s := store()
		if r.Method == http.MethodPost {
			var req struct {
				Action string `json:"action"`
				Text   string `json:"text"`
				ID     int    `json:"id"`
			}
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			var err error
			switch req.Action {
			case "add":
				_, err = s.Add(req.Text, "")
			case "delete":
				_, err = s.Remove(req.ID)
			case "clear":
				err = s.Clear()
			default:
				err = fmt.Errorf("unknown action %q", req.Action)
			}
			if err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			update(r.Context())
			w.WriteHeader(http.StatusNoContent)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		page.Execute(w, s.Facts())
	}), false)
}

var page = template.Must(template.New("page").Funcs(template.FuncMap{
	"date": func(f Fact) string { return f.Added.Local().Format("02.01.2006") },
	"none": func(s string) bool { return strings.TrimSpace(s) == "" },
}).Parse(`<!doctype html><meta charset="utf-8"><meta name="viewport" content="width=device-width">
<title>Память</title>
<style>
body{font:16px system-ui;margin:2em auto;max-width:44em;padding:0 1em}
table{border-collapse:collapse;width:100%}td{padding:.4em;border-bottom:1px solid #8884;vertical-align:top}
.meta{color:#888;font-size:.85em;white-space:nowrap}button{font:inherit}form{display:flex;gap:.5em;margin:1em 0}
input{flex:1;font:inherit;padding:.3em}#err{color:#c33}
</style>
<h1>Память Маруси</h1>
<p>Это знает Маруся на всех колонках. Скажите «Маруся, запомни…» или «забудь…», либо правьте здесь.</p>
<form onsubmit="send({action:'add',text:this.text.value});return false">
<input name="text" placeholder="Новый факт, например: Кота зовут Барсик" required><button>Добавить</button>
</form>
<p id="err"></p>
{{if .}}<table>{{range .}}<tr><td class="meta">#{{.ID}}</td><td>{{.Text}}</td>
<td class="meta">{{date .}}{{if not (none .Device)}}, {{.Device}}{{end}}</td>
<td><button onclick="send({action:'delete',id:{{.ID}}})">Забыть</button></td></tr>{{end}}</table>
<p><button onclick="if(this.dataset.sure)send({action:'clear'});else{this.dataset.sure=1;this.textContent='Точно стереть всё?'}">Стереть всё</button></p>
{{else}}<p>Пока ничего не запомнено.</p>{{end}}
<script>
async function send(body) {
  const r = await fetch(location.pathname, {method: "POST", headers: {"X-Marusya-Panel": "1"}, body: JSON.stringify(body)});
  if (r.ok) location.reload(); else document.getElementById("err").textContent = await r.text();
}
</script>`))
