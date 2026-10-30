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

package claude

import (
	"context"
	"encoding/json"

	"github.com/anthropics/anthropic-sdk-go"

	"github.com/yawaflua/marusya-gemini/internal/provider"
	"github.com/yawaflua/marusya-gemini/internal/provider/pipeline"
)

const (
	// maxIterations bounds the tool-call loop of one turn.
	maxIterations = 8
	// refusalReply is spoken when Claude declines to answer.
	refusalReply = "Извини, с этим я помочь не могу."
)

// conversation is the Claude side of a [pipeline.Session].
type conversation struct {
	p       *Provider
	params  anthropic.MessageNewParams
	history []anthropic.MessageParam
}

var _ pipeline.Conversation = (*conversation)(nil)

// Turn implements [pipeline.Conversation].
func (c *conversation) Turn(ctx context.Context, text string, out *pipeline.Output) error {
	// Messages of this turn join the history only if it completes, so an
	// abandoned turn never leaves an unanswered tool call behind.
	msgs := append(c.history[:len(c.history):len(c.history)],
		anthropic.NewUserMessage(anthropic.NewTextBlock(text)))
	for range maxIterations {
		msg, err := c.generate(ctx, msgs, out)
		if err != nil {
			return err
		}
		msgs = append(msgs, msg.ToParam())

		switch msg.StopReason {
		case anthropic.StopReasonToolUse:
			results, ok := runTools(msg, out)
			if !ok {
				return nil
			}
			msgs = append(msgs, anthropic.NewUserMessage(results...))
			continue
		case anthropic.StopReasonPauseTurn:
			continue // a long server tool (web search) paused; resume it
		case anthropic.StopReasonRefusal:
			c.p.log.Info("claude: refused", "category", msg.StopDetails.Category)
			if !out.Spoke() {
				out.Say(refusalReply)
			}
		}
		break
	}
	if ctx.Err() == nil {
		c.history = msgs
	}
	return nil
}

// generate streams one response, speaking text as it arrives.
func (c *conversation) generate(ctx context.Context, msgs []anthropic.MessageParam, out *pipeline.Output) (*anthropic.Message, error) {
	params := c.params
	params.Messages = msgs
	stream := c.p.client.Messages.NewStreaming(ctx, params)
	msg := &anthropic.Message{}
	for stream.Next() {
		ev := stream.Current()
		if err := msg.Accumulate(ev); err != nil {
			return nil, err
		}
		if d, ok := ev.AsAny().(anthropic.ContentBlockDeltaEvent); ok {
			if t, ok := d.Delta.AsAny().(anthropic.TextDelta); ok {
				out.Write(t.Text)
			}
		}
	}
	if err := stream.Err(); err != nil {
		return nil, err
	}
	out.Flush()
	return msg, nil
}

// runTools forwards Claude's client tool calls and converts the results.
func runTools(msg *anthropic.Message, out *pipeline.Output) ([]anthropic.ContentBlockParamUnion, bool) {
	var calls []provider.ToolCall
	for _, b := range msg.Content {
		tu, ok := b.AsAny().(anthropic.ToolUseBlock)
		if !ok {
			continue
		}
		args := map[string]any{}
		if raw := tu.JSON.Input.Raw(); raw != "" {
			if err := json.Unmarshal([]byte(raw), &args); err != nil {
				args = map[string]any{}
			}
		}
		calls = append(calls, provider.ToolCall{ID: tu.ID, Name: tu.Name, Args: args})
	}
	if len(calls) == 0 {
		return nil, false
	}
	results, ok := out.CallTools(calls)
	if !ok {
		return nil, false
	}
	// Every tool_use needs a tool_result, in one user message.
	blocks := make([]anthropic.ContentBlockParamUnion, 0, len(results))
	for _, r := range results {
		if r.Err != "" {
			blocks = append(blocks, anthropic.NewToolResultBlock(r.ID, r.Err, true))
			continue
		}
		b, _ := json.Marshal(r.Output)
		if r.Output == nil {
			b = []byte(`{"output":"ok"}`)
		}
		blocks = append(blocks, anthropic.NewToolResultBlock(r.ID, string(b), false))
	}
	return blocks, true
}
