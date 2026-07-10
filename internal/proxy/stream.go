package proxy

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

func streamResponses(w http.ResponseWriter, resp *http.Response) error {
	defer resp.Body.Close()
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(resp.StatusCode)
	buffer := make([]byte, 32<<10)
	flusher, _ := w.(http.Flusher)
	for {
		n, readErr := resp.Body.Read(buffer)
		if n > 0 {
			if _, err := w.Write(buffer[:n]); err != nil {
				return err
			}
			if flusher != nil {
				flusher.Flush()
			}
		}
		if readErr == io.EOF {
			return nil
		}
		if readErr != nil {
			return readErr
		}
	}
}

func streamConverted(w http.ResponseWriter, resp *http.Response, format responseFormat, model, requestID string) error {
	defer resp.Body.Close()
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(resp.StatusCode)
	state := streamState{format: format, model: model, requestID: requestID, created: time.Now().Unix(), openBlock: -1}
	if format == formatAnthropic {
		state.writeAnthropicStart(w)
	}
	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 32<<10), 8<<20)
	flusher, _ := w.(http.Flusher)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		data := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if data == "" || data == "[DONE]" {
			continue
		}
		var event map[string]any
		decoder := json.NewDecoder(strings.NewReader(data))
		decoder.UseNumber()
		if decoder.Decode(&event) != nil {
			continue
		}
		if err := state.consume(w, event); err != nil {
			return err
		}
		if flusher != nil {
			flusher.Flush()
		}
	}
	if err := scanner.Err(); err != nil {
		return err
	}
	return state.finish(w, false, usage{})
}

type streamState struct {
	format      responseFormat
	model       string
	requestID   string
	created     int64
	started     bool
	finished    bool
	hadTool     bool
	openBlock   int
	blockKind   string
	nextBlock   int
	toolIndexes map[string]int
}

func (s *streamState) consume(w io.Writer, event map[string]any) error {
	kind := text(event["type"])
	switch kind {
	case "response.created", "response.in_progress":
		response, _ := event["response"].(map[string]any)
		if id := text(response["id"]); id != "" {
			s.requestID = id
		}
		return s.ensureChatStart(w)
	case "response.output_text.delta":
		return s.delta(w, "text", stringValue(event["delta"]), event)
	case "response.reasoning_summary_text.delta", "response.reasoning_text.delta":
		return s.delta(w, "thinking", stringValue(event["delta"]), event)
	case "response.output_item.added":
		item, _ := event["item"].(map[string]any)
		if text(item["type"]) == "function_call" {
			return s.startTool(w, item)
		}
	case "response.function_call_arguments.delta":
		return s.toolDelta(w, first(text(event["item_id"]), text(event["call_id"])), stringValue(event["delta"]))
	case "response.completed", "response.incomplete", "response.failed":
		response, _ := event["response"].(map[string]any)
		return s.finish(w, kind == "response.incomplete", parseUsage(response["usage"]))
	}
	return nil
}

func (s *streamState) ensureChatStart(w io.Writer) error {
	if s.format != formatOpenAIChat || s.started {
		return nil
	}
	s.started = true
	return writeSSEData(w, map[string]any{
		"id": s.requestID, "object": "chat.completion.chunk", "created": s.created, "model": s.model,
		"choices": []any{map[string]any{"index": 0, "delta": map[string]any{"role": "assistant"}, "finish_reason": nil}},
	})
}

func (s *streamState) delta(w io.Writer, kind, delta string, event map[string]any) error {
	if delta == "" {
		return nil
	}
	if s.format == formatOpenAIChat {
		if err := s.ensureChatStart(w); err != nil {
			return err
		}
		key := "content"
		if kind == "thinking" {
			key = "reasoning_content"
		}
		return writeSSEData(w, map[string]any{
			"id": s.requestID, "object": "chat.completion.chunk", "created": s.created, "model": s.model,
			"choices": []any{map[string]any{"index": 0, "delta": map[string]any{key: delta}, "finish_reason": nil}},
		})
	}
	if err := s.ensureAnthropicBlock(w, kind); err != nil {
		return err
	}
	deltaType := "text_delta"
	field := "text"
	if kind == "thinking" {
		deltaType, field = "thinking_delta", "thinking"
	}
	return writeSSEEvent(w, "content_block_delta", map[string]any{"type": "content_block_delta", "index": s.openBlock, "delta": map[string]any{"type": deltaType, field: delta}})
}

