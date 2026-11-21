package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestOAuthAndModelProxy(t *testing.T) {
	const (
		business = "business-jwt"
		project  = "short-lived-project-token"
		key      = "local-only-key"
		keyPath  = "/api/biz/v1/organization/org/projects/proj/api_keys"
		gateway  = "/api/v1/ultra-zai/anthropic/v1/messages"
		count    = "/api/anthropic/v1/messages/count_tokens"
	)
	var pollToken string
	var issues atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth := r.Header.Get("Authorization")
		var data any
		switch r.Method + " " + r.URL.Path {
		case "POST /api/v1/oauth/cli/init":
			if !strings.HasPrefix(auth, "Bearer ") || len(auth) != len("Bearer ")+64 {
				t.Error("expected a random OAuth polling credential")
			}
			pollToken = auth
			data = object{"flow_id": "flow", "authorize_url": "https://zcode.z.ai/sign-in", "expires_at": time.Now().Add(time.Minute).Unix(), "poll_interval_sec": 1}
		case "GET /api/v1/oauth/cli/poll/flow":
			if auth != pollToken {
				t.Error("poll credential changed")
			}
			data = object{"status": "ready", "token": "session", "zai": object{"access_token": "account", "refresh_token": "refresh"}}
		case "POST /api/auth/z/login":
			var body map[string]string
			_ = json.NewDecoder(r.Body).Decode(&body)
			if auth != "" || body["token"] != "account" {
				t.Error("invalid business token exchange")
			}
			data = object{"accessToken": business, "expiresIn": 3600}
		case "GET /api/biz/customer/getCustomerInfo":
			if auth != business {
				t.Error("customer information requires raw business JWT")
			}
			data = object{"organizations": []object{
				{"organizationId": "other"},
				{"organizationId": "org", "organizationName": "默认机构", "projects": []object{
					{"projectId": "other"}, {"projectId": "proj", "projectName": "默认项目"},
				}},
			}}
		case "GET " + keyPath:
			if auth != "Bearer "+business {
				t.Error("key management requires bearer business JWT")
			}
			data = []object{{"name": "not-zcode"}}
		case "POST " + keyPath:
			var body struct {
				Name  string `json:"name"`
				Scene int    `json:"usageScene"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			if auth != "Bearer "+business || body.Name != "zcode-api-key" || body.Scene != 1 {
				t.Error("invalid Coding Plan key creation")
			}
			data = object{"apiKey": "key-id"}
		case "POST " + keyPath + "/key-id/access_tokens":
			issues.Add(1)
			if auth != "Bearer "+business {
				t.Error("project issuance requires bearer business JWT")
			}
			data = object{"enable": true, "accessToken": project, "tokenType": "Bearer", "expiresIn": 3600, "expiresAt": time.Now().Add(time.Hour).Unix()}
		case "POST " + gateway, "POST " + count:
			if auth != "Bearer "+project || r.Header.Get("X-Api-Key") != project || r.Header.Get("Anthropic-Version") != "2023-06-01" {
				t.Error("proxy did not supply upstream authentication")
			}
			if r.URL.Path == count {
				writeJSON(w, 200, object{"input_tokens": 2})
				return
			}
			var body struct {
				Model    string   `json:"model"`
				Stream   bool     `json:"stream"`
				System   []object `json:"system"`
				Messages []struct {
					Content json.RawMessage `json:"content"`
				} `json:"messages"`
			}
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Error(err)
				http.Error(w, "bad request", 400)
				return
			}
			if r.URL.Query().Has("beta") {
				if r.Header.Get("Anthropic-Beta") != "example-2026-01-01" || body.Model != "GLM-5.3" {
					t.Error("Anthropic query/header or body was not passed through")
				}
				writeJSON(w, 200, object{"id": "msg_native", "type": "message", "content": []object{{"type": "text", "text": "native"}}})
				return
			}
			if body.Model != "GLM-5.3" || len(body.System) != 1 || body.System[0]["text"] != "system prompt" {
				t.Error("chat model or system prompt was not translated")
			}
			if body.Stream {
				w.Header().Set("Content-Type", "text/event-stream")
				for _, line := range []string{
					`{"type":"message_start","message":{"id":"msg_stream","usage":{"input_tokens":2}}}`,
					`{"type":"content_block_start","index":2,"content_block":{"type":"tool_use","id":"call_1","name":"read","input":{}}}`,
					`{"type":"content_block_delta","index":2,"delta":{"type":"input_json_delta","partial_json":"{\"file\":\"a\"}"}}`,
					`{"type":"message_delta","delta":{"stop_reason":"tool_use"},"usage":{"output_tokens":3}}`,
					`{"type":"message_stop"}`,
				} {
					_, _ = io.WriteString(w, "data: "+line+"\n\n")
				}
				return
			}
			writeJSON(w, 200, object{"id": "msg_chat", "content": []object{{"type": "text", "text": "answer"}}, "stop_reason": "end_turn", "usage": object{"input_tokens": 2, "output_tokens": 3}})
			return
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
			http.NotFound(w, r)
			return
		}
		writeJSON(w, 200, object{"code": "0", "data": data})
	}))
	defer upstream.Close()

	z := &zcode{http: upstream.Client(), oauthBase: upstream.URL + "/api/v1", apiBase: upstream.URL}
	session, err := z.login(context.Background(), func(url string) {
		if url != "https://zcode.z.ai/sign-in" {
			t.Fatal("incorrect browser login URL")
		}
	})
	if err != nil || session != (sessionTokens{Access: "account", Refresh: "refresh", Session: "session"}) {
		t.Fatalf("OAuth session = %+v, %v", session, err)
	}
	z.accountToken = session.Access
	tok, err := z.projectToken(context.Background())
	if err != nil || tok != project {
		t.Fatalf("project token = %q, %v", tok, err)
	}
	s := &server{key: key, z: z, models: catalog}
	handler := s.routes()
	request := func(method, path, body string, headers map[string]string) *httptest.ResponseRecorder {
		t.Helper()
		rec := httptest.NewRecorder()
		r := httptest.NewRequest(method, path, strings.NewReader(body))
		for name, value := range headers {
			r.Header.Set(name, value)
		}
		handler.ServeHTTP(rec, r)
		return rec
	}
	if res := request("GET", "/v1/models", "", nil); res.Code != 401 {
		t.Fatalf("unauthorized models response = %d", res.Code)
	}
	bearer := map[string]string{"Authorization": "Bearer " + key}
	if res := request("GET", "/v1/models", "", bearer); res.Code != 200 || !strings.Contains(res.Body.String(), "GLM-5.3") {
		t.Fatalf("OpenAI models = %d %s", res.Code, res.Body)
	}
	anthropicAuth := map[string]string{"X-Api-Key": key, "Anthropic-Beta": "example-2026-01-01"}
	if res := request("GET", "/anthropic/v1/models", "", anthropicAuth); res.Code != 200 || !strings.Contains(res.Body.String(), "\"has_more\":false") {
		t.Fatalf("Anthropic models = %d %s", res.Code, res.Body)
	}
	res := request("POST", "/anthropic/v1/messages?beta=true", `{"model":"GLM-5.3","messages":[{"role":"user","content":"hi"}]}`, anthropicAuth)
	if res.Code != 200 || !strings.Contains(res.Body.String(), "msg_native") {
		t.Fatalf("Anthropic messages = %d %s", res.Code, res.Body)
	}
	res = request("POST", "/v1/messages/count_tokens", `{"model":"GLM-5.3","messages":[{"role":"user","content":"hi"}]}`, anthropicAuth)
	if res.Code != 200 || !strings.Contains(res.Body.String(), "\"input_tokens\":2") {
		t.Fatalf("Anthropic count = %d %s", res.Code, res.Body)
	}

	chat := `{"model":"glm-5.3","messages":[{"role":"developer","content":"system prompt"},{"role":"user","content":"hi"}]`
	res = request("POST", "/v1/chat/completions", chat+"}", bearer)
	if res.Code != 200 || !strings.Contains(res.Body.String(), "\"content\":\"answer\"") || !strings.Contains(res.Body.String(), "\"total_tokens\":5") {
		t.Fatalf("OpenAI completion = %d %s", res.Code, res.Body)
	}

	res = request("POST", "/v1/chat/completions", chat+`,"stream":true,"stream_options":{"include_usage":true}}`, bearer)
	for _, expected := range []string{`"name":"read"`, `"arguments":"{\"file\":\"a\"}"`, `"finish_reason":"tool_calls"`, `"total_tokens":5`, "data: [DONE]\n\n"} {
		if res.Code != 200 || !strings.Contains(res.Body.String(), expected) {
			t.Errorf("OpenAI stream missing %q: %d %s", expected, res.Code, res.Body)
		}
	}
	if issues.Load() != 1 {
		t.Fatalf("project token issued %d times for one session", issues.Load())
	}
	z.rejectProjectToken(project)
	if _, err := z.projectToken(context.Background()); err != nil || issues.Load() != 2 {
		t.Fatalf("project token after rejection issued %d times, error %v", issues.Load(), err)
	}
}

func TestEnvPersistsSecrets(t *testing.T) {
	path := filepath.Join(t.TempDir(), ".env")
	if err := os.WriteFile(path, []byte("# Keep this\nUNRELATED=yes\n"), 0644); err != nil {
		t.Fatal(err)
	}
	env, err := readEnv(path)
	if err != nil {
		t.Fatal(err)
	}
	env.set("GLM_API_KEY", "local")
	env.set("ZAI_ACCESS_TOKEN", "session")
	if err := env.save(); err != nil {
		t.Fatal(err)
	}
	env, err = readEnv(path)
	if err != nil || env.get("GLM_API_KEY") != "local" || env.get("ZAI_ACCESS_TOKEN") != "session" || env.get("UNRELATED") != "yes" {
		t.Fatalf("credentials file failed to round-trip: %v", err)
	}
	data, err := os.ReadFile(path)
	if err != nil || !bytes.HasPrefix(data, []byte("# Keep this\n")) {
		t.Fatalf("existing file content lost: %v", err)
	}
	stat, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS != "windows" && stat.Mode().Perm() != 0600 {
		t.Errorf("file mode = %o", stat.Mode().Perm())
	}
}
