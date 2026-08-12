package proxy

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

type responseFormat uint8

const (
	formatResponses responseFormat = iota
	formatOpenAIChat
	formatAnthropic
)

type preparedRequest struct {
	body        []byte
	stream      bool
	model       string
	publicModel string
	format      responseFormat
}

func prepareRequest(raw []byte, format responseFormat) (preparedRequest, error) {
	var source map[string]any
	decoder := json.NewDecoder(strings.NewReader(string(raw)))
	decoder.UseNumber()
	if err := decoder.Decode(&source); err != nil || source == nil {
		return preparedRequest{}, errors.New("request body must be a JSON object")
	}
	requested := text(source["model"])
	model, ok := canonicalModel(requested)
	if !ok {
		return preparedRequest{}, fmt.Errorf("unsupported model %q", requested)
	}
	var payload map[string]any
	switch format {
	case formatOpenAIChat:
		payload = chatToResponses(source)
	case formatAnthropic:
		payload = anthropicToResponses(source)
	default:
		payload = cloneObject(source)
		delete(payload, "messages")
	}
	payload["model"] = model
	payload["store"] = false
	formatReasoning(payload, source, model)
	encoded, err := json.Marshal(payload)
	if err != nil {
		return preparedRequest{}, errors.New("failed to encode upstream request")
	}
	return preparedRequest{body: encoded, stream: boolean(source["stream"]), model: model, publicModel: requested, format: format}, nil
}

func chatToResponses(source map[string]any) map[string]any {
	out := map[string]any{"stream": boolean(source["stream"]), "store": false}
	input := make([]any, 0)
	instructions := make([]string, 0)
	messages, _ := source["messages"].([]any)
	for _, raw := range messages {
		message, _ := raw.(map[string]any)
		role := strings.ToLower(text(message["role"]))
		if role == "system" || role == "developer" {
			if value := contentText(message["content"]); value != "" {
				instructions = append(instructions, value)
			}
			continue
		}
		if role == "tool" {
			input = append(input, map[string]any{"type": "function_call_output", "call_id": text(message["tool_call_id"]), "output": contentText(message["content"])})
			continue
		}
		content := responseContent(message["content"], role == "assistant")
		if len(content) > 0 {
			input = append(input, map[string]any{"type": "message", "role": role, "content": content})
		}
		if role == "assistant" {
			if calls, ok := message["tool_calls"].([]any); ok {
				for _, rawCall := range calls {
					call, _ := rawCall.(map[string]any)
					function, _ := call["function"].(map[string]any)
					input = append(input, map[string]any{
						"type": "function_call", "call_id": first(text(call["id"]), "call_missing"),
						"name": text(function["name"]), "arguments": first(text(function["arguments"]), "{}"),
					})
				}
			}
		}
	}
	if len(input) == 0 {
		input = append(input, map[string]any{"type": "message", "role": "user", "content": []any{map[string]any{"type": "input_text", "text": ""}}})
	}
	out["input"] = input
	if len(instructions) > 0 {
		out["instructions"] = strings.Join(instructions, "\n\n")
	}
	copyFirst(out, source, "max_output_tokens", "max_completion_tokens", "max_tokens")
	copyKeys(out, source, "temperature", "top_p", "parallel_tool_calls", "user", "metadata", "service_tier")
	if tools := openAITools(source["tools"]); len(tools) > 0 {
		out["tools"] = tools
		if choice, ok := source["tool_choice"]; ok {
			out["tool_choice"] = openAIToolChoice(choice)
		}
	}
	if format, ok := source["response_format"]; ok {
		out["text"] = map[string]any{"format": format}
	}
	return out
}

