package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"crypto/tls"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"
)

const (
	probeModel        = "gpt-6-astra"
	responsesEndpoint = "https://chatgpt.com/backend-api/codex/responses"
	apiLimit          = 3 << 20
	answerLimit       = 32 << 10
)

type GateError struct{ Code, Text string }

func (e *GateError) Error() string      { return e.Text }
func gate(code, text string) *GateError { return &GateError{Code: code, Text: text} }

type LoginResult struct {
	JWT, Refresh, Temp string
	ExpiresIn          int
	Admin              bool
	UserID             int64
}
type AccountView struct {
	ID       int64  `json:"id"`
	Name     string `json:"name"`
	Status   string `json:"status"`
	Reason   string `json:"reason"`
	Eligible bool   `json:"eligible"`
	Used     *int   `json:"current_concurrency,omitempty"`
	Limit    *int   `json:"concurrency,omitempty"`
}
type ProbeAnswer struct {
	Text      string `json:"text"`
	Model     string `json:"model"`
	RequestID string `json:"request_id,omitempty"`
}

type Adapter struct {
	base              string
	admin             *http.Client
	upstream          string // fixed in production; isolated TLS server in tests
	upstreamTransport *http.Transport
}

func newTransport() *http.Transport {
	return &http.Transport{Proxy: nil, DialContext: (&net.Dialer{Timeout: 15 * time.Second, KeepAlive: 30 * time.Second}).DialContext, TLSClientConfig: &tls.Config{MinVersion: tls.VersionTLS12}, TLSHandshakeTimeout: 15 * time.Second, MaxResponseHeaderBytes: 48 << 10, MaxIdleConns: 4, IdleConnTimeout: 30 * time.Second, ForceAttemptHTTP2: true}
}
func rejectRedirect(_ *http.Request, _ []*http.Request) error { return http.ErrUseLastResponse }

// Accept either a site origin or an explicit /api/v1 URL. HTTP is local only.
func NewAdapter(base string) (*Adapter, error) {
	parsed, err := url.Parse(strings.TrimSpace(base))
	if err != nil || parsed.Hostname() == "" || parsed.User != nil || parsed.Opaque != "" || parsed.RawQuery != "" || parsed.ForceQuery || parsed.Fragment != "" || parsed.RawPath != "" {
		return nil, gate("CONFIG", "Sub2API 地址无效")
	}
	if parsed.Scheme != "https" && !(parsed.Scheme == "http" && (net.ParseIP(parsed.Hostname()).IsLoopback() || parsed.Hostname() == "localhost")) {
		return nil, gate("CONFIG", "Sub2API 需要 HTTPS 或本机回环地址")
	}
	path := strings.TrimSuffix(parsed.Path, "/")
	if path != "" && path != "/api/v1" {
		return nil, gate("CONFIG", "请使用 Sub2API 站点地址或 /api/v1")
	}
	parsed.Path = "/api/v1"
	return &Adapter{base: parsed.String(), admin: &http.Client{Transport: newTransport(), Timeout: 18 * time.Second, CheckRedirect: rejectRedirect}, upstream: responsesEndpoint, upstreamTransport: newTransport()}, nil
}

func httpFailure(ctx context.Context, err error, side string) error {
	if errors.Is(ctx.Err(), context.Canceled) {
		return gate("CANCELED", "任务已取消")
	}
	var timeout net.Error
	if errors.Is(ctx.Err(), context.DeadlineExceeded) || (errors.As(err, &timeout) && timeout.Timeout()) {
		return gate("TIMEOUT", side+"请求超时")
	}
	return gate("NETWORK", side+"连接失败，请检查服务、代理和证书")
}

