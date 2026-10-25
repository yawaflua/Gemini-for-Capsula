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

package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/yawaflua/marusya-gemini/internal/models"
)

// The ask API lets other programs talk through a speaker: Claude Code asks
// a question and waits for the spoken answer (cmd/marusya-ask), Home
// Assistant announces that the washing machine is done.
//
//	POST /v1/ask   Authorization: Bearer <MARUSYA_AUTH_TOKEN>
//	{"text": "Клод спрашивает: …", "listen": true, "device": "", "timeout": 120, "source": "claude"}
//	→ {"device": "vk-mini-a113x", "answered": true, "answer": "да, продолжай"}
//
// The message is synthesized, played once the speaker is free (with a chime
// and an orange ring) and, with listen, the next utterance is transcribed
// and returned instead of going to the model.

const (
	maxAskText       = 1000
	defaultAskWait   = 2 * time.Minute
	maxAskWait       = 10 * time.Minute
	maxAnswerAudio   = 60 * time.Second
	askChunk         = 100 * time.Millisecond // audio sent per frame
	askRetryInterval = time.Second
	// askDeclineWait is how long a delivered announcement without listen
	// may still be declined by a busy device.
	askDeclineWait = 1500 * time.Millisecond
)

var (
	errNoSpeech    = errors.New("на сервере нет синтеза речи: нужен ключ Gemini API (GEMINI_API_KEY)")
	errNoAnnounce  = errors.New("агент на колонке не умеет объявления: перезагрузите колонку, чтобы она скачала новый")
	errAskTimeout  = errors.New("колонка была занята всё это время")
	errNoAnswer    = errors.New("no answer")
	errAskReplaced = errors.New("колонка отключилась")
)

// pendingAsk is an announcement being delivered to a device.
type pendingAsk struct {
	id       string
	listen   bool
	declined chan struct{} // the device was busy
	answer   chan []byte   // the recorded answer; nil: nobody answered

	// Owned under deviceSession.mu.
	replyTurn string
	pcm       bytes.Buffer
}

// AskRequest is the body of POST /v1/ask.
type AskRequest struct {
	Text string `json:"text"`
	// Listen waits for a spoken answer.
	Listen bool `json:"listen,omitempty"`
	// Device is the device ID; empty picks the only (or first) speaker.
	Device string `json:"device,omitempty"`
	// Timeout bounds the whole request, seconds (default 120, max 600).
	Timeout int `json:"timeout,omitempty"`
	// Source names the caller in the history, e.g. "claude" or
	// "homeassistant".
	Source string `json:"source,omitempty"`
}

// AskResponse is the answer of POST /v1/ask.
type AskResponse struct {
	Device   string `json:"device"`
	Answered bool   `json:"answered"`
	Answer   string `json:"answer,omitempty"`
	Error    string `json:"error,omitempty"`
}

func (s *Server) handleAsk(w http.ResponseWriter, r *http.Request) {
	if !s.authorized(r) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	var req AskRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10)).Decode(&req); err != nil {
		writeAsk(w, http.StatusBadRequest, AskResponse{Error: "bad JSON: " + err.Error()})
		return
	}
	req.Text = strings.TrimSpace(req.Text)
	switch {
	case req.Text == "":
		writeAsk(w, http.StatusBadRequest, AskResponse{Error: "text is empty"})
		return
	case utf8.RuneCountInString(req.Text) > maxAskText:
		writeAsk(w, http.StatusBadRequest, AskResponse{Error: "text is too long"})
		return
	}
	wait := defaultAskWait
	if req.Timeout > 0 {
		wait = min(time.Duration(req.Timeout)*time.Second, maxAskWait)
	}
	d := s.askDevice(req.Device)
	if d == nil {
		writeAsk(w, http.StatusServiceUnavailable, AskResponse{Device: req.Device, Error: "speaker is not connected"})
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), wait)
	defer cancel()
	answer, err := d.announce(ctx, req.Text, req.Listen, cmpOr(req.Source, "api"))
	resp := AskResponse{Device: d.hello.DeviceID, Answered: err == nil && req.Listen, Answer: answer}
	switch {
	case err == nil, errors.Is(err, errNoAnswer):
		writeAsk(w, http.StatusOK, resp)
	case errors.Is(err, errNoAnnounce), errors.Is(err, errNoSpeech):
		resp.Error = err.Error()
		writeAsk(w, http.StatusNotImplemented, resp)
	case errors.Is(err, errAskTimeout):
		resp.Error = err.Error()
		writeAsk(w, http.StatusGatewayTimeout, resp)
	default:
		resp.Error = err.Error()
		writeAsk(w, http.StatusBadGateway, resp)
	}
}