func (s *streamState) startTool(w io.Writer, item map[string]any) error {
	s.hadTool = true
	id := first(text(item["id"]), text(item["call_id"]), fmt.Sprintf("call_%d", s.nextBlock))
	name := text(item["name"])
	if s.toolIndexes == nil {
		s.toolIndexes = make(map[string]int)
	}
	if s.format == formatOpenAIChat {
		if err := s.ensureChatStart(w); err != nil {
			return err
		}
		index := len(s.toolIndexes)
		s.toolIndexes[id] = index
		return writeSSEData(w, map[string]any{
			"id": s.requestID, "object": "chat.completion.chunk", "created": s.created, "model": s.model,
			"choices": []any{map[string]any{"index": 0, "delta": map[string]any{"tool_calls": []any{map[string]any{"index": index, "id": id, "type": "function", "function": map[string]any{"name": name, "arguments": ""}}}}, "finish_reason": nil}},
		})
	}
	if err := s.closeAnthropicBlock(w); err != nil {
		return err
	}
	index := s.nextBlock
	s.nextBlock++
	s.openBlock, s.blockKind = index, "tool"
	s.toolIndexes[id] = index
	return writeSSEEvent(w, "content_block_start", map[string]any{"type": "content_block_start", "index": index, "content_block": map[string]any{"type": "tool_use", "id": id, "name": name, "input": map[string]any{}}})
}

func (s *streamState) toolDelta(w io.Writer, id, delta string) error {
	if delta == "" {
		return nil
	}
	index := s.toolIndexes[id]
	if s.format == formatOpenAIChat {
		return writeSSEData(w, map[string]any{
			"id": s.requestID, "object": "chat.completion.chunk", "created": s.created, "model": s.model,
			"choices": []any{map[string]any{"index": 0, "delta": map[string]any{"tool_calls": []any{map[string]any{"index": index, "function": map[string]any{"arguments": delta}}}}, "finish_reason": nil}},
		})
	}
	return writeSSEEvent(w, "content_block_delta", map[string]any{"type": "content_block_delta", "index": index, "delta": map[string]any{"type": "input_json_delta", "partial_json": delta}})
}

func (s *streamState) writeAnthropicStart(w io.Writer) error {
	s.started = true
	return writeSSEEvent(w, "message_start", map[string]any{
		"type":    "message_start",
		"message": map[string]any{"id": "msg_" + strings.TrimPrefix(s.requestID, "resp_"), "type": "message", "role": "assistant", "model": s.model, "content": []any{}, "stop_reason": nil, "stop_sequence": nil, "usage": map[string]any{"input_tokens": 0, "output_tokens": 0}},
	})
}

func (s *streamState) ensureAnthropicBlock(w io.Writer, kind string) error {
	if s.openBlock >= 0 && s.blockKind == kind {
		return nil
	}
	if err := s.closeAnthropicBlock(w); err != nil {
		return err
	}
	index := s.nextBlock
	s.nextBlock++
	s.openBlock, s.blockKind = index, kind
	block := map[string]any{"type": "text", "text": ""}
	if kind == "thinking" {
		block = map[string]any{"type": "thinking", "thinking": "", "signature": ""}
	}
	return writeSSEEvent(w, "content_block_start", map[string]any{"type": "content_block_start", "index": index, "content_block": block})
}

func (s *streamState) closeAnthropicBlock(w io.Writer) error {
	if s.format != formatAnthropic || s.openBlock < 0 {
		return nil
	}
	err := writeSSEEvent(w, "content_block_stop", map[string]any{"type": "content_block_stop", "index": s.openBlock})
	s.openBlock, s.blockKind = -1, ""
	return err
}

func (s *streamState) finish(w io.Writer, incomplete bool, tokens usage) error {
	if s.finished {
		return nil
	}
	s.finished = true
	if s.format == formatOpenAIChat {
		if err := s.ensureChatStart(w); err != nil {
			return err
		}
		reason := "stop"
		if s.hadTool {
			reason = "tool_calls"
		} else if incomplete {
			reason = "length"
		}
		if err := writeSSEData(w, map[string]any{
			"id": s.requestID, "object": "chat.completion.chunk", "created": s.created, "model": s.model,
			"choices": []any{map[string]any{"index": 0, "delta": map[string]any{}, "finish_reason": reason}},
			"usage":   map[string]any{"prompt_tokens": tokens.Input, "completion_tokens": tokens.Output, "total_tokens": tokens.Input + tokens.Output},
		}); err != nil {
			return err
		}
		_, err := io.WriteString(w, "data: [DONE]\n\n")
		return err
	}
	if err := s.closeAnthropicBlock(w); err != nil {
		return err
	}
	stop := "end_turn"
	if s.hadTool {
		stop = "tool_use"
	} else if incomplete {
		stop = "max_tokens"
	}
	if err := writeSSEEvent(w, "message_delta", map[string]any{"type": "message_delta", "delta": map[string]any{"stop_reason": stop, "stop_sequence": nil}, "usage": map[string]any{"output_tokens": tokens.Output}}); err != nil {
		return err
	}
	return writeSSEEvent(w, "message_stop", map[string]any{"type": "message_stop"})
}

func writeSSEData(w io.Writer, payload any) error {
	raw, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	_, err = fmt.Fprintf(w, "data: %s\n\n", raw)
	return err
}

func writeSSEEvent(w io.Writer, event string, payload any) error {
	raw, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	_, err = fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event, raw)
	return err
}