// Translate only known reasons. Remote messages, URLs and body excerpts may carry secrets.
func adminFailure(status int, reason, path string) error {
	switch reason {
	case "STEP_UP_REQUIRED":
		return gate(reason, "本次读取需要 Sub2API 二次验证")
	case "STEP_UP_TOTP_NOT_ENABLED":
		return gate(reason, "请先在 Sub2API 完成敏感操作验证设置")
	case "STEP_UP_UNAVAILABLE":
		return gate(reason, "Sub2API 的二次验证暂不可用")
	case "STEP_UP_ADMIN_API_KEY_FORBIDDEN", "STEP_UP_MCP_OAUTH_FORBIDDEN", "STEP_UP_MACHINE_CREDENTIAL_FORBIDDEN":
		return gate("FORBIDDEN", "请使用管理员登录会话进行二次验证")
	case "CAPTCHA_REQUIRED", "CAPTCHA_INVALID", "CAPTCHA_VERIFICATION_FAILED", "TURNSTILE_VERIFICATION_FAILED":
		return gate("CAPTCHA_REQUIRED", "Sub2API 要求额外的人机验证，请使用受支持的正常登录方式")
	}
	if status == http.StatusBadRequest {
		switch {
		case strings.HasPrefix(path, "/admin/accounts/data?"):
			return gate("ADMIN_EXPORT_BAD_REQUEST", "Sub2API 在读取所选账号资料时返回 HTTP 400，尚未向模型发题")
		case strings.HasPrefix(path, "/admin/accounts/"):
			return gate("ADMIN_DETAIL_BAD_REQUEST", "Sub2API 在核对账号状态时返回 HTTP 400，尚未向模型发题")
		case strings.HasPrefix(path, "/admin/accounts?"):
			return gate("ADMIN_LIST_BAD_REQUEST", "Sub2API 在读取账号列表时返回 HTTP 400，请刷新列表")
		}
	}
	switch status {
	case 401:
		return gate("UNAUTHORIZED", "Sub2API 登录或验证失效")
	case 403:
		return gate("FORBIDDEN", "Sub2API 拒绝授权，请检查管理员权限和验证要求")
	case 404:
		return gate("NOT_FOUND", "Sub2API 未找到该账号或接口")
	case 429:
		return gate("ADMIN_LIMIT", "Sub2API 请求过于频繁，请稍后重试")
	default:
		return gate("ADMIN_ERROR", fmt.Sprintf("Sub2API 请求失败（HTTP %d）", status))
	}
}

func (a *Adapter) call(ctx context.Context, method, path, jwt string, payload, dst any) error {
	var body io.Reader
	if payload != nil {
		b, err := json.Marshal(payload)
		if err != nil {
			return gate("INVALID", "请求格式无效")
		}
		body = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, a.base+path, body)
	if err != nil {
		return gate("CONFIG", "Sub2API 地址无效")
	}
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if jwt != "" {
		req.Header.Set("Authorization", "Bearer "+jwt)
	}
	resp, err := a.admin.Do(req)
	if err != nil {
		return httpFailure(ctx, err, "Sub2API ")
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		// Handlers use numeric code + reason; auth middleware uses a string code.
		var details struct {
			Reason  string          `json:"reason"`
			Code    json.RawMessage `json:"code"`
			Message string          `json:"message"`
		}
		_ = json.NewDecoder(io.LimitReader(resp.Body, 16<<10)).Decode(&details)
		if details.Reason == "" {
			_ = json.Unmarshal(details.Code, &details.Reason)
		}
		if path == "/auth/refresh" || path == "/auth/me" {
			return sessionHTTPFailure(resp.StatusCode, details.Reason, string(details.Code), details.Message)
		}
		return adminFailure(resp.StatusCode, details.Reason, path)
	}
	// Decode into typed structs so refresh_token and arbitrary exported fields never enter application objects.
	var envelope struct {
		Code   *int   `json:"code"`
		Reason string `json:"reason"`
		Data   any    `json:"data"`
	}
	envelope.Data = dst
	lr := &io.LimitedReader{R: resp.Body, N: apiLimit + 1}
	dec := json.NewDecoder(lr)
	if err = dec.Decode(&envelope); err != nil {
		return gate("ADMIN_FORMAT", "Sub2API 返回的数据无法读取")
	}
	var extra json.RawMessage
	if err = dec.Decode(&extra); err != io.EOF || lr.N <= 0 || envelope.Code == nil {
		return gate("ADMIN_FORMAT", "Sub2API 响应格式或大小异常")
	}
	if *envelope.Code != 0 {
		return adminFailure(resp.StatusCode, envelope.Reason, path)
	}
	return nil
}

func (a *Adapter) login(ctx context.Context, path string, payload any) (LoginResult, error) {
	var r struct {
		Token   string `json:"access_token"`
		Refresh string `json:"refresh_token"`
		Temp    string `json:"temp_token"`
		Need2FA bool   `json:"requires_2fa"`
		Expires int    `json:"expires_in"`
		User    struct {
			ID   int64  `json:"id"`
			Role string `json:"role"`
		} `json:"user"`
	}
	if err := a.call(ctx, "POST", path, "", payload, &r); err != nil {
		return LoginResult{}, err
	}
	if r.Need2FA && r.Temp != "" {
		return LoginResult{Temp: r.Temp}, nil
	}
	if r.Token == "" || r.User.ID < 1 {
		return LoginResult{}, gate("LOGIN_FAILED", "登录响应缺少有效身份或令牌")
	}
	if r.User.Role != "admin" {
		if r.Refresh != "" {
			_ = a.Logout(ctx, r.Refresh)
		}
		return LoginResult{}, gate("FORBIDDEN", "仅 Sub2API 管理员可以登录检测器")
	}
	return LoginResult{JWT: r.Token, Refresh: r.Refresh, Admin: true, UserID: r.User.ID, ExpiresIn: r.Expires}, nil
}
func (a *Adapter) Login(ctx context.Context, email, password string) (LoginResult, error) {
	return a.login(ctx, "/auth/login", map[string]string{"email": email, "password": password})
}
func (a *Adapter) Login2FA(ctx context.Context, temp, code string) (LoginResult, error) {
	return a.login(ctx, "/auth/login/2fa", map[string]string{"temp_token": temp, "totp_code": code})
}

