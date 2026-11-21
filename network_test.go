package main

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) {
	return f(r)
}

func TestBusinessLoginRetriesTransientConnection(t *testing.T) {
	attempts := 0
	upstream := &zcode{
		apiBase:      "https://api.z.ai",
		accountToken: "test-account",
		http: &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			attempts++
			if r.URL.Hostname() != "api.z.ai" || r.URL.Path != "/api/auth/z/login" || r.Method != "POST" {
				t.Fatalf("unexpected exchange request: %s %s", r.Method, r.URL)
			}
			if attempts == 1 {
				return nil, io.ErrUnexpectedEOF
			}
			return &http.Response{
				StatusCode: 200,
				Body:       io.NopCloser(strings.NewReader(`{"code":200,"data":{"accessToken":"business","expiresIn":3600}}`)),
			}, nil
		})},
	}
	token, err := upstream.businessToken(context.Background())
	if err != nil || token != "business" || attempts != 2 {
		t.Fatalf("exchange: token=%q attempts=%d error=%v", token, attempts, err)
	}
}

func TestBusinessLoginRecognizesInvalidSession(t *testing.T) {
	upstream := &zcode{
		apiBase:      "https://api.z.ai",
		accountToken: "test-account",
		http: &http.Client{Transport: roundTripFunc(func(_ *http.Request) (*http.Response, error) {
			return &http.Response{
				StatusCode: 200,
				Body:       io.NopCloser(strings.NewReader(`{"code":500,"msg":"Z.ai user information is invalid","data":null,"success":false}`)),
			}, nil
		})},
	}
	if _, err := upstream.businessToken(context.Background()); !errors.Is(err, errLoginRequired) {
		t.Fatalf("exchange error: %v", err)
	}
}

func TestExplicitProxyReportsAuthenticationWithoutCredentials(t *testing.T) {
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "CONNECT" || r.Host != "api.z.ai:443" {
			t.Errorf("unexpected proxy request: %s %s", r.Method, r.Host)
		}
		w.WriteHeader(http.StatusProxyAuthRequired)
	}))
	defer proxy.Close()

	address, err := url.Parse(proxy.URL)
	if err != nil {
		t.Fatal(err)
	}
	address.User = url.UserPassword("proxy-user", "test-password")
	transport, err := upstreamTransport(address.String())
	if err != nil {
		t.Fatal(err)
	}
	defer transport.CloseIdleConnections()

	z := zcode{apiBase: "https://api.z.ai", http: &http.Client{Transport: transport}}
	err = z.getJSON(context.Background(), "GET", z.apiBase+"/test", "", nil, nil)
	if err == nil || err.Error() != "соединение с api.z.ai: прокси требует аутентификацию (HTTP 407)" {
		t.Fatalf("unexpected diagnostic: %v", err)
	}
}
