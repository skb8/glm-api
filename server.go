package main

import (
	"bytes"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

const maxRequestBody = 16 << 20

type server struct {
	z      *zcode
	key    string
	models []string
}

type apiError struct {
	Type    string `json:"type"`
	Message string `json:"message"`
}

func (s *server) routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/models", s.authorize(false, s.listModels))
	mux.HandleFunc("GET /v1/models/{model}", s.authorize(false, s.getModel))
	mux.HandleFunc("POST /v1/chat/completions", s.authorize(false, s.chat))
	mux.HandleFunc("GET /anthropic/v1/models", s.authorize(true, s.listAnthropicModels))
	mux.HandleFunc("GET /anthropic/v1/models/{model}", s.authorize(true, s.getAnthropicModel))
	for _, base := range []string{"/v1", "/anthropic/v1"} {
		mux.HandleFunc("POST "+base+"/messages", s.authorize(true, s.anthropic))
		mux.HandleFunc("POST "+base+"/messages/count_tokens", s.authorize(true, s.anthropic))
	}
	return mux
}

func (s *server) authorize(anthropic bool, handle http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		scheme, bearer, _ := strings.Cut(r.Header.Get("Authorization"), " ")
		if (strings.EqualFold(scheme, "Bearer") && subtle.ConstantTimeCompare([]byte(bearer), []byte(s.key)) == 1) ||
			(anthropic && subtle.ConstantTimeCompare([]byte(r.Header.Get("X-Api-Key")), []byte(s.key)) == 1) {
			handle(w, r)
			return
		}
		writeAPIError(w, anthropic, http.StatusUnauthorized, "authentication_error", "Invalid proxy API key")
	}
}

func (s *server) listModels(w http.ResponseWriter, r *http.Request) {
	models := make([]map[string]any, 0, len(s.models))
	for _, id := range s.models {
		models = append(models, openAIModel(id))
	}
	writeJSON(w, 200, map[string]any{"object": "list", "data": models})
}

func (s *server) getModel(w http.ResponseWriter, r *http.Request) {
	for _, id := range s.models {
		if strings.EqualFold(id, r.PathValue("model")) {
			writeJSON(w, 200, openAIModel(id))
			return
		}
	}
	writeAPIError(w, false, 404, "invalid_request_error", "Model not found")
}

func openAIModel(id string) map[string]any {
	return map[string]any{"id": id, "object": "model", "created": 0, "owned_by": "z.ai"}
}

func anthropicModel(id string) map[string]any {
	return map[string]any{"id": id, "type": "model", "display_name": id, "created_at": time.Unix(0, 0).UTC().Format(time.RFC3339)}
}

func (s *server) getAnthropicModel(w http.ResponseWriter, r *http.Request) {
	for _, id := range s.models {
		if strings.EqualFold(id, r.PathValue("model")) {
			writeJSON(w, 200, anthropicModel(id))
			return
		}
	}
	writeAPIError(w, true, 404, "not_found_error", "Model not found")
}

func (s *server) listAnthropicModels(w http.ResponseWriter, r *http.Request) {
	models := make([]map[string]any, 0, len(s.models))
	for _, id := range s.models {
		models = append(models, anthropicModel(id))
	}
	var first, last any
	if len(s.models) > 0 {
		first, last = s.models[0], s.models[len(s.models)-1]
	}
	writeJSON(w, 200, map[string]any{"data": models, "first_id": first, "last_id": last, "has_more": false})
}

func (s *server) anthropic(w http.ResponseWriter, r *http.Request) {
	body, ok := readBody(w, r, true)
	if !ok {
		return
	}
	endpoint := s.z.oauthBase + "/ultra-zai/anthropic/v1/messages"
	if strings.HasSuffix(r.URL.Path, "/count_tokens") {
		endpoint = s.z.apiBase + "/api/anthropic/v1/messages/count_tokens"
	}
	res, err := s.forward(r, endpoint, body)
	if err != nil {
		writeForwardError(w, true, err)
		return
	}
	defer res.Body.Close()
	if res.StatusCode >= 300 && res.StatusCode < 400 {
		writeAPIError(w, true, 502, "api_error", "Unexpected redirect from Z.AI")
		return
	}
	copyResponseHeaders(w, res)
	w.WriteHeader(res.StatusCode)
	if strings.HasPrefix(res.Header.Get("Content-Type"), "text/event-stream") {
		_ = http.NewResponseController(w).Flush()
		_, _ = io.Copy(flushWriter{w}, res.Body)
		return
	}
	_, _ = io.Copy(w, res.Body)
}

func (s *server) forward(r *http.Request, endpoint string, body []byte) (*http.Response, error) {
	token, err := s.z.projectToken(r.Context())
	if err != nil {
		return nil, err
	}
	if r.URL.RawQuery != "" {
		endpoint += "?" + r.URL.RawQuery
	}
	req, err := http.NewRequestWithContext(r.Context(), http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Anthropic-Version", "2023-06-01")
	for _, name := range []string{"Anthropic-Version", "Anthropic-Beta", "Accept"} {
		for _, value := range r.Header.Values(name) {
			if name == "Anthropic-Version" {
				req.Header.Set(name, value)
			} else {
				req.Header.Add(name, value)
			}
		}
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("X-Api-Key", token)

	res, err := s.z.http.Do(req)
	if err != nil {
		return nil, upstreamNetworkError(req.URL.Hostname(), err)
	}
	if res.StatusCode == http.StatusUnauthorized {
		s.z.rejectProjectToken(token)
	}
	return res, nil
}

func readBody(w http.ResponseWriter, r *http.Request, anthropic bool) ([]byte, bool) {
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxRequestBody))
	if err != nil {
		var limit *http.MaxBytesError
		if errors.As(err, &limit) {
			writeAPIError(w, anthropic, 413, "request_too_large", "Request exceeds 16 MiB")
		} else {
			writeAPIError(w, anthropic, 400, "invalid_request_error", "Cannot read request")
		}
		return nil, false
	}
	if !json.Valid(body) {
		writeAPIError(w, anthropic, 400, "invalid_request_error", "Invalid JSON body")
		return nil, false
	}
	return body, true
}

func writeForwardError(w http.ResponseWriter, anthropic bool, err error) {
	writeAPIError(w, anthropic, 502, "api_error", fmt.Sprintf("Z.AI: %s", err))
}

func writeAPIError(w http.ResponseWriter, anthropic bool, status int, kind, message string) {
	body := map[string]any{"error": apiError{Type: kind, Message: message}}
	if anthropic {
		body["type"] = "error"
	}
	writeJSON(w, status, body)
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

func copyResponseHeaders(w http.ResponseWriter, res *http.Response) {
	for name, values := range res.Header {
		name = http.CanonicalHeaderKey(name)
		switch {
		case name == "Content-Type", name == "Content-Encoding", name == "Request-Id", name == "X-Request-Id",
			name == "Cache-Control", name == "Retry-After", strings.HasPrefix(name, "Anthropic-Ratelimit-"):
			for _, value := range values {
				w.Header().Add(name, value)
			}
		}
	}
}

type flushWriter struct{ http.ResponseWriter }

func (w flushWriter) Write(data []byte) (int, error) {
	n, err := w.ResponseWriter.Write(data)
	if err != nil {
		return n, err
	}
	return n, http.NewResponseController(w.ResponseWriter).Flush()
}