var (
	sessionInvalid   = gate("SESSION_INVALID", "Sub2API 登录已失效，请重新登录")
	accessExpired    = gate("ACCESS_EXPIRED", "Sub2API 访问令牌已到期，需要续期")
	sessionTemporary = gate("SESSION_TEMPORARY", "暂时无法核对 Sub2API 身份，请稍后重试")
)

func sessionHTTPFailure(status int, reason, rawCode, message string) error {
	// Only authoritative auth failures should discard the persistent login. A 401
	// page from a reverse proxy or a 5xx when Redis is offline proves nothing.
	if status == http.StatusUnauthorized || status == http.StatusForbidden || status == http.StatusNotFound {
		switch reason {
		case "TOKEN_EXPIRED", "ACCESS_TOKEN_EXPIRED":
			return accessExpired
		case "REFRESH_TOKEN_INVALID", "REFRESH_TOKEN_EXPIRED", "REFRESH_TOKEN_REUSED", "TOKEN_REVOKED", "INVALID_TOKEN", "USER_NOT_FOUND", "USER_INACTIVE", "USER_NOT_ACTIVE", "SESSION_BINDING_MISMATCH":
			return sessionInvalid
		}
		// v0.2.24 returns this ordinary envelope without a reason after a role downgrade.
		if status == http.StatusForbidden && rawCode == "403" && message == "Backend mode is active. Only admin login is allowed." {
			return sessionInvalid
		}
	}
	return sessionTemporary
}

func sessionFault(err error) error {
	if err == sessionInvalid || err == accessExpired {
		return err
	}
	return sessionTemporary
}

func panelToken(s string) bool {
	return len(s) > 0 && len(s) <= 32<<10 && !strings.Contains(s, " ") && headerValue.MatchString(s)
}

// Refresh rotates the Sub2API administrator session. The response has no identity.
// Save the pair before VerifyAdmin, so a temporary /auth/me failure can be retried.
func (a *Adapter) Refresh(ctx context.Context, refresh string) (LoginResult, error) {
	if !panelToken(refresh) {
		return LoginResult{}, sessionInvalid
	}
	var pair struct {
		JWT     string `json:"access_token"`
		Refresh string `json:"refresh_token"`
		Expires int    `json:"expires_in"`
		Type    string `json:"token_type"`
	}
	if err := a.call(ctx, http.MethodPost, "/auth/refresh", "", map[string]string{"refresh_token": refresh}, &pair); err != nil {
		return LoginResult{}, sessionFault(err)
	}
	if !panelToken(pair.JWT) || !panelToken(pair.Refresh) || pair.Refresh == refresh || pair.Expires <= 0 || int64(pair.Expires) > (1<<63-1)/int64(time.Second) || !strings.EqualFold(pair.Type, "Bearer") {
		return LoginResult{}, sessionTemporary
	}
	return LoginResult{JWT: pair.JWT, Refresh: pair.Refresh, ExpiresIn: pair.Expires}, nil
}

// VerifyAdmin gets the live, flat user profile for the access token.
func (a *Adapter) VerifyAdmin(ctx context.Context, jwt string, expectedUserID int64) error {
	if expectedUserID <= 0 || !panelToken(jwt) {
		return sessionInvalid
	}
	var me struct {
		ID     int64  `json:"id"`
		Role   string `json:"role"`
		Status string `json:"status"`
	}
	if err := a.call(ctx, http.MethodGet, "/auth/me", jwt, nil, &me); err != nil {
		return sessionFault(err)
	}
	if me.ID <= 0 || me.Role == "" || me.Status == "" {
		return sessionTemporary
	}
	if me.ID != expectedUserID || me.Role != "admin" || me.Status != "active" {
		return sessionInvalid
	}
	return nil
}

func (a *Adapter) StepUp(ctx context.Context, jwt, code string) error {
	var r struct {
		Verified bool `json:"verified"`
	}
	if err := a.call(ctx, "POST", "/user/totp/step-up", jwt, map[string]string{"code": code}, &r); err != nil {
		return err
	}
	if !r.Verified {
		return gate("STEP_UP_REQUIRED", "Sub2API 尚未确认二次验证")
	}
	return nil
}
func (a *Adapter) Logout(ctx context.Context, refresh string) error {
	if refresh == "" {
		return nil
	}
	var r struct{}
	return a.call(ctx, "POST", "/auth/logout", "", map[string]string{"refresh_token": refresh}, &r)
}

