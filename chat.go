package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

const defaultMaxTokens = 4096

type object = map[string]any

type chatRequest struct {
	Model         string        `json:"model"`
	Messages      []chatMessage `json:"messages"`
	Stream        bool          `json:"stream"`
	StreamOptions struct {
		IncludeUsage bool `json:"include_usage"`
	} `json:"stream_options"`
	MaxTokens         int             `json:"max_tokens"`
	MaxCompletion     int             `json:"max_completion_tokens"`
	Temperature       *float64        `json:"temperature"`
	TopP              *float64        `json:"top_p"`
	Stop              json.RawMessage `json:"stop"`
	N                 *int            `json:"n"`
	Tools             []chatTool      `json:"tools"`
	ToolChoice        json.RawMessage `json:"tool_choice"`
	ParallelToolCalls *bool           `json:"parallel_tool_calls"`
	ResponseFormat    struct {
		Type string `json:"type"`
	} `json:"response_format"`
}

type chatMessage struct {
	Role       string          `json:"role"`
	Content    json.RawMessage `json:"content"`
	ToolCalls  []chatCall      `json:"tool_calls"`
	ToolCallID string          `json:"tool_call_id"`
}

type chatCall struct {
	ID       string `json:"id"`
	Type     string `json:"type"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}

type chatTool struct {
	Type     string `json:"type"`
	Function struct {
		Name        string          `json:"name"`
		Description string          `json:"description"`
		Parameters  json.RawMessage `json:"parameters"`
	} `json:"function"`
}

func (s *server) chat(w http.ResponseWriter, r *http.Request) {
	body, ok := readBody(w, r, false)
	if !ok {
		return
	}
	var chat chatRequest
	if err := json.Unmarshal(body, &chat); err != nil {
		writeAPIError(w, false, 400, "invalid_request_error", "Invalid Chat Completions request")
		return
	}
	for _, id := range s.models {
		if strings.EqualFold(chat.Model, id) {
			chat.Model = id
			break
		}
	}
	payload, err := chat.toAnthropic()
	if err != nil {
		writeAPIError(w, false, 400, "invalid_request_error", err.Error())
		return
	}
	res, err := s.forward(r, s.z.oauthBase+"/ultra-zai/anthropic/v1/messages", payload)
	if err != nil {
		writeForwardError(w, false, err)
		return
	}
	defer res.Body.Close()

	contentType := res.Header.Get("Content-Type")
	for _, name := range []string{"Request-Id", "X-Request-Id", "Retry-After"} {
		if value := res.Header.Get(name); value != "" {
			w.Header().Set(name, value)
		}
	}
	if res.StatusCode >= 200 && res.StatusCode < 300 && strings.HasPrefix(contentType, "text/event-stream") {
		streamChat(w, res.Body, chat)
		return
	}

	var message claudeMessage
	err = json.NewDecoder(io.LimitReader(res.Body, 16<<20)).Decode(&message)
	if res.StatusCode >= 400 || message.Error.Message != "" || message.Type == "error" || len(message.Code) != 0 && string(message.Code) != "null" {
		status := res.StatusCode
		if status < 400 {
			status = 502
		}
		kind := message.Error.Type
		if kind == "" {
			kind = "api_error"
		}
		text := message.Error.Message
		if text == "" {
			text = message.Msg
		}
		if text == "" {
			text = "Request to Z.AI failed"
		}
		writeAPIError(w, false, status, kind, text)
		return
	}
	if err != nil || res.StatusCode >= 300 || message.ID == "" || chat.Stream {
		writeAPIError(w, false, 502, "api_error", "Unexpected response from Z.AI")
		return
	}
	writeJSON(w, 200, message.completion(chat.Model))
}

func (chat chatRequest) toAnthropic() ([]byte, error) {
	if strings.TrimSpace(chat.Model) == "" || len(chat.Messages) == 0 {
		return nil, errors.New("model and messages are required")
	}
	if chat.N != nil && *chat.N != 1 {
		return nil, errors.New("only n=1 is supported")
	}
	if format := chat.ResponseFormat.Type; format != "" && format != "text" {
		return nil, errors.New("only text response_format is supported")
	}

	maxTokens := chat.MaxCompletion
	if maxTokens == 0 {
		maxTokens = chat.MaxTokens
	}
	if maxTokens == 0 {
		maxTokens = defaultMaxTokens
	}
	if maxTokens < 0 {
		return nil, errors.New("max_tokens must be positive")
	}

	out := object{"model": chat.Model, "max_tokens": maxTokens, "stream": chat.Stream}
	if chat.Temperature != nil {
		out["temperature"] = *chat.Temperature
	}
	if chat.TopP != nil {
		out["top_p"] = *chat.TopP
	}
	if len(chat.Stop) > 0 && string(chat.Stop) != "null" {
		var stop []string
		var single string
		if json.Unmarshal(chat.Stop, &single) == nil {
			stop = []string{single}
		} else if json.Unmarshal(chat.Stop, &stop) != nil {
			return nil, errors.New("stop must be a string or an array of strings")
		}
		if len(stop) > 0 {
			out["stop_sequences"] = stop
		}
	}

	var system []object
	var messages []struct {
		Role    string   `json:"role"`
		Content []object `json:"content"`
	}
	for i, message := range chat.Messages {
		blocks, err := contentBlocks(message.Content)
		if err != nil {
			return nil, fmt.Errorf("messages[%d]: %w", i, err)
		}
		role := message.Role
		switch role {
		case "system", "developer":
			for _, block := range blocks {
				if block["type"] != "text" {
					return nil, errors.New("system and developer messages must be text")
				}
			}
			system = append(system, blocks...)
			continue
		case "tool":
			if message.ToolCallID == "" {
				return nil, errors.New("tool messages require tool_call_id")
			}
			role = "user"
			result := object{"type": "tool_result", "tool_use_id": message.ToolCallID}
			if len(blocks) > 0 {
				result["content"] = blocks
			}
			blocks = []object{result}
		case "assistant":
			for _, call := range message.ToolCalls {
				var input map[string]json.RawMessage
				if call.ID == "" || call.Type != "function" || call.Function.Name == "" ||
					json.Unmarshal([]byte(call.Function.Arguments), &input) != nil || input == nil {
					return nil, errors.New("tool_calls require an ID, function name and a JSON object in arguments")
				}
				blocks = append(blocks, object{"type": "tool_use", "id": call.ID, "name": call.Function.Name, "input": input})
			}
		case "user":
		default:
			return nil, fmt.Errorf("unsupported role %q", role)
		}
		if len(blocks) == 0 {
			continue
		}
		if last := len(messages) - 1; last >= 0 && messages[last].Role == role {
			messages[last].Content = append(messages[last].Content, blocks...)
			continue
		}
		messages = append(messages, struct {
			Role    string   `json:"role"`
			Content []object `json:"content"`
		}{role, blocks})
	}
	if len(messages) == 0 {
		return nil, errors.New("at least one message with content is required")
	}
	out["messages"] = messages
	if len(system) > 0 {
		out["system"] = system
	}
	if err := chat.translateTools(out); err != nil {
		return nil, err
	}
	return json.Marshal(out)
}

func contentBlocks(content json.RawMessage) ([]object, error) {
	if len(content) == 0 || bytes.Equal(bytes.TrimSpace(content), []byte("null")) {
		return nil, nil
	}
	var text string
	if json.Unmarshal(content, &text) == nil {
		if text == "" {
			return nil, nil
		}
		return []object{{"type": "text", "text": text}}, nil
	}
	var parts []struct {
		Type  string `json:"type"`
		Text  string `json:"text"`
		Image struct {
			URL string `json:"url"`
		} `json:"image_url"`
	}
	if json.Unmarshal(content, &parts) != nil {
		return nil, errors.New("content must be text or an array of text and image parts")
	}
	var blocks []object
	for _, part := range parts {
		switch part.Type {
		case "text":
			if part.Text != "" {
				blocks = append(blocks, object{"type": "text", "text": part.Text})
			}
		case "image_url":
			source := object{"type": "url", "url": part.Image.URL}
			if data, ok := strings.CutPrefix(part.Image.URL, "data:"); ok {
				media, encoded, valid := strings.Cut(data, ";base64,")
				if !valid || !strings.HasPrefix(media, "image/") || encoded == "" {
					return nil, errors.New("invalid image data URL")
				}
				source = object{"type": "base64", "media_type": media, "data": encoded}
			} else if !strings.HasPrefix(part.Image.URL, "https://") && !strings.HasPrefix(part.Image.URL, "http://") {
				return nil, errors.New("image_url must be an HTTP URL or image data URL")
			}
			blocks = append(blocks, object{"type": "image", "source": source})
		default:
			return nil, fmt.Errorf("unsupported content type %q", part.Type)
		}
	}
	return blocks, nil
}

func (chat chatRequest) translateTools(out object) error {
	if len(chat.Tools) == 0 {
		return nil
	}
	tools := make([]object, 0, len(chat.Tools))
	for _, tool := range chat.Tools {
		if tool.Type != "function" || tool.Function.Name == "" {
			return errors.New("each tool must have type function and a name")
		}
		schema := tool.Function.Parameters
		if len(schema) == 0 || string(schema) == "null" {
			schema = json.RawMessage(`{"type":"object","properties":{}}`)
		}
		if schema[0] != '{' {
			return errors.New("function parameters must be a JSON schema object")
		}
		t := object{"name": tool.Function.Name, "input_schema": schema}
		if tool.Function.Description != "" {
			t["description"] = tool.Function.Description
		}
		tools = append(tools, t)
	}
	choice := object{"type": "auto"}
	if raw := chat.ToolChoice; len(raw) > 0 && string(raw) != "null" {
		var mode string
		if json.Unmarshal(raw, &mode) == nil {
			switch mode {
			case "none":
				choice["type"] = "none"
			case "auto":
			case "required":
				choice["type"] = "any"
			default:
				return errors.New("unsupported tool_choice")
			}
		} else {
			var selection chatTool
			if json.Unmarshal(raw, &selection) != nil || selection.Type != "function" || selection.Function.Name == "" {
				return errors.New("invalid tool_choice")
			}
			choice = object{"type": "tool", "name": selection.Function.Name}
		}
	}
	if chat.ParallelToolCalls != nil && !*chat.ParallelToolCalls {
		choice["disable_parallel_tool_use"] = true
	}
	out["tools"], out["tool_choice"] = tools, choice
	return nil
}

type claudeBlock struct {
	Type     string          `json:"type"`
	Text     string          `json:"text"`
	Thinking string          `json:"thinking"`
	ID       string          `json:"id"`
	Name     string          `json:"name"`
	Input    json.RawMessage `json:"input"`
}

type claudeMessage struct {
	ID         string          `json:"id"`
	Type       string          `json:"type"`
	Content    []claudeBlock   `json:"content"`
	StopReason string          `json:"stop_reason"`
	Usage      claudeUsage     `json:"usage"`
	Error      apiError        `json:"error"`
	Code       json.RawMessage `json:"code"`
	Msg        string          `json:"msg"`
}

type claudeUsage struct {
	Input         *int `json:"input_tokens"`
	Output        *int `json:"output_tokens"`
	CacheRead     *int `json:"cache_read_input_tokens"`
	CacheCreation *int `json:"cache_creation_input_tokens"`
}

func (u *claudeUsage) update(delta claudeUsage) {
	for _, field := range []struct {
		dst **int
		src *int
	}{
		{&u.Input, delta.Input},
		{&u.Output, delta.Output},
		{&u.CacheRead, delta.CacheRead},
		{&u.CacheCreation, delta.CacheCreation},
	} {
		if field.src != nil {
			*field.dst = field.src
		}
	}
}

func (u claudeUsage) openAI() object {
	total := func(values ...*int) int {
		sum := 0
		for _, value := range values {
			if value != nil {
				sum += *value
			}
		}
		return sum
	}
	in, out := total(u.Input, u.CacheRead, u.CacheCreation), total(u.Output)
	data := object{"prompt_tokens": in, "completion_tokens": out, "total_tokens": in + out}
	if u.CacheRead != nil {
		data["prompt_tokens_details"] = object{"cached_tokens": *u.CacheRead}
	}
	return data
}

func finishReason(reason string) string {
	switch reason {
	case "max_tokens", "model_context_window_exceeded":
		return "length"
	case "tool_use":
		return "tool_calls"
	case "refusal":
		return "content_filter"
	default:
		return "stop"
	}
}

func (m claudeMessage) completion(model string) object {
	var text, reasoning strings.Builder
	var calls []object
	for _, block := range m.Content {
		switch block.Type {
		case "text":
			text.WriteString(block.Text)
		case "thinking":
			reasoning.WriteString(block.Thinking)
		case "tool_use":
			calls = append(calls, object{
				"id": block.ID, "type": "function",
				"function": object{"name": block.Name, "arguments": string(block.Input)},
			})
		}
	}
	message := object{"role": "assistant", "content": text.String()}
	if reasoning.Len() > 0 {
		message["reasoning_content"] = reasoning.String()
	}
	if len(calls) > 0 {
		message["tool_calls"] = calls
		if text.Len() == 0 {
			message["content"] = nil
		}
	}
	return object{
		"id": m.ID, "object": "chat.completion", "created": time.Now().Unix(), "model": model,
		"choices": []object{{"index": 0, "message": message, "finish_reason": finishReason(m.StopReason)}},
		"usage":   m.Usage.openAI(),
	}
}
