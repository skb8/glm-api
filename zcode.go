package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

const (
	zcodeBase    = "https://zcode.z.ai/api/v1"
	zaiBase      = "https://api.z.ai"
	zcodeVersion = "3.18.2"
)

var (
	catalog          = []string{"GLM-5.3", "GLM-5.3-Flash", "GLM-5.2", "GLM-5-Turbo"}
	errLoginRequired = errors.New("сессия Z.AI отклонена, запустите glm-api -login")
)

type zcode struct {
	http         *http.Client
	oauthBase    string
	apiBase      string
	accountToken string

	mu       sync.Mutex
	business credential
	project  credential
	keyPath  string
}

type credential struct {
	token  string
	expiry time.Time
}

func (c credential) usable() bool {
	remaining := time.Until(c.expiry)
	return c.token != "" && remaining > 0
}

type sessionTokens struct {
	Access  string
	Refresh string
	Session string
}

type upstreamError struct {
	status int
	code   string
}

func (e *upstreamError) Error() string {
	if e.code != "" {
		return fmt.Sprintf("код Z.AI %s (HTTP %d)", e.code, e.status)
	}
	return fmt.Sprintf("HTTP %d", e.status)
}

func (e *upstreamError) is(status int) bool {
	return e != nil && (e.status == status || e.code == fmt.Sprint(status))
}

func upstreamIs(err error, status int) bool {
	var upstream *upstreamError
	return errors.As(err, &upstream) && upstream.is(status)
}

func (z *zcode) getJSON(ctx context.Context, method, endpoint, auth string, data, out any) error {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()

	var body io.Reader
	if data != nil {
		payload, err := json.Marshal(data)
		if err != nil {
			return err
		}
		body = bytes.NewReader(payload)
	}
	req, err := http.NewRequestWithContext(ctx, method, endpoint, body)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Content-Type", "application/json")
	if auth != "" {
		req.Header.Set("Authorization", auth)
	}
	res, err := z.http.Do(req)
	if err != nil {
		return errors.New("ошибка соединения с Z.AI")
	}
	defer res.Body.Close()
	if res.StatusCode < 200 || res.StatusCode >= 300 {
		return &upstreamError{status: res.StatusCode}
	}
	var envelope struct {
		Code json.RawMessage `json:"code"`
		Data json.RawMessage `json:"data"`
	}
	if json.NewDecoder(io.LimitReader(res.Body, 2<<20)).Decode(&envelope) != nil {
		return errors.New("некорректный JSON от Z.AI")
	}
	switch strings.Trim(string(envelope.Code), "\"") {
	case "0", "200":
	case "", "null":
		// The standalone business login may omit the code.
		if endpoint != z.apiBase+"/api/auth/z/login" {
			return errors.New("в ответе Z.AI отсутствует код")
		}
	default:
		return &upstreamError{status: res.StatusCode, code: safeCode(envelope.Code)}
	}
	if json.Unmarshal(envelope.Data, out) != nil || string(envelope.Data) == "null" {
		return errors.New("в ответе Z.AI отсутствуют ожидаемые данные")
	}
	return nil
}

func safeCode(raw json.RawMessage) string {
	code := strings.Trim(string(raw), "\"")
	if len(code) <= 10 && strings.Trim(code, "0123456789") == "" {
		return code
	}
	return "unknown"
}

