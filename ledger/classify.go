package ledger

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

// The prompt that classified 9/9 sample merchants correctly in testing.
const classifyPrompt = `Classify this card purchase merchant into one category.
food = groceries, restaurants, cafes, fast food
going_out = bars, clubs, concerts, music venues, event tickets
transportation = gas, rideshare, transit, parking
other = anything else

Merchant: `

// Ollama returns a Classifier that asks a local Ollama model, for example
// Ollama("http://127.0.0.1:11434", "gemma3:4b"). A JSON schema forces the
// reply to be exactly one of Categories, so the model can't ramble or
// invent a category, whatever the merchant name says.
func Ollama(baseURL, model string) Classifier {
	client := &http.Client{Timeout: 2 * time.Minute} // the first call loads the model (~20 s on CPU)
	return func(ctx context.Context, merchant string) (string, error) {
		reqBody, err := json.Marshal(map[string]any{
			"model":   model,
			"prompt":  classifyPrompt + merchant,
			"stream":  false,
			"options": map[string]any{"temperature": 0}, // same merchant → same answer
			"format": map[string]any{
				"type":     "object",
				"required": []string{"category"},
				"properties": map[string]any{
					"category": map[string]any{"type": "string", "enum": Categories},
				},
			},
		})
		if err != nil {
			return "", err
		}
		req, err := http.NewRequestWithContext(ctx, "POST", baseURL+"/api/generate", bytes.NewReader(reqBody))
		if err != nil {
			return "", err
		}
		resp, err := client.Do(req)
		if err != nil {
			return "", fmt.Errorf("ollama: %w", err)
		}
		defer resp.Body.Close()
		body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		if err != nil {
			return "", fmt.Errorf("ollama: %w", err)
		}
		if resp.StatusCode != http.StatusOK {
			return "", fmt.Errorf("ollama: %s: %.200s", resp.Status, body)
		}

		// Ollama wraps the model's JSON answer in a "response" string.
		var outer struct {
			Response string `json:"response"`
		}
		var answer struct {
			Category string `json:"category"`
		}
		if err := json.Unmarshal(body, &outer); err != nil {
			return "", fmt.Errorf("ollama: bad reply: %w", err)
		}
		if err := json.Unmarshal([]byte(outer.Response), &answer); err != nil {
			return "", fmt.Errorf("ollama: bad answer %.100q: %w", outer.Response, err)
		}
		return answer.Category, nil
	}
}