type routeProxy struct {
	ID       int64           `json:"id"`
	Key      string          `json:"proxy_key"`
	Protocol string          `json:"protocol"`
	Host     string          `json:"host"`
	Port     int             `json:"port"`
	Status   string          `json:"status"`
	Username string          `json:"username"`
	Password string          `json:"password"`
	Expires  json.RawMessage `json:"expires_at"`
}
type accountRules struct {
	CodexCLIOnly bool `json:"codex_cli_only"`
}
type credentialData struct {
	Token      string            `json:"access_token"`
	Expires    json.RawMessage   `json:"expires_at"`
	AccountID  string            `json:"chatgpt_account_id"`
	FedRAMP    json.RawMessage   `json:"chatgpt_account_is_fedramp"`
	UA         string            `json:"user_agent"`
	Mode       string            `json:"auth_mode"`
	LegacyMode string            `json:"openai_auth_mode"`
	Mapping    map[string]string `json:"model_mapping"`
	Override   bool              `json:"header_override_enabled"`
}
type accountData struct {
	ID          int64          `json:"id"`
	Name        string         `json:"name"`
	Platform    string         `json:"platform"`
	Type        string         `json:"type"`
	Status      string         `json:"status"`
	Schedulable *bool          `json:"schedulable"`
	Busy        *int           `json:"current_concurrency"`
	Limit       *int           `json:"concurrency"`
	Parent      *int64         `json:"parent_account_id"`
	Expires     *int64         `json:"expires_at"`
	Limited     *time.Time     `json:"rate_limit_reset_at"`
	Overload    *time.Time     `json:"overload_until"`
	TempPause   *time.Time     `json:"temp_unschedulable_until"`
	ProxyID     *int64         `json:"proxy_id"`
	Proxy       *routeProxy    `json:"proxy"`
	Cred        credentialData `json:"credentials"`
	Rules       accountRules   `json:"extra"`
}

func ordinaryOAuth(c credentialData) bool {
	for _, s := range []string{c.Mode, c.LegacyMode} {
		switch strings.ToLower(strings.TrimSpace(s)) {
		case "", "oauth", "chatgpt":
		default:
			return false
		}
	}
	return true
}

// Only the pre-export capacity check may return this sentinel. It is safe to retry.
var errAccountFull = gate("ACCOUNT_FULL", "账号并发已满，等待空位")

func (r accountData) eligibility(now time.Time) string {
	switch {
	case r.ID <= 0 || r.Platform != "openai" || r.Type != "oauth" || r.Parent != nil || !ordinaryOAuth(r.Cred):
		return "仅支持普通 OpenAI OAuth 账号"
	case r.Rules.CodexCLIOnly:
		return "账号设置为仅允许 Codex 官方客户端"
	case r.Status != "active":
		return "Sub2API 账号未处于正常状态"
	case r.Schedulable == nil || r.Busy == nil || *r.Busy < 0 || r.Limit == nil:
		return "Sub2API 运行状态字段不完整"
	case !*r.Schedulable:
		return "Sub2API 已暂停调度"
	case r.Expires != nil && *r.Expires <= now.Unix():
		return "Sub2API 账号已到期"
	case (r.Limited != nil && r.Limited.After(now)) || (r.Overload != nil && r.Overload.After(now)) || (r.TempPause != nil && r.TempPause.After(now)):
		return "Sub2API 标记账号限流或冷却中"
	}
	return ""
}
func (a *Adapter) List(ctx context.Context, jwt string) ([]AccountView, error) {
	accounts := []AccountView{}
	seen := map[int64]bool{}
	for page := 1; page <= 128; page++ {
		var r struct {
			Items []accountData `json:"items"`
			Pages int           `json:"pages"`
			Page  int           `json:"page"`
		}
		path := fmt.Sprintf("/admin/accounts?platform=openai&type=oauth&lite=1&page_size=32&page=%d", page)
		if err := a.call(ctx, "GET", path, jwt, nil, &r); err != nil {
			return nil, err
		}
		if r.Items == nil || r.Page != page || r.Pages < 1 || r.Pages > 128 {
			return nil, gate("ADMIN_FORMAT", "Sub2API 账号分页不完整或超出支持范围")
		}
		for _, row := range r.Items {
			if row.ID <= 0 || row.Name == "" || len(row.Name) > 512 || row.Platform != "openai" || row.Type != "oauth" || seen[row.ID] {
				return nil, gate("LIST_CHANGED", "账号列表已变化或格式不匹配，请刷新")
			}
			seen[row.ID] = true
			reason := row.eligibility(time.Now())
			accounts = append(accounts, AccountView{ID: row.ID, Name: row.Name, Status: row.Status, Reason: reason, Eligible: reason == "", Used: row.Busy, Limit: row.Limit})
		}
		if page >= r.Pages {
			return accounts, nil
		}
	}
	return nil, gate("ADMIN_FORMAT", "账号页数超出上限")
}

