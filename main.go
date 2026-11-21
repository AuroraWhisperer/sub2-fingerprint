package main

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"log"
	"math/big"
	"mime"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"
)

//go:embed web/* questions.json
var assets embed.FS

//go:embed VERSION
var releaseVersion string

type settings struct {
	Base     string `json:"sub2_url"`
	Parallel int    `json:"parallel"`
	Listen   string `json:"listen"`
	Data     string `json:"data_dir"`
}
type sub2 interface {
	Login(context.Context, string, string) (LoginResult, error)
	Login2FA(context.Context, string, string) (LoginResult, error)
	Logout(context.Context, string) error
	StepUp(context.Context, string, string) error
	List(context.Context, string) ([]AccountView, error)
	Probe(context.Context, string, int64, string) (ProbeAnswer, error)
}
type session struct {
	key, csrf, jwt, refresh, temp string
	owner                         int64
	end                           time.Time
	ctx                           context.Context
	cancel                        context.CancelFunc
}
type app struct {
	cfg         settings
	api         sub2
	queue       *queue
	bankVersion string
	groups      [][]Question
	questions   map[string]Question
	static      http.Handler
	mu          sync.Mutex
	sessions    map[string]*session
	loginWindow time.Time
	attempts    int
}

func newApp(cfg settings, api sub2) (*app, error) {
	a := &app{cfg: cfg, api: api, questions: map[string]Question{}, sessions: map[string]*session{}}
	host, port, e := net.SplitHostPort(cfg.Listen)
	if e != nil || host != "127.0.0.1" || port == "" || cfg.Parallel < 1 || cfg.Parallel > 2 || !filepath.IsAbs(cfg.Data) {
		return nil, errors.New("config requires IPv4 loopback listen, parallel 1 or 2, and an absolute private data directory")
	}
	contents, e := assets.ReadFile("questions.json")
	if e != nil {
		return nil, e
	}
	if e = json.Unmarshal(contents, &a.groups); e != nil || len(a.groups) == 0 {
		return nil, errors.New("invalid challenge catalogue")
	}
	for _, g := range a.groups {
		if len(g) != 3 {
			return nil, errors.New("invalid challenge group")
		}
		for _, q := range g {
			if q.ID == "" || a.questions[q.ID].ID != "" || q.Expected < 292 || q.Expected > 332 || q.Prompt == "" {
				return nil, errors.New("invalid challenge")
			}
			a.questions[q.ID] = q
		}
	}
	bank, e := assets.ReadFile("web/unified_bank.json")
	if e != nil {
		return nil, e
	}
	hash := sha256.Sum256(bank)
	a.bankVersion = hex.EncodeToString(hash[:])
	root, _ := fs.Sub(assets, "web")
	a.static = http.FileServer(http.FS(root))
	a.queue, e = openQueue(cfg.Data, a.bankVersion, api.Probe)
	return a, e
}
func respond(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}
func fail(w http.ResponseWriter, status int, code, text string) {
	respond(w, status, map[string]string{"error": text, "code": code})
}
func fault(w http.ResponseWriter, err error) {
	var e *GateError
	if errors.As(err, &e) {
		status := 400
		if strings.HasPrefix(e.Code, "STEP_UP") {
			status = 403
		}
		fail(w, status, e.Code, e.Text)
		return
	}
	fail(w, 502, "SUB2_UNAVAILABLE", "与 Sub2API 的通信未完成，请检查服务连接")
}
func decode(w http.ResponseWriter, r *http.Request, value any) bool {
	d := json.NewDecoder(http.MaxBytesReader(w, r.Body, 16<<10))
	d.DisallowUnknownFields()
	if err := d.Decode(value); err != nil || d.Decode(new(any)) != io.EOF {
		fail(w, 400, "INVALID_INPUT", "提交的内容无效")
		return false
	}
	return true
}
func exactMethod(w http.ResponseWriter, r *http.Request, method string) bool {
	if r.Method != method {
		w.Header().Set("Allow", method)
		fail(w, 405, "METHOD", "请求方法不支持")
		return false
	}
	return true
}
func (a *app) validHost(h string) bool {
	_, port, _ := net.SplitHostPort(a.cfg.Listen)
	return h == a.cfg.Listen || h == net.JoinHostPort("localhost", port)
}
func (a *app) lookup(r *http.Request) *session {
	c, err := r.Cookie("s2fp_session")
	if err != nil {
		return nil
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	s := a.sessions[c.Value]
	if s != nil && time.Now().Before(s.end) {
		return s
	}
	return nil
}
func (a *app) prune() {
	a.mu.Lock()
	defer a.mu.Unlock()
	for k, s := range a.sessions {
		if !time.Now().Before(s.end) {
			s.cancel()
			delete(a.sessions, k)
		}
	}
}
func (a *app) replace(w http.ResponseWriter, old *session, info LoginResult) (*session, bool) {
	a.prune()
	a.mu.Lock()
	defer a.mu.Unlock()
	if old != nil {
		if a.sessions[old.key] != old {
			fail(w, 409, "STALE_SESSION", "登录状态已变化，请刷新页面")
			return nil, false
		}
		delete(a.sessions, old.key)
		old.cancel()
	}
	if len(a.sessions) >= 48 {
		fail(w, 429, "SESSION_LIMIT", "本机打开的会话过多，请稍后重试")
		return nil, false
	}
	lifetime := 15 * time.Minute
	if info.Temp != "" {
		lifetime = 5 * time.Minute
	}
	if info.JWT != "" {
		lifetime = 30 * time.Minute
		if info.ExpiresIn > 0 && info.ExpiresIn < 1800 {
			lifetime = time.Duration(info.ExpiresIn) * time.Second
		}
	}
	end := time.Now().Add(lifetime)
	ctx, cancel := context.WithDeadline(context.Background(), end)
	s := &session{key: randomID(), csrf: randomID(), jwt: info.JWT, refresh: info.Refresh, temp: info.Temp, owner: info.UserID, end: end, ctx: ctx, cancel: cancel}
	a.sessions[s.key] = s
	http.SetCookie(w, &http.Cookie{Name: "s2fp_session", Value: s.key, HttpOnly: true, SameSite: http.SameSiteStrictMode, Path: "/", Expires: end})
	return s, true
}
func (a *app) allowAuth() bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	if time.Since(a.loginWindow) > time.Minute {
		a.loginWindow = time.Now()
		a.attempts = 0
	}
	a.attempts++
	return a.attempts <= 12
}
func (a *app) sessionView(s *session) map[string]any {
	return map[string]any{"logged_in": s.jwt != "", "needs_2fa": s.temp != "", "csrf": s.csrf, "parallel": a.cfg.Parallel, "bank_version": a.bankVersion, "version": strings.TrimSpace(releaseVersion)}
}
func (a *app) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Referrer-Policy", "no-referrer")
	w.Header().Set("X-Frame-Options", "DENY")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Security-Policy", "default-src 'self'; script-src 'self'; style-src 'self'; img-src 'self' data:; connect-src 'self'; frame-ancestors 'none'; base-uri 'none'; form-action 'self'; object-src 'none'")
	if !a.validHost(r.Host) {
		fail(w, 403, "HOST", "请从本机隧道地址打开")
		return
	}
	p := r.URL.Path
	if p == "/healthz" {
		if !exactMethod(w, r, "GET") {
			return
		}
		a.queue.mu.Lock()
		ok := !a.queue.storageFault && !a.queue.stopping
		a.queue.mu.Unlock()
		if !ok {
			fail(w, 503, "STORAGE", "记录存储不可用")
			return
		}
		respond(w, 200, map[string]string{"status": "ok", "version": strings.TrimSpace(releaseVersion)})
		return
	}
	if !strings.HasPrefix(p, "/api/") {
		if r.Method != "GET" && r.Method != "HEAD" {
			fail(w, 405, "METHOD", "请求方法不支持")
			return
		}
		if p != "/" && (strings.Contains(strings.TrimPrefix(p, "/"), "/") || strings.HasPrefix(p, "/.")) {
			http.NotFound(w, r)
			return
		}
		a.static.ServeHTTP(w, r)
		return
	}
	s := a.lookup(r)
	if r.Method == "POST" {
		mt, _, e := mime.ParseMediaType(r.Header.Get("Content-Type"))
		if e != nil || mt != "application/json" || r.Header.Get("Origin") != "http://"+r.Host || s == nil || subtle.ConstantTimeCompare([]byte(r.Header.Get("X-CSRF-Token")), []byte(s.csrf)) != 1 {
			fail(w, 403, "CSRF", "页面会话已过期或来源不符，请刷新")
			return
		}
	}
	if p == "/api/session" {
		if !exactMethod(w, r, "GET") {
			return
		}
		if s == nil {
			var ok bool
			s, ok = a.replace(w, nil, LoginResult{})
			if !ok {
				return
			}
		}
		respond(w, 200, a.sessionView(s))
		return
	}
	if s == nil {
		fail(w, 401, "SESSION", "请先登录")
		return
	}
	switch p {
	case "/api/login", "/api/login2fa", "/api/logout", "/api/stepup":
		a.auth(w, r, s)
		return
	}
	if s.jwt == "" || s.owner <= 0 {
		fail(w, 401, "SESSION", "请先登录 Sub2API 管理员账号")
		return
	}
	switch p {
	case "/api/accounts":
		if !exactMethod(w, r, "GET") {
			return
		}
		list, e := a.api.List(r.Context(), s.jwt)
		if e != nil {
			fault(w, e)
			return
		}
		respond(w, 200, map[string]any{"accounts": list})
	case "/api/questions":
		if !exactMethod(w, r, "GET") {
			return
		}
		i, e := rand.Int(rand.Reader, big.NewInt(int64(len(a.groups))))
		if e != nil {
			fail(w, 503, "RANDOM", "暂不能生成题目")
			return
		}
		respond(w, 200, map[string]any{"questions": a.groups[i.Int64()]})
	case "/api/runs":
		if r.Method == "GET" {
			respond(w, 200, map[string]any{"runs": a.queue.history(s.owner)})
			return
		}
		if exactMethod(w, r, "POST") {
			a.submit(w, r, s)
		}
	default:
		a.runDetail(w, r, s)
	}
}
func sixDigits(s string) bool {
	if len(s) != 6 {
		return false
	}
	for _, c := range s {
		if c < '0' || c > '9' {
			return false
		}
	}
	return true
}
func (a *app) auth(w http.ResponseWriter, r *http.Request, s *session) {
	if !exactMethod(w, r, "POST") {
		return
	}
	p := r.URL.Path
	if p == "/api/logout" {
		var empty struct{}
		if !decode(w, r, &empty) {
			return
		}
		newSession, ok := a.replace(w, s, LoginResult{})
		if !ok {
			return
		}
		if s.refresh != "" {
			if err := a.api.Logout(r.Context(), s.refresh); err != nil {
				log.Print("local logout completed; remote revocation unconfirmed")
			}
		}
		respond(w, 200, a.sessionView(newSession))
		return
	}
	if !a.allowAuth() {
		fail(w, 429, "RATE_LIMIT", "验证次数过多，请稍后再试")
		return
	}
	var info LoginResult
	var err error
	switch p {
	case "/api/stepup":
		if s.jwt == "" {
			fail(w, 401, "SESSION", "请先登录")
			return
		}
		var body struct {
			Code string `json:"code"`
		}
		if !decode(w, r, &body) {
			return
		}
		if !sixDigits(body.Code) {
			fail(w, 400, "INVALID_CODE", "请输入六位验证码")
			return
		}
		err = a.api.StepUp(r.Context(), s.jwt, body.Code)
		if err != nil {
			fault(w, err)
			return
		}
		respond(w, 200, a.sessionView(s))
		return
	case "/api/login2fa":
		if s.jwt != "" || s.temp == "" {
			fail(w, 400, "NO_CHALLENGE", "请重新登录后验证")
			return
		}
		var body struct {
			Code string `json:"code"`
		}
		if !decode(w, r, &body) {
			return
		}
		if !sixDigits(body.Code) {
			fail(w, 400, "INVALID_CODE", "请输入六位验证码")
			return
		}
		info, err = a.api.Login2FA(r.Context(), s.temp, body.Code)
	case "/api/login":
		if s.jwt != "" {
			fail(w, 409, "ALREADY_LOGGED_IN", "当前已经登录")
			return
		}
		var b struct {
			Email    string `json:"email"`
			Password string `json:"password"`
		}
		if !decode(w, r, &b) {
			return
		}
		b.Email = strings.TrimSpace(b.Email)
		if len(b.Email) > 320 || !strings.Contains(b.Email, "@") || b.Password == "" || len(b.Password) > 1024 {
			fail(w, 400, "LOGIN_INPUT", "请输入有效的邮箱和密码")
			return
		}
		info, err = a.api.Login(r.Context(), b.Email, b.Password)
	}
	if err != nil {
		log.Print("admin authentication unsuccessful")
		fault(w, err)
		return
	}
	if info.Temp == "" && (!info.Admin || info.UserID <= 0 || info.JWT == "") {
		if info.Refresh != "" {
			_ = a.api.Logout(r.Context(), info.Refresh)
		}
		fail(w, 403, "ADMIN_REQUIRED", "仅允许管理员登录")
		return
	}
	if info.Temp != "" {
		info = LoginResult{Temp: info.Temp}
	}
	st, ok := a.replace(w, s, info)
	if !ok {
		if info.Refresh != "" {
			_ = a.api.Logout(r.Context(), info.Refresh)
		}
		return
	}
	respond(w, 200, a.sessionView(st))
}
func (a *app) submit(w http.ResponseWriter, r *http.Request, s *session) {
	var req RunRequest
	if !decode(w, r, &req) {
		return
	}
	if !uuidFormat.MatchString(req.ID) || len(req.Accounts) < 1 || len(req.Accounts) > 24 || len(req.Questions) < 1 || len(req.Questions) > 3 || req.Parallel < 1 || req.Parallel > a.cfg.Parallel {
		fail(w, 400, "SELECTION", "请选择 1–24 个账号、1–3 道题和可用的并发数")
		return
	}
	seen := map[int64]bool{}
	for _, id := range req.Accounts {
		if id <= 0 || seen[id] {
			fail(w, 400, "ACCOUNT", "账号选择无效")
			return
		}
		seen[id] = true
	}
	asked := map[string]bool{}
	qs := make([]Question, 0, len(req.Questions))
	for _, id := range req.Questions {
		q, ok := a.questions[id]
		if !ok || asked[id] {
			fail(w, 400, "QUESTION", "题目已变化，请刷新并重新选择")
			return
		}
		asked[id] = true
		qs = append(qs, q)
	}
	if ok, err := a.queue.known(s.owner, req); ok {
		if err != nil {
			fail(w, 409, "SUBMISSION", "此提交编号已被使用")
			return
		}
		respond(w, 200, map[string]string{"id": req.ID})
		return
	}
	accounts, err := a.api.List(r.Context(), s.jwt)
	if err != nil {
		fault(w, err)
		return
	}
	byID := make(map[int64]AccountView, len(accounts))
	for _, a := range accounts {
		byID[a.ID] = a
	}
	if err = a.queue.start(s.ctx, s.owner, s.jwt, req, byID, qs); err != nil {
		fail(w, 409, "CANNOT_START", err.Error())
		return
	}
	respond(w, 202, map[string]string{"id": req.ID})
}
func (a *app) runDetail(w http.ResponseWriter, r *http.Request, s *session) {
	rest, found := strings.CutPrefix(r.URL.Path, "/api/runs/")
	if !found {
		http.NotFound(w, r)
		return
	}
	id, op, has := strings.Cut(rest, "/")
	if !uuidFormat.MatchString(id) {
		http.NotFound(w, r)
		return
	}
	if has {
		if op != "cancel" {
			http.NotFound(w, r)
			return
		}
		if !exactMethod(w, r, "POST") {
			return
		}
		var empty struct{}
		if !decode(w, r, &empty) {
			return
		}
		if a.queue.stopRun(s.owner, id) != nil {
			fail(w, 404, "NOT_FOUND", errAbsent.Error())
			return
		}
		respond(w, 200, map[string]bool{"ok": true})
		return
	}
	if !exactMethod(w, r, "GET") {
		return
	}
	run, err := a.queue.get(s.owner, id)
	if err != nil {
		if errors.Is(err, errAbsent) {
			fail(w, 404, "NOT_FOUND", errAbsent.Error())
		} else {
			fail(w, 503, "STORAGE", errHistory.Error())
		}
		return
	}
	respond(w, 200, run)
}
func run() error {
	config := flag.String("config", "config.json", "path to detector config")
	flag.Parse()
	file, err := os.Open(*config)
	if err != nil {
		return err
	}
	defer file.Close()
	var cfg settings
	d := json.NewDecoder(io.LimitReader(file, 64<<10))
	d.DisallowUnknownFields()
	if e := d.Decode(&cfg); e != nil {
		return errors.New("invalid config JSON")
	}
	client, err := NewAdapter(cfg.Base)
	if err != nil {
		return err
	}
	a, err := newApp(cfg, client)
	if err != nil {
		return err
	}
	defer a.queue.close()
	srv := &http.Server{Addr: cfg.Listen, Handler: a, ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 20 * time.Second, WriteTimeout: 40 * time.Second, IdleTimeout: 45 * time.Second, MaxHeaderBytes: 16 << 10}
	stop, done := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer done()
	go func() {
		t := time.NewTicker(time.Minute)
		defer t.Stop()
		for {
			select {
			case <-t.C:
				a.prune()
			case <-stop.Done():
				ctx, c := context.WithTimeout(context.Background(), 5*time.Second)
				_ = srv.Shutdown(ctx)
				c()
				return
			}
		}
	}()
	log.Printf("private detector listening on %s", cfg.Listen)
	err = srv.ListenAndServe()
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}
func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
