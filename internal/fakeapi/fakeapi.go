// Package fakeapi builds Anthropic Messages API server-sent event streams for tests.
package fakeapi

import (
	"fmt"
	"strings"
)

// Event frames one server-sent event.
func Event(name, data string) string {
	return "event: " + name + "\ndata: " + data + "\n\n"
}

func MessageStart(model string) string {
	return Event("message_start", fmt.Sprintf(`{"type":"message_start","message":{"id":"msg_1","type":"message","role":"assistant","model":%q,"content":[],"stop_reason":null,"stop_sequence":null,"usage":{"input_tokens":1,"output_tokens":1}}}`, model))
}

func BlockStart(i int, block string) string {
	return Event("content_block_start", fmt.Sprintf(`{"type":"content_block_start","index":%d,"content_block":%s}`, i, block))
}

func BlockDelta(i int, delta string) string {
	return Event("content_block_delta", fmt.Sprintf(`{"type":"content_block_delta","index":%d,"delta":%s}`, i, delta))
}

func BlockStop(i int) string {
	return Event("content_block_stop", fmt.Sprintf(`{"type":"content_block_stop","index":%d}`, i))
}

func TextBlock(i int, parts ...string) string {
	var s strings.Builder
	s.WriteString(BlockStart(i, `{"type":"text","text":""}`))
	for _, p := range parts {
		s.WriteString(BlockDelta(i, fmt.Sprintf(`{"type":"text_delta","text":%q}`, p)))
	}
	s.WriteString(BlockStop(i))
	return s.String()
}

func ToolBlock(i int, id, name string, parts ...string) string {
	var s strings.Builder
	s.WriteString(BlockStart(i, fmt.Sprintf(`{"type":"tool_use","id":%q,"name":%q,"input":{}}`, id, name)))
	for _, p := range parts {
		s.WriteString(BlockDelta(i, fmt.Sprintf(`{"type":"input_json_delta","partial_json":%q}`, p)))
	}
	s.WriteString(BlockStop(i))
	return s.String()
}

// Stream frames a whole message; details is the raw JSON for stop_details ("null" for none).
func Stream(model, stop, details string, blocks ...string) string {
	s := MessageStart(model) + strings.Join(blocks, "")
	s += Event("message_delta", fmt.Sprintf(`{"type":"message_delta","delta":{"stop_reason":%q,"stop_sequence":null,"stop_details":%s},"usage":{"output_tokens":5}}`, stop, details))
	return s + Event("message_stop", `{"type":"message_stop"}`)
}