type exportData struct {
	Accounts []struct {
		Name     string         `json:"name"`
		Platform string         `json:"platform"`
		Type     string         `json:"type"`
		Cred     credentialData `json:"credentials"`
		Rules    accountRules   `json:"extra"`
		ProxyKey *string        `json:"proxy_key"`
	} `json:"accounts"`
	Proxies        []routeProxy `json:"proxies"`
	SkippedShadows int          `json:"skipped_shadows"`
}

func (r exportData) proxy(account accountData) (*url.URL, error) {
	mismatch := gate("PROXY_CONFIG", "账号的代理资料不完整或有变动，请在 Sub2API 检查后重试")
	key := r.Accounts[0].ProxyKey
	if account.ProxyID == nil {
		if key != nil || account.Proxy != nil || len(r.Proxies) != 0 {
			return nil, mismatch
		}
		return nil, nil
	}
	if account.Proxy == nil || *account.ProxyID <= 0 || account.Proxy.ID != *account.ProxyID || key == nil || *key == "" || len(r.Proxies) != 1 {
		return nil, mismatch
	}
	p := r.Proxies[0]
	d := account.Proxy
	if p.Key != *key || d.Host != p.Host || d.Port != p.Port || d.Protocol != p.Protocol || d.Username != p.Username || p.Status != "active" || d.Status != "active" {
		return nil, mismatch
	}
	for _, raw := range []json.RawMessage{d.Expires, p.Expires} {
		if len(raw) != 0 && string(raw) != "null" {
			until, ok := expiry(raw)
			if !ok || !time.Now().Before(until) {
				return nil, gate("PROXY_CONFIG", "代理有效期已过或无法确认，请在 Sub2API 核对")
			}
		}
	}
	switch p.Protocol {
	case "http", "https", "socks5", "socks5h":
	default:
		return nil, gate("PROXY_CONFIG", "当前代理协议不受支持")
	}
	if p.Port < 1 || p.Port > 65535 || p.Host == "" || strings.ContainsAny(p.Host, "/\\%@?# \r\n\t") {
		return nil, mismatch
	}
	if strings.Contains(p.Host, ":") && net.ParseIP(p.Host) == nil {
		return nil, mismatch
	}
	u := &url.URL{Scheme: p.Protocol, Host: net.JoinHostPort(p.Host, strconv.Itoa(p.Port))}
	if p.Username != "" || p.Password != "" {
		u.User = url.UserPassword(p.Username, p.Password)
	}
	return u, nil
}

func expiry(raw json.RawMessage) (time.Time, bool) {
	s := strings.TrimSpace(string(raw))
	if strings.HasPrefix(s, "\"") {
		if json.Unmarshal(raw, &s) != nil {
			return time.Time{}, false
		}
	}
	if i, err := strconv.ParseInt(s, 10, 64); err == nil {
		return time.Unix(i, 0), true
	}
	t, err := time.Parse(time.RFC3339, s)
	return t, err == nil
}

// JWT claims here are only additional consistency checks; upstream authenticates the token.
func bearerMetadata(token string) (account string, expires time.Time, ok bool) {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return "", time.Time{}, false
	}
	b, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return "", time.Time{}, false
	}
	var claims struct {
		Exp  json.RawMessage `json:"exp"`
		Auth struct {
			ID string `json:"chatgpt_account_id"`
		} `json:"https://api.openai.com/auth"`
	}
	if json.Unmarshal(b, &claims) != nil {
		return "", time.Time{}, false
	}
	t, valid := expiry(claims.Exp)
	return claims.Auth.ID, t, valid
}

func mappingAllowsAstra(m map[string]string) bool {
	if len(m) == 0 {
		return true
	}
	target, ok := m[probeModel]
	if !ok {
		longest := -1
		for key, v := range m {
			if strings.HasSuffix(key, "*") && strings.HasPrefix(probeModel, strings.TrimSuffix(key, "*")) && len(key) > longest {
				longest, target, ok = len(key), v, true
			}
		}
	}
	return ok && (target == probeModel || target == "gpt-6")
}

var headerValue = regexp.MustCompile(`^[\x20-\x7e]+$`)
var accountLabel = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.:-]{0,255}$`)
var clientUA = regexp.MustCompile(`^([\w.-]{1,64})/([0-9]+\.[0-9]+\.[0-9]+)(?:\s|$)`)
var modelLabel = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:/-]{0,127}$`)

