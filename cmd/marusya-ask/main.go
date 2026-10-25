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

// Command marusya-ask speaks through a Marusya speaker and returns the
// spoken answer (POST /v1/ask on marusya-server).
//
//	marusya-ask "Сборка готова"                  # announce
//	marusya-ask -listen "Деплоить в прод?"       # ask, print the answer
//	marusya-ask hook                             # Claude Code hook (Stop, Notification)
//
// As a Claude Code hook it reads the hook input from stdin. On Stop it reads
// out Claude's question (or that the task is done) and, if you answer, makes
// Claude continue with your answer; on Notification it reads out the
// notification, e.g. that a permission is needed. Settings come from flags
// or the environment: MARUSYA_URL (or MARUSYA_SERVER, the ws:// URL of the
// speakers), MARUSYA_AUTH_TOKEN and MARUSYA_DEVICE.
package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"
)

var (
	serverURL = flag.String("server", "", "marusya-server URL, e.g. http://192.168.31.80:8080 (default $MARUSYA_URL, or derived from $MARUSYA_SERVER).")
	token     = flag.String("token", "", "Bearer token (default $MARUSYA_AUTH_TOKEN).")
	device    = flag.String("device", "", "Speaker device ID; empty picks the first one (default $MARUSYA_DEVICE).")
	listen    = flag.Bool("listen", false, "Wait for a spoken answer and print it.")
	timeout   = flag.Duration("timeout", 5*time.Minute, "How long to wait for the speaker to be free and for the answer.")
	source    = flag.String("source", "", `Caller name in the web panel history (default "claude" for hooks, "api" otherwise).`)
	doneToo   = flag.Bool("on_done", true, "Hook: also speak up when Claude finished without a question, so you can give the next task.")
)

func main() {
	flag.Usage = func() {
		fmt.Fprintf(os.Stderr, "usage: marusya-ask [flags] TEXT | marusya-ask [flags] hook\n\n")
		flag.PrintDefaults()
	}
	flag.Parse()
	if flag.NArg() == 0 {
		flag.Usage()
		os.Exit(2)
	}
	var err error
	if flag.NArg() == 1 && flag.Arg(0) == "hook" {
		err = runHook(os.Stdin, os.Stdout)
		if err != nil {
			journal("  error: %v", err)
		}
	} else {
		var answer string
		var ok bool
		answer, ok, err = ask(strings.Join(flag.Args(), " "), *listen, cmp(*source, "api"))
		if err == nil && *listen {
			if !ok {
				fmt.Fprintln(os.Stderr, "no answer")
				os.Exit(3)
			}
			fmt.Println(answer)
		}
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "marusya-ask:", err)
		os.Exit(1)
	}
}

func cmp(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

// baseURL returns the HTTP URL of the server.
func baseURL() (string, error) {
	if u := cmp(*serverURL, os.Getenv("MARUSYA_URL")); u != "" {
		return strings.TrimSuffix(u, "/"), nil
	}
	if ws := os.Getenv("MARUSYA_SERVER"); ws != "" {
		u := strings.Replace(ws, "ws", "http", 1)
		if i := strings.Index(u, "/v1/"); i >= 0 {
			u = u[:i]
		}
		return u, nil
	}
	return "", errors.New("set -server or $MARUSYA_URL, e.g. http://192.168.31.80:8080")
}

type askRequest struct {
	Text    string `json:"text"`
	Listen  bool   `json:"listen,omitempty"`
	Device  string `json:"device,omitempty"`
	Timeout int    `json:"timeout,omitempty"`
	Source  string `json:"source,omitempty"`
}

type askResponse struct {
	Device   string `json:"device"`
	Answered bool   `json:"answered"`
	Answer   string `json:"answer"`
	Error    string `json:"error"`
}

// ask calls POST /v1/ask.
func ask(text string, listen bool, src string) (string, bool, error) {
	base, err := baseURL()
	if err != nil {
		return "", false, err
	}
	body, _ := json.Marshal(askRequest{
		Text: text, Listen: listen, Source: src,
		Device:  cmp(*device, os.Getenv("MARUSYA_DEVICE")),
		Timeout: int(timeout.Seconds()),
	})
	req, err := http.NewRequest("POST", base+"/v1/ask", bytes.NewReader(body))
	if err != nil {
		return "", false, err
	}
	req.Header.Set("Content-Type", "application/json")
	if t := cmp(*token, os.Getenv("MARUSYA_AUTH_TOKEN")); t != "" {
		req.Header.Set("Authorization", "Bearer "+t)
	}
	c := &http.Client{Timeout: *timeout + 30*time.Second}
	resp, err := c.Do(req)
	if err != nil {
		return "", false, err
	}
	defer resp.Body.Close()
	var ar askResponse
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&ar); err != nil {
		return "", false, fmt.Errorf("%s: %w", resp.Status, err)
	}
	if resp.StatusCode != http.StatusOK {
		return "", false, fmt.Errorf("%s: %s", resp.Status, ar.Error)
	}
	return ar.Answer, ar.Answered, nil
}