func (z *zcode) login(ctx context.Context, showURL func(string)) (sessionTokens, error) {
	var tokens sessionTokens
	secret, err := randomHex()
	if err != nil {
		return tokens, err
	}
	auth := "Bearer " + secret
	var init struct {
		URL      string `json:"authorize_url"`
		Flow     string `json:"flow_id"`
		Expires  int64  `json:"expires_at"`
		Interval int    `json:"poll_interval_sec"`
	}
	if err := z.getJSON(ctx, "POST", z.oauthBase+"/oauth/cli/init", auth, map[string]string{"provider": "zai"}, &init); err != nil {
		return tokens, fmt.Errorf("запуск входа ZCode: %w", err)
	}
	link, err := url.Parse(init.URL)
	if err != nil || link.Scheme != "https" || link.Hostname() == "" || init.Flow == "" || init.Interval < 1 {
		return tokens, errors.New("некорректный ответ инициализации входа ZCode")
	}
	ctx, cancel := context.WithDeadline(ctx, time.Unix(init.Expires, 0))
	defer cancel()
	if ctx.Err() != nil {
		return tokens, errors.New("ссылка для входа ZCode истекла")
	}
	showURL(link.String())

	endpoint := z.oauthBase + "/oauth/cli/poll/" + url.PathEscape(init.Flow)
	for {
		var poll struct {
			Status string `json:"status"`
			Token  string `json:"token"`
			ZAI    struct {
				Access       string `json:"access_token"`
				AccessCamel  string `json:"accessToken"`
				Refresh      string `json:"refresh_token"`
				RefreshCamel string `json:"refreshToken"`
			} `json:"zai"`
		}
		err := z.getJSON(ctx, "GET", endpoint, auth, nil, &poll)
		switch {
		case err != nil:
			var remote *upstreamError
			if errors.As(err, &remote) && remote.status < 500 && remote.status != 408 && remote.status != 429 {
				return tokens, fmt.Errorf("вход ZCode: %w", err)
			}
		case poll.Status == "ready":
			tokens = sessionTokens{
				Session: firstToken(poll.Token),
				Access:  firstToken(poll.ZAI.Access, poll.ZAI.AccessCamel),
				Refresh: firstToken(poll.ZAI.Refresh, poll.ZAI.RefreshCamel),
			}
			if tokens.Session == "" || tokens.Access == "" {
				return sessionTokens{}, errors.New("ZCode не вернул токены сессии")
			}
			return tokens, nil
		case poll.Status != "pending":
			return tokens, errors.New("вход ZCode отклонён")
		}
		timer := time.NewTimer(time.Duration(init.Interval) * time.Second)
		select {
		case <-ctx.Done():
			timer.Stop()
			return tokens, fmt.Errorf("ожидание входа ZCode: %w", ctx.Err())
		case <-timer.C:
		}
	}
}

func firstToken(values ...string) string {
	for _, value := range values {
		if value = strings.TrimSpace(value); value != "" && !strings.ContainsAny(value, " \t\r\n\"'") {
			return value
		}
	}
	return ""
}

func jwtExpiry(token string) time.Time {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return time.Time{}
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return time.Time{}
	}
	var claims struct {
		Exp int64 `json:"exp"`
	}
	if json.Unmarshal(payload, &claims) != nil || claims.Exp <= 0 {
		return time.Time{}
	}
	return time.Unix(claims.Exp, 0)
}

func (z *zcode) businessToken(ctx context.Context) (string, error) {
	if z.business.usable() && time.Until(z.business.expiry) > 30*time.Second {
		return z.business.token, nil
	}
	if expires := jwtExpiry(z.accountToken); !expires.IsZero() && time.Now().After(expires) {
		return "", errLoginRequired
	}

	var data struct {
		Access      string `json:"access_token"`
		AccessCamel string `json:"accessToken"`
		TTLSnake    int64  `json:"expires_in"`
		TTLCamel    int64  `json:"expiresIn"`
	}
	start := time.Now()
	err := z.getJSON(ctx, "POST", z.apiBase+"/api/auth/z/login", "", map[string]string{"token": z.accountToken}, &data)
	if upstreamIs(err, 401) || upstreamIs(err, 403) {
		return "", errLoginRequired
	}
	if err != nil {
		return "", fmt.Errorf("обмен сессии Z.AI: %w", err)
	}
	token := firstToken(data.Access, data.AccessCamel)
	if token == "" {
		return "", errors.New("Z.AI не вернул токен для управления проектом")
	}
	expiry := jwtExpiry(token)
	for _, ttl := range []int64{data.TTLSnake, data.TTLCamel} {
		if ttl > 0 {
			candidate := start.Add(time.Duration(ttl) * time.Second)
			if expiry.IsZero() || candidate.Before(expiry) {
				expiry = candidate
			}
		}
	}
	if expiry.IsZero() {
		expiry = start.Add(5 * time.Minute)
	}
	if !expiry.After(time.Now()) {
		return "", errLoginRequired
	}
	z.business = credential{token: token, expiry: expiry}
	return token, nil
}

func defaultIndex[T any](values []T, name func(T) string, preferred string) int {
	for i, value := range values {
		if strings.Contains(name(value), preferred) {
			return i
		}
	}
	return 0
}

