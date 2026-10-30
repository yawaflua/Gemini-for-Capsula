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

// Command dice is an example server plugin written with the Go SDK: it
// rolls dice for board games ("Маруся, брось два кубика").
//
//	go build -o examples/plugins/dice/dice ./examples/plugins/dice
package main

import (
	"context"
	"fmt"
	"log"
	"math/rand/v2"

	"github.com/yawaflua/marusya-gemini/pkg/pluginsdk"
)

func ptr(f float64) *float64 { return &f }

func main() {
	p := pluginsdk.New()
	maxDice := 10
	p.OnInit(func(context.Context, pluginsdk.Init) error {
		var cfg struct {
			MaxDice int `json:"max_dice"`
		}
		if err := p.Config(&cfg); err != nil {
			return err
		}
		if cfg.MaxDice > 0 {
			maxDice = cfg.MaxDice
		}
		return nil
	})
	p.Tool(pluginsdk.ToolSpec{
		Name:        "roll_dice",
		Description: "Rolls dice for a board game and returns each value and the sum. Use it whenever the user asks to roll or throw dice.",
		Params: []pluginsdk.Param{
			{Name: "count", Type: pluginsdk.TypeInteger, Description: "How many dice, 1 by default.", Minimum: ptr(1), Maximum: ptr(100)},
			{Name: "sides", Type: pluginsdk.TypeInteger, Description: "Sides of each die, 6 by default.", Minimum: ptr(2), Maximum: ptr(1000)},
		},
	}, func(ctx context.Context, c pluginsdk.Call) (any, error) {
		count, sides := c.Int("count", 1), c.Int("sides", 6)
		if count < 1 || count > maxDice {
			return nil, fmt.Errorf("can roll 1 to %d dice", maxDice)
		}
		if sides < 2 {
			return nil, fmt.Errorf("a die needs at least 2 sides")
		}
		values, sum := make([]int, count), 0
		for i := range values {
			values[i] = 1 + rand.IntN(sides)
			sum += values[i]
		}
		p.Log("info", "%s rolled %v", c.Device.ID, values)
		return map[string]any{"values": values, "sum": sum}, nil
	})
	if err := p.Run(); err != nil {
		log.Fatal(err)
	}
}