func cmpOr(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

func writeAsk(w http.ResponseWriter, code int, r AskResponse) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(r)
}

// askDevice finds the speaker for an ask request: by device ID, else the
// first one that supports announcements.
func (s *Server) askDevice(id string) *deviceSession {
	ds := s.sessions()
	slices.SortFunc(ds, func(a, b *deviceSession) int { return a.connected.Compare(b.connected) })
	for _, d := range ds {
		if id != "" && d.hello.DeviceID == id {
			return d
		}
	}
	if id != "" {
		return nil
	}
	for _, d := range ds {
		if d.supportsFeature(models.FeatureAnnounce) {
			return d
		}
	}
	if len(ds) > 0 {
		return ds[0] // reports errNoAnnounce
	}
	return nil
}

func (d *deviceSession) supportsFeature(f models.Feature) bool {
	return slices.Contains(d.hello.Features, f)
}

// announce speaks text on the device once it is free and, with listen, returns
// the transcribed answer ([errNoAnswer] if nobody answered).
func (d *deviceSession) announce(ctx context.Context, text string, listen bool, source string) (string, error) {
	if !d.supportsFeature(models.FeatureAnnounce) {
		return "", errNoAnnounce
	}
	if d.cfg.Synthesizer == nil || (listen && d.cfg.Recognizer == nil) {
		return "", errNoSpeech
	}
	lang := d.language()
	if v, ok := d.cfg.Synthesizer.(interface{ SetVoice(string) }); ok {
		if voice := d.srv.Settings().Voice; voice != "" {
			v.SetVoice(voice) // the voice chosen in the web panel
		}
	}
	pcm, err := d.cfg.Synthesizer.Synthesize(ctx, text, lang)
	if err != nil {
		return "", err
	}

	pa := &pendingAsk{
		id:       "ask-" + newID(),
		listen:   listen,
		declined: make(chan struct{}, 1),
		answer:   make(chan []byte, 1),
	}
	defer func() {
		d.mu.Lock()
		if d.ask == pa {
			d.ask = nil
		}
		d.mu.Unlock()
		d.srv.events.devicesChanged()
	}()
	for {
		if err := d.claimAsk(ctx, pa); err != nil {
			return "", err
		}
		d.log.Info("announcement", "id", pa.id, "source", source, "listen", listen, "text", text)
		if err := d.deliver(pa, text, pcm); err != nil {
			return "", err
		}
		rec, retry, err := d.awaitAnswer(ctx, pa)
		if err != nil {
			return "", err
		}
		if retry {
			d.mu.Lock()
			d.ask = nil
			d.mu.Unlock()
			select {
			case <-time.After(askRetryInterval):
			case <-ctx.Done():
				return "", errAskTimeout
			}
			continue
		}
		if !listen {
			d.srv.events.publish(Event{Kind: EventAsk, Session: d.id, Device: d.hello.DeviceID, Source: source, Model: text})
			return "", nil
		}
		answer, err := d.transcribeAnswer(ctx, pa, rec, lang)
		ev := Event{Kind: EventAsk, Session: d.id, Device: d.hello.DeviceID, Source: source, Model: text, User: answer}
		if err != nil && !errors.Is(err, errNoAnswer) {
			ev.Error = err.Error()
		}
		d.srv.events.publish(ev)
		return answer, err
	}
}

// claimAsk waits until the device is free and makes pa its announcement.
func (d *deviceSession) claimAsk(ctx context.Context, pa *pendingAsk) error {
	t := time.NewTicker(300 * time.Millisecond)
	defer t.Stop()
	for {
		d.mu.Lock()
		if !d.inTurn && !d.awaiting && d.ask == nil {
			d.ask = pa
			pa.replyTurn = ""
			pa.pcm.Reset()
			d.mu.Unlock()
			d.srv.events.devicesChanged()
			return nil
		}
		d.mu.Unlock()
		select {
		case <-t.C:
		case <-ctx.Done():
			return errAskTimeout
		case <-d.ctx.Done():
			return errAskReplaced
		}
	}
}