func requestHeaders(detail accountData, cred credentialData, now time.Time) (http.Header, error) {
	invalid := gate("IDENTITY", "账号凭据或身份信息缺失／不一致")
	if !ordinaryOAuth(cred) || cred.Override {
		return nil, gate("UNSUPPORTED_ACCOUNT", "账号认证或请求头配置需要专用适配")
	}
	if !mappingAllowsAstra(cred.Mapping) {
		return nil, gate("MODEL_MAPPING", "该账号的模型映射未允许 Astra 原样发送")
	}
	if cred.Token == "" || len(cred.Token) > 48<<10 || strings.ContainsAny(cred.Token, " \t\r\n") || !headerValue.MatchString(cred.Token) {
		return nil, invalid
	}
	id, tokenExpiry, hasTokenExpiry := bearerMetadata(cred.Token)
	if id != "" && cred.AccountID != "" && id != cred.AccountID {
		return nil, invalid
	}
	if cred.AccountID != "" {
		id = cred.AccountID
	}
	if !accountLabel.MatchString(id) || detail.Cred.AccountID != "" && detail.Cred.AccountID != id {
		return nil, invalid
	}
	t, has := expiry(cred.Expires)
	if len(cred.Expires) > 0 && string(cred.Expires) != "null" && !has {
		return nil, gate("TOKEN_EXPIRY", "无法验证该账号令牌有效期")
	}
	if hasTokenExpiry && (!has || tokenExpiry.Before(t)) {
		t, has = tokenExpiry, true
	}
	if !has || !now.Before(t) {
		return nil, gate("UPSTREAM_TOKEN", "请等待 Sub2API 刷新凭据，或在原面板刷新后重试")
	}
	h := make(http.Header)
	h.Set("Authorization", "Bearer "+cred.Token)
	h.Set("ChatGPT-Account-Id", id)
	h.Set("Content-Type", "application/json")
	h.Set("Accept", "text/event-stream")
	h.Set("OpenAI-Beta", "responses=experimental")
	fed := strings.Trim(string(cred.FedRAMP), "\"")
	switch strings.ToLower(fed) {
	case "true", "1":
		h.Set("X-OpenAI-Fedramp", "true")
	case "", "false", "0", "null":
	default:
		return nil, invalid
	}
	// Use the account-configured identity when present. Label this integration in the default protocol UA.
	ua := cred.UA
	if ua == "" {
		ua = "sub2-fingerprint/" + strings.TrimPrefix(strings.TrimSpace(releaseVersion), "v")
	}
	m := clientUA.FindStringSubmatch(ua)
	if len(m) != 3 || len(ua) > 512 || !headerValue.MatchString(ua) {
		return nil, gate("IDENTITY", "账号客户端标识无法安全匹配")
	}
	h.Set("User-Agent", ua)
	h.Set("Originator", m[1])
	h.Set("Version", m[2])
	return h, nil
}

func (a *Adapter) Probe(ctx context.Context, jwt string, id int64, prompt string) (ProbeAnswer, error) {
	var empty ProbeAnswer
	if id <= 0 || len(prompt) == 0 || len(prompt) > 8192 {
		return empty, gate("INVALID", "账号或题目无效")
	}
	ctx, cancel := context.WithTimeout(ctx, 120*time.Second)
	defer cancel()
	rawID := strconv.FormatInt(id, 10)
	var row accountData
	if err := a.call(ctx, "GET", "/admin/accounts/"+rawID, jwt, nil, &row); err != nil {
		return empty, err
	}
	if row.ID != id || row.Name == "" {
		return empty, gate("ACCOUNT_MISMATCH", "所选账号与 Sub2API 返回值不一致")
	}
	if reason := row.eligibility(time.Now()); reason != "" {
		return empty, gate("ACCOUNT_UNAVAILABLE", reason)
	}
	if *row.Limit > 0 && *row.Busy >= *row.Limit {
		return empty, errAccountFull
	}
	var data exportData
	// A missing or empty ids parameter would export all accounts. Construct it from the verified positive ID only.
	if err := a.call(ctx, "GET", "/admin/accounts/data?ids="+rawID+"&include_proxies=true", jwt, nil, &data); err != nil {
		return empty, err
	}
	if len(data.Accounts) != 1 || data.SkippedShadows != 0 || data.Accounts[0].Name != row.Name || data.Accounts[0].Type != row.Type || data.Accounts[0].Platform != row.Platform {
		return empty, gate("ACCOUNT_MISMATCH", "所选账号资料已变化或导出条目不唯一")
	}
	if data.Accounts[0].Rules.CodexCLIOnly {
		return empty, gate("ACCOUNT_UNAVAILABLE", "账号设置为仅允许 Codex 官方客户端")
	}
	proxy, err := data.proxy(row)
	if err != nil {
		return empty, err
	}
	headers, err := requestHeaders(row, data.Accounts[0].Cred, time.Now())
	if err != nil {
		return empty, err
	}
	wire := map[string]any{
		"model": probeModel, "reasoning": map[string]string{"effort": "low"},
		"include":      []string{"reasoning.encrypted_content"},
		"instructions": "Answer the user’s numerical challenge directly. Do not use tools.",
		"input":        []any{map[string]any{"role": "user", "content": []any{map[string]string{"type": "input_text", "text": prompt}}}},
		"stream":       true, "store": false,
	}
	body, err := json.Marshal(wire)
	if err != nil {
		return empty, gate("INVALID", "题目无法编码")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, a.upstream, bytes.NewReader(body))
	if err != nil {
		return empty, gate("CONFIG", "上游地址无效")
	}
	req.Header = headers
	var nonce [16]byte
	if _, err = rand.Read(nonce[:]); err != nil {
		return empty, gate("RANDOM", "无法生成独立请求标识")
	}
	nonce[6] = (nonce[6] & 0x0f) | 0x40
	nonce[8] = (nonce[8] & 0x3f) | 0x80
	s := hex.EncodeToString(nonce[:])
	requestID := s[:8] + "-" + s[8:12] + "-" + s[12:16] + "-" + s[16:20] + "-" + s[20:]
	req.Header.Set("Session_id", requestID)
	transport := a.upstreamTransport.Clone()
	transport.DisableKeepAlives = true
	if proxy != nil {
		transport.Proxy = http.ProxyURL(proxy)
	}
	defer transport.CloseIdleConnections()
	client := http.Client{Transport: transport, CheckRedirect: rejectRedirect}
	resp, err := client.Do(req)
	if err != nil {
		return empty, httpFailure(ctx, err, "上游 ")
	}
	defer resp.Body.Close()
	switch resp.StatusCode {
	case 200:
	case http.StatusBadRequest:
		return empty, upstreamBadRequest(resp)
	case 401:
		return empty, gate("UPSTREAM_TOKEN", "上游拒绝账号凭据，请在 Sub2API 核对后重试")
	case 403:
		return empty, gate("UPSTREAM_FORBIDDEN", "上游拒绝当前账号或客户端请求")
	case 429:
		return empty, gate("UPSTREAM_RATE_LIMIT", "账号已被上游限流，请稍后重试")
	default:
		return empty, gate("UPSTREAM_HTTP", fmt.Sprintf("上游返回 HTTP %d", resp.StatusCode))
	}
	mediatype, _, _ := mime.ParseMediaType(resp.Header.Get("Content-Type"))
	if mediatype != "text/event-stream" {
		return empty, gate("UPSTREAM_FORMAT", "上游未返回预期的事件流")
	}
	result, err := readProbeStream(resp.Body)
	if err != nil {
		if ctx.Err() != nil {
			return empty, httpFailure(ctx, ctx.Err(), "上游 ")
		}
		return empty, err
	}
	result.RequestID = requestID
	return result, nil
}

