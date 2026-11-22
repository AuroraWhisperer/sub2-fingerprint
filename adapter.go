package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	probeModel        = "gpt-6-astra"
	probeReasoning    = "low"
	probeInstruction  = "Answer the numerical challenge directly. Do not use tools."
	probeGroupDefault = "Probe"
	apiLimit          = 3 << 20
	answerLimit       = 32 << 10
	probeTimeout      = 180 * time.Second
	probeMaxAttempts  = 5
	adminTimeout      = 18 * time.Second
	cleanupTimeout    = 20 * time.Second
)

const probeBatchAdminCalls = 4

var (
	headerValue  = regexp.MustCompile("^[\\x20-\\x7e]+$")
	accountLabel = regexp.MustCompile("^[A-Za-z0-9][A-Za-z0-9_.:-]{0,255}$")
	modelLabel   = regexp.MustCompile("^[A-Za-z0-9][A-Za-z0-9._:/-]{0,127}$")
)

type Adapter struct {
	base           string
	gateway        string
	admin          *http.Client
	gatewayClient  *http.Client
	probeKey       string
	probeGroupName string
	requestTimeout time.Duration
	retryDelay     time.Duration
	groupMu        sync.Mutex
	recoveryMu     sync.RWMutex
	recoveryFailed bool
}