// deliver sends the announcement and its audio.
func (d *deviceSession) deliver(pa *pendingAsk, text string, pcm []byte) error {
	err := d.send(&models.Message{Type: models.TypeAnnounce, TurnID: pa.id,
		Announce: &models.Announce{Text: text, Listen: pa.listen}})
	chunk := d.cfg.Synthesizer.Format().FrameSize(askChunk)
	for i := 0; err == nil && i < len(pcm); i += chunk {
		ctx, cancel := context.WithTimeout(d.ctx, writeTimeout)
		err = d.conn.SendAudio(ctx, pcm[i:min(i+chunk, len(pcm))])
		cancel()
	}
	if err == nil {
		err = d.send(&models.Message{Type: models.TypeTurnComplete, TurnID: pa.id})
	}
	return err
}

// awaitAnswer waits for the outcome of a delivered announcement: the
// recorded answer (nil if nobody answered), or retry if the device was busy.
func (d *deviceSession) awaitAnswer(ctx context.Context, pa *pendingAsk) (rec []byte, retry bool, err error) {
	var timeout <-chan time.Time
	if !pa.listen {
		timeout = time.After(askDeclineWait)
	}
	select {
	case <-pa.declined:
		return nil, true, nil
	case rec := <-pa.answer:
		return rec, false, nil
	case <-timeout:
		return nil, false, nil
	case <-ctx.Done():
		return nil, false, errNoAnswer
	case <-d.ctx.Done():
		return nil, false, errAskReplaced
	}
}

func (d *deviceSession) transcribeAnswer(ctx context.Context, pa *pendingAsk, rec []byte, lang string) (string, error) {
	d.mu.Lock()
	replyTurn := pa.replyTurn
	d.mu.Unlock()
	if rec == nil {
		return "", errNoAnswer
	}
	// The device waits in the thinking state for the answer to be taken.
	defer d.send(&models.Message{Type: models.TypeTurnComplete, TurnID: replyTurn})
	text, err := d.cfg.Recognizer.Transcribe(ctx, rec, d.hello.InputFormat, lang)
	if err != nil {
		d.sendError(replyTurn, models.ErrorCodeUnavailable, err)
		return "", err
	}
	text = strings.TrimSpace(text)
	d.log.Info("answer to announcement", "id", pa.id, "answer", text)
	if text == "" {
		return "", errNoAnswer
	}
	return text, nil
}

// language is the speech language of the device.
func (d *deviceSession) language() string {
	if d.hello.Language != "" {
		return d.hello.Language
	}
	return d.srv.Settings().Language
}

// The device side of an announcement, called from onMessage and onAudio.

// askTurnStart takes a turn answering an announcement. It reports whether
// the turn was handled here.
func (d *deviceSession) askTurnStart(m *models.Message) bool {
	if m.ReplyTo == "" {
		return false
	}
	d.mu.Lock()
	pa := d.ask
	ok := pa != nil && pa.id == m.ReplyTo && pa.listen && pa.replyTurn == ""
	if ok {
		pa.replyTurn = m.TurnID
	}
	d.mu.Unlock()
	if !ok {
		// The question expired meanwhile: drop the answer rather than
		// passing it to the model as a request.
		d.log.Info("answer to an expired announcement dropped", "announce", m.ReplyTo)
		d.send(&models.Message{Type: models.TypeTurnComplete, TurnID: m.TurnID})
	}
	return true
}

// askAudio records answer audio; it reports whether pcm was taken.
func (d *deviceSession) askAudio(pcm []byte) bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	pa := d.ask
	if pa == nil || pa.replyTurn == "" {
		return false
	}
	if pa.pcm.Len() < d.hello.InputFormat.FrameSize(maxAnswerAudio) {
		pa.pcm.Write(pcm)
	}
	return true
}

// askTurnEnd completes or cancels an answer turn or a declined
// announcement; it reports whether m concerned an announcement.
func (d *deviceSession) askTurnEnd(m *models.Message, cancelled bool) bool {
	d.mu.Lock()
	pa := d.ask
	if pa == nil {
		d.mu.Unlock()
		return m.ReplyTo != ""
	}
	var (
		declined = cancelled && m.TurnID == pa.id
		noAnswer = cancelled && (m.ReplyTo == pa.id || (pa.replyTurn != "" && m.TurnID == pa.replyTurn))
		answered = !cancelled && pa.replyTurn != "" && m.TurnID == pa.replyTurn
		rec      []byte
	)
	if answered {
		rec = bytes.Clone(pa.pcm.Bytes())
	}
	d.mu.Unlock()
	switch {
	case declined:
		select {
		case pa.declined <- struct{}{}:
		default:
		}
	case answered, noAnswer:
		select {
		case pa.answer <- rec:
		default:
		}
	default:
		return m.ReplyTo != ""
	}
	return true
}