// Provider messages sometimes echo input or credentials. Classify them locally and
// return only our own labels plus explicitly recognised protocol parameters.
type providerProblem struct {
	Code    string `json:"code"`
	Param   string `json:"param"`
	Message string `json:"message"`
}

func (p providerProblem) diagnostic(source string) *GateError {
	model := func() *GateError {
		return gate("UPSTREAM_MODEL", source+"指出模型不可用或模型参数有误，请核对该账号能否使用 "+probeModel)
	}
	effort := func() *GateError {
		return gate("UPSTREAM_REASONING", source+"指出推理参数有误，请核对该账号能否使用 low 档位")
	}
	client := func() *GateError {
		return gate("UPSTREAM_CLIENT", source+"拒绝当前客户端或版本，请核对该账号允许的客户端")
	}
	switch p.Param {
	case "reasoning.effort", "reasoning":
		return effort()
	case "model":
		return model()
	case "input", "instructions", "store", "stream", "tools", "include", "tool_choice":
		return gate("UPSTREAM_PARAMETER", source+"指出请求参数 "+p.Param+" 有误")
	}
	switch p.Code {
	case "model_not_found", "unsupported_model", "model_not_supported":
		return model()
	case "unsupported_client", "unsupported_client_version", "client_version_outdated":
		return client()
	case "context_length_exceeded":
		return gate("UPSTREAM_INPUT", source+"指出输入超过模型上下文限制")
	}
	// Some ChatGPT responses contain only a human-readable detail.
	m := strings.ToLower(p.Message)
	switch {
	case p.Code == "server_is_overloaded", p.Code == "slow_down", strings.Contains(m, "selected model is at capacity"):
		return gate("UPSTREAM_OVERLOAD", source+"报告服务过载，请稍后再测")
	case strings.Contains(m, "update your codex"), strings.Contains(m, "upgrade your codex"):
		return client()
	case strings.Contains(m, "reasoning effort") && (strings.Contains(m, "not supported") || strings.Contains(m, "does not support") || strings.Contains(m, "unsupported")):
		return effort()
	case strings.Contains(m, "model is not supported"), strings.Contains(m, "model is not available"), strings.Contains(m, "model not found"), strings.Contains(m, "does not exist or you do not have access"):
		return model()
	}
	return nil
}

var upstreamRequestRef = regexp.MustCompile(`^(?:req_[0-9A-Fa-f]{16,64}|[0-9A-Fa-f]{8}-(?:[0-9A-Fa-f]{4}-){3}[0-9A-Fa-f]{12})$`)