func newTransport() *http.Transport {
	return &http.Transport{
		Proxy:                  nil,
		DialContext:            (&net.Dialer{Timeout: 15 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
		TLSClientConfig:        &tls.Config{MinVersion: tls.VersionTLS12},
		TLSHandshakeTimeout:    15 * time.Second,
		MaxResponseHeaderBytes: 48 << 10,
		MaxIdleConns:           4,
		IdleConnTimeout:        30 * time.Second,
		ForceAttemptHTTP2:      true,
	}
}

func rejectRedirect(_ *http.Request, _ []*http.Request) error { return http.ErrUseLastResponse }

// NewAdapter creates an admin-only adapter for tests and non-probe operations.
// Production must use NewConfiguredAdapter so a Probe key and exact group name exist.
func NewAdapter(base string) (*Adapter, error) {
	return NewConfiguredAdapter(base, "", probeGroupDefault)
}

func NewConfiguredAdapter(base, probeKey, probeGroupName string) (*Adapter, error) {
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
	if probeKey != "" && !panelToken(probeKey) {
		return nil, gate("CONFIG", "Probe API Key 无效")
	}
	if probeGroupName == "" {
		probeGroupName = probeGroupDefault
	}
	if len(probeGroupName) > 128 || strings.TrimSpace(probeGroupName) != probeGroupName {
		return nil, gate("CONFIG", "Probe 分组名称无效")
	}
	gatewayURL := *parsed
	gatewayURL.Path = ""
	gatewayURL.RawPath = ""
	adminURL := *parsed
	adminURL.Path = "/api/v1"
	adminURL.RawPath = ""
	return &Adapter{
		base:           strings.TrimSuffix(adminURL.String(), "/"),
		gateway:        strings.TrimSuffix(gatewayURL.String(), "/"),
		admin:          &http.Client{Transport: newTransport(), Timeout: adminTimeout, CheckRedirect: rejectRedirect},
		gatewayClient:  &http.Client{Transport: newTransport(), Timeout: probeTimeout, CheckRedirect: rejectRedirect},
		probeKey:       probeKey,
		probeGroupName: probeGroupName,
		requestTimeout: probeTimeout,
		retryDelay:     time.Second,
	}, nil
}

func probeBatchTimeout(requestTimeout time.Duration, promptCount int, concurrency int) time.Duration {
	if requestTimeout <= 0 {
		requestTimeout = probeTimeout
	}
	if promptCount < 1 {
		promptCount = 1
	}
	if concurrency < 1 {
		concurrency = 1
	}
	if concurrency > promptCount {
		concurrency = promptCount
	}
	waves := (promptCount + concurrency - 1) / concurrency
	return time.Duration(waves)*requestTimeout + probeBatchAdminCalls*adminTimeout
}

func maxProbeBatchTimeout(promptCount int) time.Duration {
	return probeBatchTimeout(probeTimeout, promptCount, 1)
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

// CurrentAdmin gets the live, flat administrator profile for a browser access token.
// The detector deliberately does not accept or rotate the browser refresh token.
func (a *Adapter) CurrentAdmin(ctx context.Context, jwt string) (int64, error) {
	if !panelToken(jwt) {
		return 0, sessionInvalid
	}
	var me struct {
		ID     int64  `json:"id"`
		Role   string `json:"role"`
		Status string `json:"status"`
	}
	if err := a.call(ctx, http.MethodGet, "/auth/me", jwt, nil, &me); err != nil {
		return 0, sessionFault(err)
	}
	if me.ID <= 0 || me.Role == "" || me.Status == "" {
		return 0, sessionTemporary
	}
	if me.Role != "admin" || me.Status != "active" {
		return 0, sessionInvalid
	}
	return me.ID, nil
}

// VerifyAdmin gets the live, flat user profile for a stored detector session.
func (a *Adapter) VerifyAdmin(ctx context.Context, jwt string, expectedUserID int64) error {
	if expectedUserID <= 0 {
		return sessionInvalid
	}
	id, err := a.CurrentAdmin(ctx, jwt)
	if err != nil {
		return err
	}
	if id != expectedUserID {
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

type accountRules struct {
	CodexCLIOnly bool `json:"codex_cli_only"`
}

type accountData struct {
	ID          int64        `json:"id"`
	Name        string       `json:"name"`
	Platform    string       `json:"platform"`
	Type        string       `json:"type"`
	Status      string       `json:"status"`
	Schedulable *bool        `json:"schedulable"`
	Busy        *int         `json:"current_concurrency"`
	Limit       *int         `json:"concurrency"`
	Parent      *int64       `json:"parent_account_id"`
	Expires     *int64       `json:"expires_at"`
	Limited     *time.Time   `json:"rate_limit_reset_at"`
	Overload    *time.Time   `json:"overload_until"`
	TempPause   *time.Time   `json:"temp_unschedulable_until"`
	GroupIDs    []int64      `json:"group_ids"`
	Rules       accountRules `json:"extra"`
}

// Only the pre-request capacity check may return this sentinel. It is safe to retry.
var errAccountFull = gate("ACCOUNT_FULL", "账号并发已满，等待空位")

func (r accountData) eligibility(now time.Time) string {
	switch {
	case r.ID <= 0 || r.Platform != "openai" || r.Type != "oauth" || r.Parent != nil:
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

type groupData struct {
	ID   int64  `json:"id"`
	Name string `json:"name"`
}

type groupList []groupData

func (g *groupList) UnmarshalJSON(raw []byte) error {
	var list []groupData
	if err := json.Unmarshal(raw, &list); err == nil {
		*g = list
		return nil
	}
	var envelope struct {
		Items  []groupData `json:"items"`
		Groups []groupData `json:"groups"`
	}
	if err := json.Unmarshal(raw, &envelope); err != nil {
		return err
	}
	if envelope.Items != nil {
		*g = envelope.Items
	} else {
		*g = envelope.Groups
	}
	return nil
}

func containsGroup(groups []int64, id int64) bool {
	for _, group := range groups {
		if group == id {
			return true
		}
	}
	return false
}

func sameGroupSet(left, right []int64) bool {
	if len(left) != len(right) {
		return false
	}
	l := append([]int64(nil), left...)
	r := append([]int64(nil), right...)
	sort.Slice(l, func(i, j int) bool { return l[i] < l[j] })
	sort.Slice(r, func(i, j int) bool { return r[i] < r[j] })
	for i := range l {
		if l[i] != r[i] {
			return false
		}
	}
	return true
}

func copyGroups(groups []int64) []int64 { return append([]int64(nil), groups...) }

func (a *Adapter) findProbeGroup(ctx context.Context, jwt string) (int64, error) {
	var groups groupList
	if err := a.call(ctx, http.MethodGet, "/admin/groups/all", jwt, nil, &groups); err != nil {
		return 0, err
	}
	var found int64
	for _, group := range groups {
		if group.ID <= 0 || group.Name == "" {
			return 0, gate("GROUP_FORMAT", "Sub2API 分组列表格式不完整")
		}
		if group.Name == a.probeGroupName {
			if found != 0 {
				return 0, gate("GROUP_AMBIGUOUS", "Sub2API 中存在多个同名 Probe 分组")
			}
			found = group.ID
		}
	}
	if found == 0 {
		return 0, gate("GROUP_NOT_FOUND", "Sub2API 中未找到名为"+a.probeGroupName+"的分组")
	}
	return found, nil
}

func (a *Adapter) accountDetail(ctx context.Context, jwt string, id int64) (accountData, error) {
	var row accountData
	if err := a.call(ctx, http.MethodGet, "/admin/accounts/"+strconv.FormatInt(id, 10), jwt, nil, &row); err != nil {
		return accountData{}, err
	}
	if row.ID != id || row.Name == "" || row.GroupIDs == nil {
		return accountData{}, gate("ACCOUNT_MISMATCH", "所选账号资料缺少有效分组信息")
	}
	return row, nil
}

func (a *Adapter) updateAccountGroups(ctx context.Context, jwt string, id int64, groups []int64) error {
	return a.call(ctx, http.MethodPut, "/admin/accounts/"+strconv.FormatInt(id, 10), jwt, map[string]any{"group_ids": groups}, &struct{}{})
}

func (a *Adapter) markRecoveryFailed() {
	a.recoveryMu.Lock()
	a.recoveryFailed = true
	a.recoveryMu.Unlock()
}

func (a *Adapter) recoveryBlocked() bool {
	a.recoveryMu.RLock()
	defer a.recoveryMu.RUnlock()
	return a.recoveryFailed
}

func (a *Adapter) restoreGroups(ctx context.Context, jwt string, id int64, original []int64) error {
	if err := a.updateAccountGroups(ctx, jwt, id, original); err != nil {
		return err
	}
	row, err := a.accountDetail(ctx, jwt, id)
	if err != nil {
		return err
	}
	if !sameGroupSet(row.GroupIDs, original) {
		return gate("GROUP_RESTORE_FAILED", "账号原分组恢复后核对不一致")
	}
	return nil
}

func (a *Adapter) probeGroupTransaction(ctx context.Context, jwt string, id int64, fn func(context.Context) error) (err error) {
	if a.recoveryBlocked() {
		return gate("GROUP_RECOVERY_BLOCKED", "上一次账号分组恢复失败，已阻止后续分组变更")
	}
	if err := ctx.Err(); err != nil {
		return httpFailure(ctx, err, "检测 ")
	}
	a.groupMu.Lock()
	defer a.groupMu.Unlock()
	if a.recoveryBlocked() {
		return gate("GROUP_RECOVERY_BLOCKED", "上一次账号分组恢复失败，已阻止后续分组变更")
	}
	groupID, err := a.findProbeGroup(ctx, jwt)
	if err != nil {
		return err
	}
	row, err := a.accountDetail(ctx, jwt, id)
	if err != nil {
		return err
	}
	original := copyGroups(row.GroupIDs)
	if containsGroup(original, groupID) {
		return fn(ctx)
	}
	expected := append(copyGroups(original), groupID)
	restoreNeeded := true
	defer func() {
		if !restoreNeeded {
			return
		}
		cleanupCtx, cancel := context.WithTimeout(context.Background(), cleanupTimeout)
		defer cancel()
		if restoreErr := a.restoreGroups(cleanupCtx, jwt, id, original); restoreErr != nil {
			a.markRecoveryFailed()
			err = gate("GROUP_RESTORE_FAILED", "账号原分组恢复失败，已阻止后续分组变更")
		}
	}()
	if err := a.updateAccountGroups(ctx, jwt, id, expected); err != nil {
		return err
	}
	attached, err := a.accountDetail(ctx, jwt, id)
	if err != nil {
		return err
	}
	if !sameGroupSet(attached.GroupIDs, expected) || !containsGroup(attached.GroupIDs, groupID) {
		return gate("GROUP_ATTACH_FAILED", "账号加入 Probe 分组后核对不一致，未向模型发题")
	}
	return fn(ctx)
}

func (a *Adapter) groupTransaction(ctx context.Context, jwt string, id int64, fn func(context.Context) (ProbeAnswer, error)) (answer ProbeAnswer, err error) {
	err = a.probeGroupTransaction(ctx, jwt, id, func(ctx context.Context) error {
		answer, err = fn(ctx)
		return err
	})
	if err != nil {
		return ProbeAnswer{}, err
	}
	return answer, nil
}

type probeBatchReporter func(index int, answer ProbeAnswer, err error)

func (a *Adapter) ProbeBatch(ctx context.Context, jwt string, id int64, prompts []string, report probeBatchReporter) error {
	if id <= 0 || len(prompts) == 0 || len(prompts) > 3 {
		return gate("INVALID", "账号或题目无效")
	}
	for _, prompt := range prompts {
		if len(prompt) == 0 || len(prompt) > 8192 {
			return gate("INVALID", "账号或题目无效")
		}
	}
	if a.probeKey == "" {
		return gate("CONFIG", "检测器未配置 Probe API Key")
	}
	row, err := a.accountDetail(ctx, jwt, id)
	if err != nil {
		return err
	}
	if reason := row.eligibility(time.Now()); reason != "" {
		return gate("ACCOUNT_UNAVAILABLE", reason)
	}
	concurrency := len(prompts)
	if *row.Limit > 0 {
		available := *row.Limit - *row.Busy
		if available <= 0 {
			return errAccountFull
		}
		if available < concurrency {
			concurrency = available
		}
	}
	batchCtx, cancel := context.WithTimeout(ctx, probeBatchTimeout(a.requestTimeout, len(prompts), concurrency))
	defer cancel()
	return a.probeGroupTransaction(batchCtx, jwt, id, func(ctx context.Context) error {
		var workers sync.WaitGroup
		sem := make(chan struct{}, concurrency)
		for index, prompt := range prompts {
			workers.Add(1)
			go func(index int, prompt string) {
				defer workers.Done()
				select {
				case sem <- struct{}{}:
				case <-ctx.Done():
					if report != nil {
						report(index, ProbeAnswer{}, httpFailure(ctx, ctx.Err(), "检测 "))
					}
					return
				}
				requestCtx, requestCancel := context.WithTimeout(ctx, a.requestTimeout)
				answer, err := a.gatewayProbe(requestCtx, prompt)
				requestCancel()
				<-sem
				if report != nil {
					report(index, answer, err)
				}
			}(index, prompt)
		}
		workers.Wait()
		return nil
	})
}

func (a *Adapter) Probe(ctx context.Context, jwt string, id int64, prompt string) (ProbeAnswer, error) {
	var empty ProbeAnswer
	if id <= 0 || len(prompt) == 0 || len(prompt) > 8192 {
		return empty, gate("INVALID", "账号或题目无效")
	}
	if a.probeKey == "" {
		return empty, gate("CONFIG", "检测器未配置 Probe API Key")
	}
	ctx, cancel := context.WithTimeout(ctx, a.requestTimeout)
	defer cancel()
	row, err := a.accountDetail(ctx, jwt, id)
	if err != nil {
		return empty, err
	}
	if reason := row.eligibility(time.Now()); reason != "" {
		return empty, gate("ACCOUNT_UNAVAILABLE", reason)
	}
	if *row.Limit > 0 && *row.Busy >= *row.Limit {
		return empty, errAccountFull
	}
	return a.groupTransaction(ctx, jwt, id, func(ctx context.Context) (ProbeAnswer, error) {
		return a.gatewayProbe(ctx, prompt)
	})
}

func (a *Adapter) gatewayProbe(ctx context.Context, prompt string) (ProbeAnswer, error) {
	delay := a.retryDelay
	for attempt := 1; ; attempt++ {
		if err := ctx.Err(); err != nil {
			return ProbeAnswer{}, httpFailure(ctx, err, "模型网关 ")
		}
		answer, err := a.gatewayProbeOnce(ctx, prompt)
		var ge *GateError
		if attempt >= probeMaxAttempts || !errors.As(err, &ge) || ge.Code != "PROBE_NO_ACCOUNT" {
			return answer, err
		}
		// Admin group reads can precede the gateway scheduler's snapshot refresh.
		// Keep the account attached while retrying only rejected HTTP 503 requests.
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ProbeAnswer{}, httpFailure(ctx, ctx.Err(), "模型网关 ")
		case <-timer.C:
		}
		delay *= 2
	}
}

func (a *Adapter) gatewayProbeOnce(ctx context.Context, prompt string) (ProbeAnswer, error) {
	payload := map[string]any{
		"model":        probeModel,
		"reasoning":    map[string]string{"effort": probeReasoning},
		"include":      []string{"reasoning.encrypted_content"},
		"instructions": probeInstruction,
		"input": []any{map[string]any{
			"role":    "user",
			"content": []any{map[string]string{"type": "input_text", "text": prompt}},
		}},
		"stream": true,
		"store":  false,
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return ProbeAnswer{}, gate("INVALID", "请求格式无效")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, a.gateway+"/v1/responses", strings.NewReader(string(body)))
	if err != nil {
		return ProbeAnswer{}, gate("CONFIG", "Sub2API 地址无效")
	}
	req.Header.Set("Authorization", "Bearer "+a.probeKey)
	req.Header.Set("Accept", "text/event-stream")
	req.Header.Set("Content-Type", "application/json")
	resp, err := a.gatewayClient.Do(req)
	if err != nil {
		return ProbeAnswer{}, httpFailure(ctx, err, "模型网关 ")
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		switch resp.StatusCode {
		case http.StatusUnauthorized:
			return ProbeAnswer{}, gate("PROBE_KEY_INVALID", "Probe API Key 无效或已失效")
		case http.StatusForbidden:
			return ProbeAnswer{}, gate("PROBE_FORBIDDEN", "Probe 分组或 API Key 无权访问模型网关")
		case http.StatusTooManyRequests:
			return ProbeAnswer{}, gate("PROBE_LIMIT", "Probe 分组请求过于频繁，请稍后重试")
		case http.StatusServiceUnavailable:
			return ProbeAnswer{}, gate("PROBE_NO_ACCOUNT", "Probe 分组当前没有可用账号，模型请求返回 HTTP 503")
		case http.StatusBadRequest:
			return ProbeAnswer{}, upstreamBadRequest(resp)
		default:
			return ProbeAnswer{}, gate("UPSTREAM_HTTP", fmt.Sprintf("模型网关返回 HTTP %d", resp.StatusCode))
		}
	}
	answer, err := readProbeStream(resp.Body)
	if err != nil {
		return ProbeAnswer{}, err
	}
	if answer.RequestID == "" {
		for _, key := range []string{"X-Request-Id", "Request-Id"} {
			id := resp.Header.Get(key)
			if strings.HasPrefix(id, "req_") && len(id) <= 128 && headerValue.MatchString(id) {
				answer.RequestID = id
				break
			}
		}
	}
	return answer, nil
}

// Provider messages sometimes echo input or credentials. Classify them locally and
// return only our own labels plus explicitly recognised protocol parameters.
type providerProblem struct {
	Code    string `json:"code"`
	Param   string `json:"param"`
	Message string `json:"message"`
}

var (
	arraySubscript         = regexp.MustCompile(`\[[0-9]+\]`)
	parameterMessage       = regexp.MustCompile(`^(unsupported parameter|missing required parameter)(?::\s*|\s+)('[^'\r\n]+'|"[^"\r\n]+"|\x60[^\x60\r\n]+\x60|[^'"\x60\s]+)`)
	quotedModelUnavailable = regexp.MustCompile(`\bmodel ['"\x60][^'"\x60\r\n]+['"\x60] (?:is (?:not (?:supported|available|found)|unsupported)|does not exist|was not found)\b`)
)

func parameterValidation(source, field, kind string) *GateError {
	// Reveal only known field paths. Array positions and all values are discarded.
	field = arraySubscript.ReplaceAllString(field, "[]")
	switch field {
	case "model", "reasoning", "reasoning.effort", "input", "input[]", "input[].role", "input[].content", "input[].content[]", "input[].content[].type", "input[].content[].text",
		"instructions", "store", "stream", "tools", "include", "include[]", "tool_choice", "max_output_tokens", "temperature", "top_p":
	default:
		return nil
	}
	var reason string
	switch kind {
	case "missing_required_parameter", "missing required parameter", "missing", "value_error.missing", "field required":
		reason = "指出请求缺少必填参数 %s"
	case "unsupported_parameter", "unsupported parameter", "extra_forbidden", "value_error.extra", "extra inputs are not permitted":
		reason = "不支持请求参数 %s"
	case "string_type", "list_type", "dict_type", "bool_type", "bool_parsing", "type_error.str", "type_error.list", "type_error.dict", "type_error.bool":
		reason = "指出参数 %s 的类型有误"
	case "literal_error", "value_error.const":
		reason = "指出参数 %s 的取值有误"
	default:
		return nil
	}
	return gate("UPSTREAM_PARAMETER", source+fmt.Sprintf(reason, field))
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
	m := strings.ToLower(strings.TrimSpace(p.Message))
	if strings.Contains(m, "requires a newer version of codex") {
		return gate("UPSTREAM_CLIENT", source+"要求使用更新版本的 Codex 客户端")
	}
	if hint := parameterValidation(source, p.Param, p.Code); hint != nil {
		return hint
	}
	if match := parameterMessage.FindStringSubmatch(m); match != nil {
		field := strings.Trim(strings.TrimSuffix(match[2], "."), "'\"\x60")
		if p.Param == "" || p.Param == field {
			if hint := parameterValidation(source, field, match[1]); hint != nil {
				return hint
			}
		}
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
	switch {
	case p.Code == "server_is_overloaded", p.Code == "slow_down", strings.Contains(m, "selected model is at capacity"):
		return gate("UPSTREAM_OVERLOAD", source+"报告服务过载，请稍后再测")
	case strings.Contains(m, "update your codex"), strings.Contains(m, "upgrade your codex"):
		return client()
	case strings.Contains(m, "reasoning effort") && (strings.Contains(m, "not supported") || strings.Contains(m, "does not support") || strings.Contains(m, "unsupported")):
		return effort()
	case quotedModelUnavailable.MatchString(m), strings.Contains(m, "model is not supported"), strings.Contains(m, "model is not available"), strings.Contains(m, "model not found"), strings.Contains(m, "does not exist or you do not have access"):
		return model()
	}
	return nil
}

var upstreamRequestRef = regexp.MustCompile(`^(?:req_[0-9A-Fa-f]{16,64}|[0-9A-Fa-f]{8}-(?:[0-9A-Fa-f]{4}-){3}[0-9A-Fa-f]{12})$`)

func validationField(loc []any) string {
	var field strings.Builder
	for i, part := range loc {
		switch v := part.(type) {
		case string:
			if i == 0 && v == "body" {
				continue
			}
			if v == "" || strings.ContainsAny(v, ".[]") {
				return ""
			}
			if field.Len() > 0 {
				field.WriteByte('.')
			}
			field.WriteString(v)
		case float64:
			if v < 0 || v != float64(int(v)) {
				return ""
			}
			field.WriteString("[]")
		default:
			return ""
		}
	}
	return field.String()
}

// Validation details may include the full original input. Read only the location and category.
func validationListDiagnostic(source string, detail json.RawMessage) *GateError {
	var issues []struct {
		Loc       []any
		Type, Msg string
	}
	if json.Unmarshal(detail, &issues) != nil {
		return nil
	}
	for _, issue := range issues {
		field := validationField(issue.Loc)
		if hint := parameterValidation(source, field, issue.Type); hint != nil {
			return hint
		}
		if hint := parameterValidation(source, field, strings.ToLower(issue.Msg)); hint != nil {
			return hint
		}
	}
	return nil
}

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
		if hint := validationListDiagnostic(source, message.Detail); hint != nil {
			return hint
		}
	}
	return gate("UPSTREAM_BAD_REQUEST", source+"拒绝本次请求，未给出可识别的具体原因")
}

type responseItem struct {
	Type, Role, Phase string
	Content           []struct{ Type, Text string }
}

func (item responseItem) finalText() (string, error) {
	if item.Type == "reasoning" || item.Type == "message" && item.Role == "assistant" && item.Phase == "commentary" {
		return "", nil
	}
	if item.Type != "message" || item.Role != "assistant" || item.Phase != "" && item.Phase != "final_answer" {
		return "", gate("UPSTREAM_REFUSAL", "上游返回了非答案内容，本题未计分")
	}
	var text strings.Builder
	for _, part := range item.Content {
		if part.Type != "output_text" {
			return "", gate("UPSTREAM_REFUSAL", "上游拒答或返回工具内容，本题未计分")
		}
		if text.Len()+len(part.Text) > answerLimit {
			return "", gate("UPSTREAM_SIZE", "上游响应超过大小限制")
		}
		text.WriteString(part.Text)
	}
	return text.String(), nil
}

type responseEvent struct {
	providerProblem                 // Some error events put the code and message directly on the event.
	Type            string          `json:"type"`
	Delta           string          `json:"delta"`
	Item            responseItem    `json:"item"`
	Error           providerProblem `json:"error"`
	Response        struct {
		Status string          `json:"status"`
		Model  string          `json:"model"`
		Error  providerProblem `json:"error"`
		Output []responseItem  `json:"output"`
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
	var streamedAnswer string
	var sawItem bool
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
		case "response.output_item.added", "response.output_item.done":
			sawItem = true
			text, err := ev.Item.finalText()
			if err != nil {
				return empty, err
			}
			if ev.Type == "response.output_item.done" && text != "" {
				if streamedAnswer != "" {
					return empty, incomplete
				}
				streamedAnswer = text
			}
		case "response.output_text.delta":
			if delta.Len()+len(ev.Delta) > answerLimit {
				return empty, large
			}
			delta.WriteString(ev.Delta)
		case "response.completed":
			if limited.N <= 0 || ev.Response.Status != "completed" {
				return empty, incomplete
			}
			var result string
			for _, item := range ev.Response.Output {
				text, err := item.finalText()
				if err != nil {
					return empty, err
				}
				if text != "" {
					if result != "" {
						return empty, incomplete
					}
					result = text
				}
			}
			if streamedAnswer != "" {
				if result != "" && result != streamedAnswer {
					return empty, incomplete
				}
				result = streamedAnswer
			} else if result == "" && !sawItem && len(ev.Response.Output) == 0 {
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