// hookInput is the part of the Claude Code hook input used here.
type hookInput struct {
	Event                string `json:"hook_event_name"`
	Cwd                  string `json:"cwd"`
	TranscriptPath       string `json:"transcript_path"`
	LastAssistantMessage string `json:"last_assistant_message"`
	Message              string `json:"message"`
	NotificationType     string `json:"notification_type"`
	ToolName             string `json:"tool_name"`
	ToolInput            struct {
		Questions []struct {
			Question string `json:"question"`
			Options  []struct {
				Label string `json:"label"`
			} `json:"options"`
		} `json:"questions"`
	} `json:"tool_input"`
}

func runHook(in io.Reader, out io.Writer) error {
	var h hookInput
	if err := json.NewDecoder(in).Decode(&h); err != nil {
		return fmt.Errorf("hook input: %w", err)
	}
	journal("%s tool=%s questions=%d", h.Event, h.ToolName, len(h.ToolInput.Questions))
	src := cmp(*source, "claude")
	project := ""
	if h.Cwd != "" {
		project = " в проекте " + filepath.Base(h.Cwd)
	}
	switch h.Event {
	case "Stop", "SubagentStop":
		msg := h.LastAssistantMessage
		if msg == "" {
			msg = lastAssistantText(h.TranscriptPath)
		}
		text, question := summarize(msg)
		switch {
		case question:
			text = "Клод" + project + " спрашивает: " + text
		case !*doneToo:
			return nil
		case text != "":
			text = "Клод" + project + " закончил: " + text
		default:
			text = "Клод" + project + " закончил."
		}
		answer, ok, err := ask(text, true, src)
		journal("  stop: answered=%v answer=%q err=%v", ok, answer, err)
		if err != nil || !ok || isDone(answer) {
			return err
		}
		// Block the stop: Claude continues with the answer as the reason.
		return json.NewEncoder(out).Encode(map[string]string{
			"decision": "block",
			"reason":   "Пользователь ответил голосом через колонку Маруся (это распознанная речь, возможны ошибки распознавания): «" + answer + "»",
		})
	case "PreToolUse":
		if h.ToolName != "AskUserQuestion" {
			return nil
		}
		return askQuestions(h, project, src, out)
	case "Notification":
		if strings.Contains(h.Message, "AskUserQuestion") {
			return nil // read out by the PreToolUse hook
		}
		_, _, err := ask("Клод"+project+": "+notification(h.Message), false, src)
		return err
	}
	return nil
}

// lastAssistantText reads the text of the last assistant message from a
// Claude Code transcript (JSON lines), for versions without
// last_assistant_message.
func lastAssistantText(path string) string {
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer f.Close()
	var last string
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1<<20), 16<<20)
	for sc.Scan() {
		var line struct {
			Type    string `json:"type"`
			Message struct {
				Content json.RawMessage `json:"content"`
			} `json:"message"`
		}
		if json.Unmarshal(sc.Bytes(), &line) != nil || line.Type != "assistant" {
			continue
		}
		var parts []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		}
		if json.Unmarshal(line.Message.Content, &parts) != nil {
			continue
		}
		var b strings.Builder
		for _, p := range parts {
			if p.Type == "text" {
				b.WriteString(p.Text)
			}
		}
		if b.Len() > 0 {
			last = b.String()
		}
	}
	return last
}

var (
	codeBlock = regexp.MustCompile("(?s)```.*?```")
	mdLink    = regexp.MustCompile(`\[([^\]]+)\]\([^)]*\)`)
	url       = regexp.MustCompile(`https?://\S+`)
	mdMarks   = regexp.MustCompile("(?m)^\\s*(#+|>|[-*+]|\\d+\\.)\\s+|[*_`~|]")
	spaces    = regexp.MustCompile(`\s+`)
	sentence  = regexp.MustCompile(`[^.!?…]+[.!?…]*`)
)

const (
	maxQuestion = 300
	maxDone     = 200
)