func anthropicToResponses(source map[string]any) map[string]any {
	out := map[string]any{"stream": boolean(source["stream"]), "store": false}
	if system := contentText(source["system"]); system != "" {
		out["instructions"] = system
	}
	input := make([]any, 0)
	messages, _ := source["messages"].([]any)
	for _, raw := range messages {
		message, _ := raw.(map[string]any)
		role := strings.ToLower(text(message["role"]))
		blocks, isBlocks := message["content"].([]any)
		if !isBlocks {
			input = append(input, map[string]any{"type": "message", "role": role, "content": responseContent(message["content"], role == "assistant")})
			continue
		}
		content := make([]any, 0)
		for _, rawBlock := range blocks {
			block, _ := rawBlock.(map[string]any)
			switch strings.ToLower(text(block["type"])) {
			case "text":
				kind := "input_text"
				if role == "assistant" {
					kind = "output_text"
				}
				content = append(content, map[string]any{"type": kind, "text": text(block["text"])})
			case "image":
				if image := anthropicImage(block); image != nil {
					content = append(content, image)
				}
			case "tool_use":
				input = append(input, map[string]any{"type": "function_call", "call_id": text(block["id"]), "name": text(block["name"]), "arguments": jsonString(block["input"])})
			case "tool_result":
				input = append(input, map[string]any{"type": "function_call_output", "call_id": text(block["tool_use_id"]), "output": contentText(block["content"])})
			}
		}
		if len(content) > 0 {
			input = append(input, map[string]any{"type": "message", "role": role, "content": content})
		}
	}
	if len(input) == 0 {
		input = append(input, map[string]any{"type": "message", "role": "user", "content": []any{map[string]any{"type": "input_text", "text": ""}}})
	}
	out["input"] = input
	copyFirst(out, source, "max_output_tokens", "max_tokens")
	copyKeys(out, source, "temperature", "top_p", "metadata")
	if tools := anthropicTools(source["tools"]); len(tools) > 0 {
		out["tools"] = tools
	}
	if choice, ok := source["tool_choice"].(map[string]any); ok {
		switch text(choice["type"]) {
		case "auto", "none":
			out["tool_choice"] = text(choice["type"])
		case "any":
			out["tool_choice"] = "required"
		case "tool":
			out["tool_choice"] = map[string]any{"type": "function", "name": text(choice["name"])}
		}
	}
	return out
}

func responseContent(raw any, assistant bool) []any {
	kind := "input_text"
	if assistant {
		kind = "output_text"
	}
	if value, ok := raw.(string); ok {
		return []any{map[string]any{"type": kind, "text": value}}
	}
	parts, _ := raw.([]any)
	out := make([]any, 0, len(parts))
	for _, rawPart := range parts {
		part, _ := rawPart.(map[string]any)
		switch strings.ToLower(text(part["type"])) {
		case "text", "input_text", "output_text":
			out = append(out, map[string]any{"type": kind, "text": text(part["text"])})
		case "image_url", "input_image":
			var imageURL string
			switch value := part["image_url"].(type) {
			case string:
				imageURL = value
			case map[string]any:
				imageURL = text(value["url"])
			}
			if imageURL != "" {
				out = append(out, map[string]any{"type": "input_image", "image_url": imageURL})
			}
		}
	}
	return out
}

func anthropicImage(block map[string]any) any {
	source, _ := block["source"].(map[string]any)
	if source == nil {
		return nil
	}
	if text(source["type"]) == "base64" {
		media := first(text(source["media_type"]), "image/png")
		return map[string]any{"type": "input_image", "image_url": "data:" + media + ";base64," + text(source["data"])}
	}
	if value := text(source["url"]); value != "" {
		return map[string]any{"type": "input_image", "image_url": value}
	}
	return nil
}

func openAITools(raw any) []any {
	items, _ := raw.([]any)
	out := make([]any, 0, len(items))
	for _, rawItem := range items {
		item, _ := rawItem.(map[string]any)
		fn, _ := item["function"].(map[string]any)
		if text(item["type"]) != "function" || text(fn["name"]) == "" {
			continue
		}
		converted := map[string]any{"type": "function", "name": text(fn["name"]), "parameters": value(fn["parameters"], map[string]any{"type": "object"})}
		if description := text(fn["description"]); description != "" {
			converted["description"] = description
		}
		if strict, ok := fn["strict"].(bool); ok {
			converted["strict"] = strict
		}
		out = append(out, converted)
	}
	return out
}

func anthropicTools(raw any) []any {
	items, _ := raw.([]any)
	out := make([]any, 0, len(items))
	for _, rawItem := range items {
		item, _ := rawItem.(map[string]any)
		if text(item["name"]) == "" {
			continue
		}
		converted := map[string]any{"type": "function", "name": text(item["name"]), "parameters": value(item["input_schema"], map[string]any{"type": "object"})}
		if description := text(item["description"]); description != "" {
			converted["description"] = description
		}
		out = append(out, converted)
	}
	return out
}

