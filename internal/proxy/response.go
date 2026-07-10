package proxy

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

type usage struct {
	Input  int64
	Output int64
	Cached int64
}

type parsedOutput struct {
	Text      string
	Thinking  string
	ToolCalls []any
	Usage     usage
	ID        string
	Created   int64
	Status    string
}

func writeNonStream(w http.ResponseWriter, resp *http.Response, format responseFormat, model, requestID string) error {
	defer resp.Body.Close()
	decoder := json.NewDecoder(io.LimitReader(resp.Body, 32<<20))
	decoder.UseNumber()
	var source map[string]any
	if err := decoder.Decode(&source); err != nil {
		return errors.New("upstream returned invalid JSON")
	}
	parsed := parseOutput(source)
	if parsed.ID == "" {
		parsed.ID = requestID
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	switch format {
	case formatOpenAIChat:
		return json.NewEncoder(w).Encode(chatResponse(parsed, model))
	case formatAnthropic:
		return json.NewEncoder(w).Encode(anthropicResponse(parsed, model))
	default:
		return json.NewEncoder(w).Encode(source)
	}
}

func parseOutput(source map[string]any) parsedOutput {
	result := parsedOutput{ID: text(source["id"]), Created: integer(source["created_at"]), Status: text(source["status"]), Usage: parseUsage(source["usage"])}
	if result.Created == 0 {
		result.Created = time.Now().Unix()
	}
	outputs, _ := source["output"].([]any)
	texts := make([]string, 0)
	thoughts := make([]string, 0)
	for _, raw := range outputs {
		item, _ := raw.(map[string]any)
		switch text(item["type"]) {
		case "message":
			parts, _ := item["content"].([]any)
			for _, rawPart := range parts {
				part, _ := rawPart.(map[string]any)
				switch text(part["type"]) {
				case "output_text", "text":
					texts = append(texts, stringValue(part["text"]))
				case "refusal":
					texts = append(texts, stringValue(part["refusal"]))
				}
			}
		case "reasoning":
			summary, _ := item["summary"].([]any)
			for _, rawPart := range summary {
				part, _ := rawPart.(map[string]any)
				thoughts = append(thoughts, stringValue(part["text"]))
			}
		case "function_call":
			callID := first(text(item["call_id"]), text(item["id"]), "call_missing")
			result.ToolCalls = append(result.ToolCalls, map[string]any{
				"id": callID, "type": "function",
				"function": map[string]any{"name": text(item["name"]), "arguments": first(stringValue(item["arguments"]), "{}")},
			})
		}
	}
	result.Text = strings.Join(texts, "")
	result.Thinking = strings.Join(thoughts, "\n")
	return result
}

func chatResponse(output parsedOutput, model string) map[string]any {
	message := map[string]any{"role": "assistant", "content": output.Text}
	if output.Thinking != "" {
		message["reasoning_content"] = output.Thinking
	}
	finish := "stop"
	if len(output.ToolCalls) > 0 {
		message["tool_calls"] = output.ToolCalls
		finish = "tool_calls"
	} else if output.Status == "incomplete" {
		finish = "length"
	}
	return map[string]any{
		"id": output.ID, "object": "chat.completion", "created": output.Created, "model": model,
		"choices": []any{map[string]any{"index": 0, "message": message, "finish_reason": finish}},
		"usage": map[string]any{
			"prompt_tokens": output.Usage.Input, "completion_tokens": output.Usage.Output,
			"total_tokens":          output.Usage.Input + output.Usage.Output,
			"prompt_tokens_details": map[string]any{"cached_tokens": output.Usage.Cached},
		},
	}
}

func anthropicResponse(output parsedOutput, model string) map[string]any {
	content := make([]any, 0, len(output.ToolCalls)+2)
	if output.Thinking != "" {
		content = append(content, map[string]any{"type": "thinking", "thinking": output.Thinking})
	}
	if output.Text != "" {
		content = append(content, map[string]any{"type": "text", "text": output.Text})
	}
	for _, raw := range output.ToolCalls {
		call, _ := raw.(map[string]any)
		fn, _ := call["function"].(map[string]any)
		input := map[string]any{}
		_ = json.Unmarshal([]byte(stringValue(fn["arguments"])), &input)
		content = append(content, map[string]any{"type": "tool_use", "id": text(call["id"]), "name": text(fn["name"]), "input": input})
	}
	if len(content) == 0 {
		content = append(content, map[string]any{"type": "text", "text": ""})
	}
	stop := "end_turn"
	if len(output.ToolCalls) > 0 {
		stop = "tool_use"
	} else if output.Status == "incomplete" {
		stop = "max_tokens"
	}
	id := output.ID
	if !strings.HasPrefix(id, "msg_") {
		id = "msg_" + strings.TrimPrefix(id, "resp_")
	}
	return map[string]any{
		"id": id, "type": "message", "role": "assistant", "model": model,
		"content": content, "stop_reason": stop, "stop_sequence": nil,
		"usage": map[string]any{
			"input_tokens": output.Usage.Input, "output_tokens": output.Usage.Output,
			"cache_creation_input_tokens": 0, "cache_read_input_tokens": output.Usage.Cached,
		},
	}
}

func parseUsage(raw any) usage {
	object, _ := raw.(map[string]any)
	result := usage{Input: integer(object["input_tokens"]), Output: integer(object["output_tokens"])}
	details, _ := object["input_tokens_details"].(map[string]any)
	result.Cached = integer(details["cached_tokens"])
	return result
}

func integer(value any) int64 {
	switch typed := value.(type) {
	case json.Number:
		result, _ := typed.Int64()
		return result
	case float64:
		return int64(typed)
	case int64:
		return typed
	case int:
		return int64(typed)
	}
	return 0
}

func stringValue(value any) string {
	result, _ := value.(string)
	return result
}

func copyUpstreamError(w http.ResponseWriter, resp *http.Response) error {
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 512<<10))
	if err != nil {
		return err
	}
	contentType := resp.Header.Get("Content-Type")
	if !strings.Contains(strings.ToLower(contentType), "json") {
		raw, _ = json.Marshal(map[string]any{"error": map[string]any{"type": "upstream_error", "message": fmt.Sprintf("Grok upstream returned HTTP %d", resp.StatusCode)}})
		contentType = "application/json"
	}
	w.Header().Set("Content-Type", contentType)
	w.Header().Set("X-Content-Type-Options", "nosniff")
	if retryAfter := resp.Header.Get("Retry-After"); retryAfter != "" {
		w.Header().Set("Retry-After", retryAfter)
	}
	w.WriteHeader(resp.StatusCode)
	_, err = w.Write(raw)
	return err
}