func (z *zcode) findKey(ctx context.Context, business string) (string, error) {
	type project struct {
		ID   string `json:"projectId"`
		Name string `json:"projectName"`
	}
	type organization struct {
		ID       string    `json:"organizationId"`
		Name     string    `json:"organizationName"`
		Projects []project `json:"projects"`
	}
	var customer struct {
		Organizations []organization `json:"organizations"`
	}
	if err := z.getJSON(ctx, "GET", z.apiBase+"/api/biz/customer/getCustomerInfo", business, nil, &customer); err != nil {
		return "", err
	}
	if len(customer.Organizations) == 0 {
		return "", errors.New("в аккаунте Z.AI нет организаций")
	}
	org := customer.Organizations[defaultIndex(customer.Organizations, func(o organization) string { return o.Name }, "默认机构")]
	if org.ID == "" || len(org.Projects) == 0 {
		return "", errors.New("в организации Z.AI нет проекта")
	}
	proj := org.Projects[defaultIndex(org.Projects, func(p project) string { return p.Name }, "默认项目")]
	if proj.ID == "" {
		return "", errors.New("Z.AI не вернул ID проекта")
	}
	path := z.apiBase + "/api/biz/v1/organization/" + url.PathEscape(org.ID) + "/projects/" + url.PathEscape(proj.ID) + "/api_keys"
	type key struct {
		ID   string `json:"apiKey"`
		Name string `json:"name"`
	}
	var keys []json.RawMessage
	auth := "Bearer " + business
	if err := z.getJSON(ctx, "GET", path, auth, nil, &keys); err != nil {
		return "", err
	}
	const name = "zcode-api-key"
	for _, raw := range keys {
		var entry key
		_ = json.Unmarshal(raw, &entry)
		if entry.Name == name {
			if strings.TrimSpace(entry.ID) == "" {
				return "", errors.New("у существующего ключа ZCode отсутствует ID")
			}
			return path + "/" + url.PathEscape(strings.TrimSpace(entry.ID)), nil
		}
	}
	var created key
	data := map[string]any{"name": name, "usageScene": 1}
	if err := z.getJSON(ctx, "POST", path, auth, data, &created); err != nil {
		return "", err
	}
	if strings.TrimSpace(created.ID) == "" {
		return "", errors.New("Z.AI не вернул ID нового ключа ZCode")
	}
	return path + "/" + url.PathEscape(strings.TrimSpace(created.ID)), nil
}

func (z *zcode) issueToken(ctx context.Context, business string) (credential, error) {
	if z.keyPath == "" {
		path, err := z.findKey(ctx, business)
		if err != nil {
			return credential{}, err
		}
		z.keyPath = path
	}
	var data struct {
		Enabled   *bool  `json:"enable"`
		Token     string `json:"accessToken"`
		TokenType string `json:"tokenType"`
		TTL       int64  `json:"expiresIn"`
		Expires   int64  `json:"expiresAt"`
	}
	start := time.Now()
	payload := map[string]string{"clientType": "zcode", "clientVersion": zcodeVersion}
	if err := z.getJSON(ctx, "POST", z.keyPath+"/access_tokens", "Bearer "+business, payload, &data); err != nil {
		if upstreamIs(err, 404) {
			z.keyPath = ""
		}
		return credential{}, err
	}
	if data.Enabled != nil && !*data.Enabled {
		return credential{}, errors.New("токены Coding Plan отключены для проекта Z.AI")
	}
	expires := time.Unix(data.Expires, 0)
	ttlExpiry := start.Add(time.Duration(data.TTL) * time.Second)
	cred := credential{token: firstToken(data.Token), expiry: expires}
	if ttlExpiry.Before(expires) {
		cred.expiry = ttlExpiry
	}
	if data.TokenType != "Bearer" || !cred.usable() {
		return credential{}, errors.New("Z.AI вернул недействительный токен проекта")
	}
	return cred, nil
}

func (z *zcode) projectToken(ctx context.Context) (string, error) {
	z.mu.Lock()
	defer z.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if z.project.usable() && time.Until(z.project.expiry) > 30*time.Second {
		return z.project.token, nil
	}

	for attempt := 0; attempt < 2; attempt++ {
		business, err := z.businessToken(ctx)
		if err != nil {
			return "", err
		}
		hadKey := z.keyPath != ""
		project, err := z.issueToken(ctx, business)
		if err == nil {
			z.project = project
			return project.token, nil
		}
		if attempt == 0 && upstreamIs(err, 401) {
			z.business = credential{}
			continue
		}
		if attempt == 0 && hadKey && z.keyPath == "" {
			continue
		}
		return "", fmt.Errorf("выпуск токена Coding Plan: %w", err)
	}
	return "", errors.New("Z.AI отклонил обмен токена проекта")
}

func (z *zcode) rejectProjectToken(token string) {
	z.mu.Lock()
	defer z.mu.Unlock()
	if z.project.token == token {
		z.project = credential{}
	}
}
