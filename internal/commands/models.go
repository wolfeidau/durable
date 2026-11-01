package commands

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"
)

const (
	modelsTimeout = 5 * time.Second
	// The full model list is several MB.
	modelsMaxBytes = 32 << 20
)

// fetchContextWindow reads the model's context length from the provider's model list.
// Errors never include the key.
func fetchContextWindow(ctx context.Context, client *http.Client, baseURL, key, model string) (int64, error) {
	ctx, cancel := context.WithTimeout(ctx, modelsTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, baseURL+"/v1/models", nil)
	if err != nil {
		return 0, fmt.Errorf("failed to build models request: %w", err)
	}
	if key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	resp, err := client.Do(req)
	if err != nil {
		return 0, fmt.Errorf("failed to list models: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return 0, fmt.Errorf("model list returned HTTP %d", resp.StatusCode)
	}
	var list struct {
		Data []struct {
			ID            string `json:"id"`
			ContextLength int64  `json:"context_length"`
		} `json:"data"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, modelsMaxBytes)).Decode(&list); err != nil {
		return 0, fmt.Errorf("failed to decode model list: %w", err)
	}
	for _, m := range list.Data {
		if m.ID != model {
			continue
		}
		if m.ContextLength <= 0 {
			return 0, fmt.Errorf("model %s reports no context length", model)
		}
		return m.ContextLength, nil
	}
	return 0, errors.New("model " + model + " is not in the model list")
}
