package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

type claudeEvent struct {
	Type    string        `json:"type"`
	Index   int           `json:"index"`
	Message claudeMessage `json:"message"`
	Block   claudeBlock   `json:"content_block"`
	Delta   struct {
		Type        string `json:"type"`
		Text        string `json:"text"`
		Thinking    string `json:"thinking"`
		PartialJSON string `json:"partial_json"`
		StopReason  string `json:"stop_reason"`
	} `json:"delta"`
	Usage claudeUsage `json:"usage"`
	Error apiError    `json:"error"`
}

type chatStream struct {
	dst          flushWriter
	model        string
	created      int64
	id           string
	tools        map[int]int
	usage        claudeUsage
	stopReason   string
	includeUsage bool
}

func streamChat(w http.ResponseWriter, input io.Reader, chat chatRequest) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Accel-Buffering", "no")
	stream := chatStream{
		dst: flushWriter{w}, model: chat.Model, created: time.Now().Unix(),
		tools: make(map[int]int), includeUsage: chat.StreamOptions.IncludeUsage,
	}

	scanner := bufio.NewScanner(input)
	scanner.Buffer(make([]byte, 8192), maxRequestBody)
	var lines []string
	var size int
	for scanner.Scan() {
		line := scanner.Text()
		if data, ok := strings.CutPrefix(line, "data:"); ok {
			data = strings.TrimPrefix(data, " ")
			size += len(data) + 1
			if size > maxRequestBody {
				stream.fail("Upstream event exceeds 16 MiB")
				return
			}
			lines = append(lines, data)
		}
		if line != "" || len(lines) == 0 {
			continue
		}
		var event claudeEvent
		err := json.Unmarshal([]byte(strings.Join(lines, "\n")), &event)
		lines, size = lines[:0], 0
		if err != nil {
			stream.fail("Invalid event from Z.AI")
			return
		}
		if event.Type == "error" {
			if event.Error.Message == "" {
				event.Error.Message = "Z.AI interrupted the stream"
			}
			if event.Error.Type == "" {
				event.Error.Type = "api_error"
			}
			_ = stream.send(object{"error": event.Error})
			return
		}
		if stream.writeEvent(event) != nil || event.Type == "message_stop" {
			return
		}
	}
	stream.fail("Z.AI closed the stream before message_stop")
}

func (s *chatStream) send(value object) error {
	data, err := json.Marshal(value)
	if err != nil {
		return err
	}
	_, err = fmt.Fprintf(s.dst, "data: %s\n\n", data)
	return err
}

func (s *chatStream) fail(message string) {
	_ = s.send(object{"error": apiError{Type: "api_error", Message: message}})
}

func (s *chatStream) chunk(choices []object) object {
	event := object{
		"id": s.id, "object": "chat.completion.chunk", "created": s.created, "model": s.model,
		"choices": choices,
	}
	if s.includeUsage {
		event["usage"] = nil
	}
	return event
}

func (s *chatStream) choice(delta object, finish any) error {
	return s.send(s.chunk([]object{{"index": 0, "delta": delta, "finish_reason": finish}}))
}

func (s *chatStream) writeEvent(event claudeEvent) error {
	switch event.Type {
	case "message_start":
		s.id = event.Message.ID
		s.usage.update(event.Message.Usage)
		return s.choice(object{"role": "assistant", "content": ""}, nil)
	case "content_block_start":
		block := event.Block
		switch block.Type {
		case "tool_use":
			index := len(s.tools)
			s.tools[event.Index] = index
			return s.choice(object{"tool_calls": []object{{
				"index": index, "id": block.ID, "type": "function",
				"function": object{"name": block.Name, "arguments": ""},
			}}}, nil)
		case "text":
			if block.Text != "" {
				return s.choice(object{"content": block.Text}, nil)
			}
		case "thinking":
			if block.Thinking != "" {
				return s.choice(object{"reasoning_content": block.Thinking}, nil)
			}
		}
	case "content_block_delta":
		delta := event.Delta
		switch delta.Type {
		case "text_delta":
			return s.choice(object{"content": delta.Text}, nil)
		case "thinking_delta":
			return s.choice(object{"reasoning_content": delta.Thinking}, nil)
		case "input_json_delta":
			if index, ok := s.tools[event.Index]; ok {
				return s.choice(object{"tool_calls": []object{{
					"index": index, "function": object{"arguments": delta.PartialJSON},
				}}}, nil)
			}
		}
	case "message_delta":
		s.usage.update(event.Usage)
		if event.Delta.StopReason != "" {
			s.stopReason = event.Delta.StopReason
		}
	case "message_stop":
		if err := s.choice(object{}, finishReason(s.stopReason)); err != nil {
			return err
		}
		if s.includeUsage {
			event := s.chunk([]object{})
			event["usage"] = s.usage.openAI()
			if err := s.send(event); err != nil {
				return err
			}
		}
		_, err := io.WriteString(s.dst, "data: [DONE]\n\n")
		return err
	}
	return nil
}