// summarize turns Claude's reply into something short enough to be read
// out: its last questions if it asks any, else its first sentence.
func summarize(msg string) (text string, question bool) {
	msg = codeBlock.ReplaceAllString(msg, " ")
	msg = mdLink.ReplaceAllString(msg, "$1")
	msg = url.ReplaceAllString(msg, "ссылка")
	msg = mdMarks.ReplaceAllString(msg, " ")
	msg = strings.TrimSpace(spaces.ReplaceAllString(msg, " "))
	var all, questions []string
	for _, s := range sentence.FindAllString(msg, -1) {
		s = strings.TrimSpace(s)
		if s == "" {
			continue
		}
		all = append(all, s)
		if strings.HasSuffix(s, "?") {
			questions = append(questions, s)
		}
	}
	if len(questions) > 0 {
		// The last questions that fit, in order.
		var picked []string
		n := 0
		for i := len(questions) - 1; i >= 0; i-- {
			l := utf8.RuneCountInString(questions[i])
			if len(picked) > 0 && n+l > maxQuestion {
				break
			}
			picked = append([]string{questions[i]}, picked...)
			n += l
		}
		return truncate(strings.Join(picked, " "), maxQuestion), true
	}
	if len(all) == 0 {
		return "", false
	}
	return truncate(all[0], maxDone), false
}

func truncate(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	r := []rune(s)
	return strings.TrimSpace(string(r[:n])) + "…"
}

var permissionRE = regexp.MustCompile(`(?i)^Claude needs your permission to use (.+)$`)

// notification translates the usual Claude Code notifications.
func notification(msg string) string {
	msg = strings.TrimSpace(msg)
	if m := permissionRE.FindStringSubmatch(msg); m != nil {
		return "нужно разрешение на " + m[1] + ". Подтвердите на компьютере."
	}
	if strings.EqualFold(msg, "Claude is waiting for your input") {
		return "ждёт вашего ответа."
	}
	if msg == "" {
		return "требует внимания."
	}
	return msg
}

// askQuestions reads out the questions of an AskUserQuestion call one by
// one. If every one is answered, the call is denied with the answers as the
// reason, which Claude reads as the user's reply; otherwise the dialog is
// shown as usual.
func askQuestions(h hookInput, project, src string, out io.Writer) error {
	qs := h.ToolInput.Questions
	if len(qs) == 0 {
		return nil
	}
	var answers []string
	for i, q := range qs {
		text := "Клод" + project + " спрашивает: "
		if len(qs) > 1 {
			text = fmt.Sprintf("Вопрос %d из %d: ", i+1, len(qs))
		}
		text += strings.TrimSpace(q.Question)
		var labels []string
		for _, o := range q.Options {
			if l := strings.TrimSpace(o.Label); l != "" {
				labels = append(labels, l)
			}
		}
		if len(labels) > 0 {
			text += " Варианты: " + joinOr(labels) + "."
		}
		answer, ok, err := ask(truncate(text, 900), true, src)
		journal("  question %d: answered=%v answer=%q err=%v", i+1, ok, answer, err)
		if err != nil || !ok {
			return err // the dialog stays for the keyboard
		}
		answers = append(answers, fmt.Sprintf("«%s» — «%s»", strings.TrimSpace(q.Question), answer))
	}
	return json.NewEncoder(out).Encode(map[string]any{
		"hookSpecificOutput": map[string]string{
			"hookEventName":      "PreToolUse",
			"permissionDecision": "deny",
			"permissionDecisionReason": "Пользователь уже ответил голосом через колонку Маруся (распознанная речь, возможны ошибки): " +
				strings.Join(answers, "; ") + ". Не задавай эти вопросы снова, продолжай с этими ответами.",
		},
	})
}

// joinOr joins labels as "a, b или c".
func joinOr(labels []string) string {
	if len(labels) == 1 {
		return labels[0]
	}
	return strings.Join(labels[:len(labels)-1], ", ") + " или " + labels[len(labels)-1]
}

var doneRE = regexp.MustCompile(`^(всё|все|хватит|ничего|нет|стоп|отбой|спасибо|пока|достаточно|не надо|ничего не надо|всё спасибо|все спасибо|нет спасибо)$`)

// isDone reports whether an answer to "Claude finished" means that there is
// nothing more to do, so Claude may stop instead of being asked again.
func isDone(answer string) bool {
	a := strings.ToLower(strings.Trim(strings.TrimSpace(answer), ".!,…"))
	return doneRE.MatchString(strings.Join(strings.Fields(strings.ReplaceAll(a, ",", " ")), " "))
}

// journal appends a line to ~/.cache/marusya-ask.log (or $MARUSYA_ASK_LOG;
// "-" disables it), to check which hooks Claude Code actually calls.
func journal(format string, args ...any) {
	path := os.Getenv("MARUSYA_ASK_LOG")
	if path == "-" {
		return
	}
	if path == "" {
		dir, err := os.UserCacheDir()
		if err != nil {
			return
		}
		path = filepath.Join(dir, "marusya-ask.log")
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return
	}
	defer f.Close()
	fmt.Fprintf(f, "%s %s\n", time.Now().Format(time.DateTime), fmt.Sprintf(format, args...))
}