func openAIToolChoice(raw any) any {
	if choice := text(raw); choice == "auto" || choice == "none" || choice == "required" {
		return choice
	}
	object, _ := raw.(map[string]any)
	if text(object["type"]) == "function" {
		fn, _ := object["function"].(map[string]any)
		if name := text(fn["name"]); name != "" {
			return map[string]any{"type": "function", "name": name}
		}
	}
	return raw
}

func formatReasoning(out, source map[string]any, model string) {
	effort := reasoningEffort(source)
	for _, key := range []string{"reasoning", "reasoning_effort", "reasoningEffort", "model_reasoning_effort", "modelReasoningEffort", "effort", "thinking", "output_config", "outputConfig"} {
		delete(out, key)
	}
	if (model == "grok-4.6" || model == "grok-4.5") && effort != "" {
		out["reasoning"] = map[string]any{"effort": normalizeEffort(effort, model)}
	}
}

func reasoningEffort(source map[string]any) string {
	for _, key := range []string{"reasoning_effort", "reasoningEffort", "model_reasoning_effort", "modelReasoningEffort", "effort"} {
		if result := text(source[key]); result != "" {
			return result
		}
	}
	if object, ok := source["reasoning"].(map[string]any); ok {
		if result := first(text(object["effort"]), text(object["reasoning_effort"])); result != "" {
			return result
		}
	}
	if object, ok := source["output_config"].(map[string]any); ok {
		if result := text(object["effort"]); result != "" {
			return result
		}
	}
	if object, ok := source["thinking"].(map[string]any); ok {
		if result := text(object["effort"]); result != "" {
			return result
		}
		if kind := text(object["type"]); kind == "enabled" || kind == "adaptive" {
			return "high"
		}
	}
	return ""
}

func normalizeEffort(raw, model string) string {
	if model == "grok-4.6" {
		switch strings.ToLower(strings.TrimSpace(raw)) {
		case "none", "off", "disabled", "false", "0", "minimal", "min", "low", "lite":
			return "low"
		case "medium", "med":
			return "medium"
		case "high", "normal", "standard":
			return "high"
		case "default", "auto", "xhigh", "max", "maximum", "ultra", "highest":
			return "xhigh"
		default:
			return "medium"
		}
	}
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "none", "off", "disabled", "false", "0", "minimal", "min", "low", "lite":
		return "low"
	case "medium", "med":
		return "medium"
	case "default", "auto", "normal", "standard", "high", "xhigh", "max", "maximum", "ultra", "highest":
		return "high"
	default:
		return "medium"
	}
}

func contentText(raw any) string {
	if result := text(raw); result != "" {
		return result
	}
	parts, _ := raw.([]any)
	texts := make([]string, 0, len(parts))
	for _, rawPart := range parts {
		part, _ := rawPart.(map[string]any)
		if kind := text(part["type"]); kind == "text" || kind == "input_text" || kind == "output_text" {
			texts = append(texts, text(part["text"]))
		}
	}
	return strings.Join(texts, "\n")
}

func text(value any) string {
	result, _ := value.(string)
	return strings.TrimSpace(result)
}

func boolean(value any) bool {
	result, _ := value.(bool)
	return result
}

func copyKeys(dst, src map[string]any, keys ...string) {
	for _, key := range keys {
		if item, ok := src[key]; ok {
			dst[key] = item
		}
	}
}

func copyFirst(dst, src map[string]any, target string, keys ...string) {
	for _, key := range keys {
		if item, ok := src[key]; ok {
			dst[target] = item
			return
		}
	}
}

func cloneObject(source map[string]any) map[string]any {
	out := make(map[string]any, len(source))
	for key, item := range source {
		out[key] = item
	}
	return out
}

func jsonString(value any) string {
	raw, err := json.Marshal(value)
	if err != nil {
		return "{}"
	}
	return string(raw)
}

func first(values ...string) string {
	for _, item := range values {
		if item = strings.TrimSpace(item); item != "" {
			return item
		}
	}
	return ""
}

func value(item, fallback any) any {
	if item == nil {
		return fallback
	}
	return item
}