func upstreamBadRequest(resp *http.Response) error {
	source := "模型上游（HTTP 400）"
	if id := resp.Header.Get("X-Request-Id"); upstreamRequestRef.MatchString(id) {
		source = "模型上游（HTTP 400，请求编号 " + id + "）"
	}
	var message struct {
		providerProblem
		Error    providerProblem `json:"error"`
		Detail   json.RawMessage `json:"detail"`
		Response struct {
			Error providerProblem `json:"error"`
		} `json:"response"`
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, (16<<10)+1))
	if err == nil && len(body) <= 16<<10 && json.Unmarshal(body, &message) == nil {
		var detail providerProblem
		if json.Unmarshal(message.Detail, &detail) != nil {
			_ = json.Unmarshal(message.Detail, &detail.Message)
		}
		for _, problem := range []providerProblem{message.Error, message.Response.Error, detail, message.providerProblem} {
			if hint := problem.diagnostic(source); hint != nil {
				return hint
			}
		}
	}
	return gate("UPSTREAM_BAD_REQUEST", source+"拒绝本次请求，未给出可识别的具体原因")
}

type responseEvent struct {
	providerProblem                 // Some error events put code, param and message directly on the event.
	Type            string          `json:"type"`
	Delta           string          `json:"delta"`
	Error           providerProblem `json:"error"`
	Response        struct {
		Status string          `json:"status"`
		Model  string          `json:"model"`
		Error  providerProblem `json:"error"`
		Output []struct {
			Type    string `json:"type"`
			Role    string `json:"role"`
			Content []struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"content"`
		} `json:"output"`
	} `json:"response"`
}

func readProbeStream(reader io.Reader) (ProbeAnswer, error) {
	var empty ProbeAnswer
	incomplete := gate("UPSTREAM_INCOMPLETE", "上游响应未完整结束，本题未计分")
	refusal := gate("UPSTREAM_REFUSAL", "上游拒答或返回工具内容，本题未计分")
	large := gate("UPSTREAM_SIZE", "上游响应超过大小限制")
	limited := &io.LimitedReader{R: reader, N: (2 << 20) + 1}
	scanner := bufio.NewScanner(limited)
	scanner.Buffer(make([]byte, 4096), 128<<10)
	var delta, frame strings.Builder
	for scanner.Scan() {
		line := scanner.Text()
		if strings.HasPrefix(line, "data:") {
			fragment := strings.TrimPrefix(strings.TrimPrefix(line, "data:"), " ")
			if frame.Len()+len(fragment)+1 > 128<<10 {
				return empty, large
			}
			frame.WriteString(fragment)
			frame.WriteByte('\n')
			continue
		}
		if line != "" || frame.Len() == 0 {
			continue
		}
		raw := frame.String()
		frame.Reset()
		if strings.TrimSpace(raw) == "[DONE]" {
			return empty, incomplete
		}
		var ev responseEvent
		if json.Unmarshal([]byte(raw), &ev) != nil {
			return empty, incomplete
		}
		switch ev.Type {
		case "error", "response.failed":
			for _, problem := range []providerProblem{ev.providerProblem, ev.Error, ev.Response.Error} {
				if hint := problem.diagnostic("模型上游在响应流内"); hint != nil {
					return empty, hint
				}
			}
			return empty, gate("UPSTREAM_STREAM_ERROR", "模型上游在响应流内报告失败，未给出可识别的具体原因")
		case "response.incomplete", "response.cancelled", "response.canceled":
			return empty, incomplete
		case "response.refusal.delta", "response.refusal.done", "response.function_call_arguments.delta", "response.function_call_arguments.done":
			return empty, refusal
		case "response.output_text.delta":
			if delta.Len()+len(ev.Delta) > answerLimit {
				return empty, large
			}
			delta.WriteString(ev.Delta)
		case "response.completed":
			if limited.N <= 0 || ev.Response.Status != "completed" {
				return empty, incomplete
			}
			var text strings.Builder
			for _, item := range ev.Response.Output {
				if item.Type == "reasoning" {
					continue
				}
				if item.Type != "message" || item.Role != "assistant" {
					return empty, refusal
				}
				for _, part := range item.Content {
					if part.Type != "output_text" {
						return empty, refusal
					}
					if text.Len()+len(part.Text) > answerLimit {
						return empty, large
					}
					text.WriteString(part.Text)
				}
			}
			result := text.String()
			if result == "" {
				result = delta.String()
			}
			if strings.TrimSpace(result) == "" {
				return empty, incomplete
			}
			label := ev.Response.Model
			if label != "" && !modelLabel.MatchString(label) {
				return empty, incomplete
			}
			return ProbeAnswer{Text: result, Model: label}, nil
		}
	}
	return empty, incomplete
}
