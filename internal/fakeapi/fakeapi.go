// Package fakeapi builds Anthropic Messages and OpenAI Responses server-sent event streams for tests.
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

// ResponsesMessageItem is an assistant message output item with one text part.
func ResponsesMessageItem(id, text string) string {
	return fmt.Sprintf(`{"type":"message","id":%q,"role":"assistant","status":"completed","content":[{"type":"output_text","text":%q,"annotations":[]}]}`, id, text)
}

// ResponsesCallItem is a function_call output item; args is the raw arguments string.
func ResponsesCallItem(id, callID, name, args string) string {
	return fmt.Sprintf(`{"type":"function_call","id":%q,"call_id":%q,"name":%q,"arguments":%q,"status":"completed"}`, id, callID, name, args)
}

func ResponsesItemAdded(index int, item string) string {
	return Event("response.output_item.added", fmt.Sprintf(`{"type":"response.output_item.added","output_index":%d,"item":%s}`, index, item))
}

func ResponsesItemDone(index int, item string) string {
	return Event("response.output_item.done", fmt.Sprintf(`{"type":"response.output_item.done","output_index":%d,"item":%s}`, index, item))
}

func ResponsesTextDelta(index int, delta string) string {
	return Event("response.output_text.delta", fmt.Sprintf(`{"type":"response.output_text.delta","output_index":%d,"delta":%q}`, index, delta))
}

// ResponsesItem frames added, then done, for one output item.
func ResponsesItem(index int, item string) string {
	return ResponsesItemAdded(index, item) + ResponsesItemDone(index, item)
}

// ResponsesCompleted frames the terminal event; output is the raw JSON array for response.output.
func ResponsesCompleted(output string) string {
	return Event("response.completed", fmt.Sprintf(`{"type":"response.completed","response":{"id":"resp_1","status":"completed","output":%s}}`, output))
}

// ResponsesIncomplete frames response.incomplete; an empty reason omits incomplete_details.
func ResponsesIncomplete(reason string) string {
	details := ""
	if reason != "" {
		details = fmt.Sprintf(`,"incomplete_details":{"reason":%q}`, reason)
	}
	return Event("response.incomplete", `{"type":"response.incomplete","response":{"id":"resp_1","status":"incomplete"`+details+`}}`)
}

func ResponsesFailed(code, message string) string {
	return Event("response.failed", fmt.Sprintf(`{"type":"response.failed","response":{"id":"resp_1","status":"failed","error":{"code":%q,"message":%q}}}`, code, message))
}

func ResponsesError(code, message string) string {
	return Event("error", fmt.Sprintf(`{"type":"error","code":%q,"message":%q}`, code, message))
}
