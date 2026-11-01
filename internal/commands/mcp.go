package commands

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/wolfeidau/durable/mcp"
)

const mcpPinFile = "mcp-tools.json"

// mcpTools is the session's MCP tool list plus what to tell the user about how it was built.
type mcpTools struct {
	Tools    []mcp.Pinned
	Warnings []string
	// Pinned means the list came from an earlier pin rather than fresh discovery.
	Pinned bool
}

// pinMCPTools pins the tool list on first start; existed (session.sqlite predates this run) pins an older session empty.
func pinMCPTools(ctx context.Context, dir string, existed bool, discover func(context.Context) ([]mcp.Pinned, []string)) (mcpTools, error) {
	pin := filepath.Join(dir, mcpPinFile)
	if res, ok, err := readMCPPin(pin); ok || err != nil {
		return res, err
	}

	res := mcpTools{Tools: []mcp.Pinned{}}
	if !existed {
		res.Tools, res.Warnings = discover(ctx)
		if res.Tools == nil {
			res.Tools = []mcp.Pinned{}
		}
	}
	data, err := json.Marshal(res.Tools)
	if err != nil {
		return mcpTools{}, fmt.Errorf("failed to encode MCP tools: %w", err)
	}
	won, err := writePin(dir, pin, data)
	if err != nil {
		return mcpTools{}, err
	}
	if !won {
		// Another process pinned first; its list wins so both agree.
		res, ok, err := readMCPPin(pin)
		if err != nil {
			return mcpTools{}, err
		}
		if !ok {
			return mcpTools{}, errors.New("pinned MCP tools disappeared")
		}
		return res, nil
	}
	res.Pinned = existed
	return res, nil
}

func readMCPPin(pin string) (mcpTools, bool, error) {
	b, err := os.ReadFile(pin) //nolint:gosec // pin is a fixed name inside the session directory
	if errors.Is(err, fs.ErrNotExist) {
		return mcpTools{}, false, nil
	}
	if err != nil {
		return mcpTools{}, false, fmt.Errorf("failed to read pinned MCP tools: %w", err)
	}
	var tools []mcp.Pinned
	if err := json.Unmarshal(b, &tools); err != nil {
		return mcpTools{}, false, fmt.Errorf("failed to parse pinned MCP tools %s: %w", pin, err)
	}
	return mcpTools{Tools: tools, Pinned: true}, true, nil
}
