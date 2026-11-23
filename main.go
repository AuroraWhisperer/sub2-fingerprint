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
	Base           string `json:"sub2_url"`
	Parallel       int    `json:"parallel"`
	Listen         string `json:"listen"`
	Data           string `json:"data_dir"`
	ProbeAPIKey    string `json:"probe_api_key"`
	ProbeGroupName string `json:"probe_group_name"`
}
type sub2 interface {
	Refresh(context.Context, string) (LoginResult, error)
	CurrentAdmin(context.Context, string) (int64, error)
	VerifyAdmin(context.Context, string, int64) error
	Login(context.Context, string, string) (LoginResult, error)
	Login2FA(context.Context, string, string) (LoginResult, error)
	Logout(context.Context, string) error
	StepUp(context.Context, string, string) error
	List(context.Context, string) ([]AccountView, error)
	Probe(context.Context, probeToken, int64, string) (ProbeAnswer, error)
}
type batchProbe interface {
	ProbeBatch(context.Context, probeToken, int64, []string, probeBatchReporter) error
}
type app struct {
	stateDir    string
	epoch       string
	cfg         settings
	api         sub2
	queue       *queue
	updater     *updater
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
	a := &app{cfg: cfg, api: api, updater: newUpdater(), questions: map[string]Question{}, sessions: map[string]*session{}}
	a.epoch = randomID()
	a.stateDir = os.Getenv("STATE_DIRECTORY")
	if a.stateDir == "" {
		a.stateDir = cfg.Data + ".logins"
	}
	host, port, e := net.SplitHostPort(cfg.Listen)
	if e != nil || host != "127.0.0.1" || port == "" || cfg.Parallel < 1 || cfg.Parallel > maxParallelAccounts || !filepath.IsAbs(cfg.Data) {
		return nil, fmt.Errorf("config requires IPv4 loopback listen, parallel from 1 to %d, and an absolute private data directory", maxParallelAccounts)
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
		for i := range g {
			q := &g[i]
			if q.ID == "" || a.questions[q.ID].ID != "" || q.Expected < 292 || q.Expected > 332 || q.Prompt == "" {
				return nil, errors.New("invalid challenge")
			}
			q.Number = i + 1
			a.questions[q.ID] = *q
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
	if err := a.loadLogins(); err != nil {
		return nil, err
	}
	a.queue, e = openQueue(cfg.Data, a.bankVersion, api.Probe)
	if e == nil {
		if batch, ok := api.(batchProbe); ok {
			a.queue.probeBatch = batch.ProbeBatch
		}
	}
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
	return map[string]any{"epoch": a.epoch, "remembered": s.remember && s.owner > 0, "expires_at": s.end.UTC(), "logged_in": s.verified && s.jwt != "", "needs_2fa": s.temp != "", "csrf": s.csrf, "parallel": a.cfg.Parallel, "bank_version": a.bankVersion, "version": strings.TrimSpace(releaseVersion)}
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
		ok := !a.queue.stopping
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
		if p == "/unified_bank.json" && r.URL.Query().Get("v") == a.bankVersion {
			w.Header().Set("Cache-Control", "private, max-age=31536000, immutable")
		}
		a.static.ServeHTTP(w, r)
		return
	}
	s := a.lookup(r)
	locked := s
	defer func() {
		if locked != nil {
			locked.mu.Unlock()
		}
	}()
	if r.Method == http.MethodPost {
		mt, _, e := mime.ParseMediaType(r.Header.Get("Content-Type"))
		origin := r.Header.Get("Origin")
		if e != nil || mt != "application/json" || (origin != "http://"+r.Host && origin != "https://"+r.Host) || s == nil || subtle.ConstantTimeCompare([]byte(r.Header.Get("X-CSRF-Token")), []byte(s.csrf)) != 1 {
			fail(w, 403, "CSRF", "页面会话已过期或来源不符，请刷新")
			return
		}
	}
	token := bearerToken(r.Header.Get("Authorization"))
	if token != "" {
		if s == nil {
			var ok bool
			s, ok = a.attachExternal(w, r.Context(), token)
			if !ok {
				return
			}
			s.mu.Lock()
			locked = s
		} else if err := a.bindExternal(r.Context(), s, token); err != nil {
			a.sharedAuthFailure(w, err)
			return
		}
	} else if s != nil && s.external {
		if p == "/api/session" {
			if err := a.dropLogin(s); err != nil {
				fail(w, 503, "SESSION_STORE", "无法清除失效的页面会话")
				return
			}
			s = nil
		} else {
			fail(w, http.StatusUnauthorized, "SUB2_SESSION", "请先登录 Sub2API 管理员账号")
			return
		}
	}
	if s != nil && p != "/api/logout" && s.owner > 0 {
		if err := a.authorize(r.Context(), s); err != nil {
			if invalidLogin(err) {
				if err = a.dropLogin(s); err != nil {
					fail(w, 503, "SESSION_STORE", "无法清除已失效的登录状态")
					return
				}
				if p == "/api/session" {
					s = nil
				} else {
					fail(w, 401, "SESSION", "登录已失效，请重新登录")
					return
				}
			} else {
				fail(w, 503, "SESSION_RETRY", "暂时无法续期，请稍后重试")
				return
			}
		}
	}
	if p == "/api/session" {
		if !exactMethod(w, r, "GET") {
			return
		}
		if s == nil {
			var ok bool
			s, ok = a.replace(w, nil, LoginResult{}, false, false)
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
	owner, jwt, credential := s.owner, s.jwt, s.probeJWT
	locked.mu.Unlock()
	locked = nil
	switch p {
	case "/api/update", "/api/update/check", "/api/update/install":
		a.updateRoute(w, r)
	case "/api/accounts":
		if !exactMethod(w, r, "GET") {
			return
		}
		list, e := a.api.List(r.Context(), jwt)
		if e != nil {
			a.sessionFailure(w, s, jwt, e)
			return
		}
		respond(w, 200, map[string]any{"accounts": list})
	case "/api/questions":
		if !exactMethod(w, r, "GET") {
			return
		}
		groups := a.groups
		if previous := r.URL.Query().Get("previous"); previous != "" && len(groups) > 1 {
			for i, group := range groups {
				if group[0].ID == previous {
					groups = append(append([][]Question(nil), groups[:i]...), groups[i+1:]...)
					break
				}
			}
		}
		i, e := rand.Int(rand.Reader, big.NewInt(int64(len(groups))))
		if e != nil {
			fail(w, 503, "RANDOM", "暂不能生成题目")
			return
		}
		respond(w, 200, map[string]any{"questions": groups[i.Int64()]})
	case "/api/runs":
		if r.Method == "GET" {
			respond(w, 200, map[string]any{"runs": a.queue.history(owner)})
			return
		}
		if exactMethod(w, r, "POST") {
			a.submit(w, r, s, owner, func() string { return *credential.Load() })
		}
	default:
		a.runDetail(w, r, owner)
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
		newSession, ok := a.replace(w, s, LoginResult{}, false, false)
		if !ok {
			return
		}
		if !s.external && s.refresh != "" {
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
	remember := s.remember
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
			Remember bool   `json:"remember"`
		}
		if !decode(w, r, &b) {
			return
		}
		b.Email = strings.TrimSpace(b.Email)
		if len(b.Email) > 320 || !strings.Contains(b.Email, "@") || b.Password == "" || len(b.Password) > 1024 {
			fail(w, 400, "LOGIN_INPUT", "请输入有效的邮箱和密码")
			return
		}
		remember = b.Remember
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
	st, ok := a.replace(w, s, info, remember, false)
	if !ok {
		if info.Refresh != "" {
			_ = a.api.Logout(r.Context(), info.Refresh)
		}
		return
	}
	respond(w, 200, a.sessionView(st))
}
func (a *app) submit(w http.ResponseWriter, r *http.Request, s *session, owner int64, token probeToken) {
	a.updater.mu.Lock()
	ready := a.updater.detectionReady()
	a.updater.mu.Unlock()
	if !ready {
		fail(w, 409, "UPDATE_ACTIVE", "程序正在更新或更新状态无法确认，请稍后再开始检测。")
		return
	}
	var req RunRequest
	if !decode(w, r, &req) {
		return
	}
	if req.Epoch != a.epoch {
		fail(w, 409, "OLD_RUN", "检测服务已重新启动，旧提交不会重发。请重新开始一轮检测")
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
	if ok, err := a.queue.known(owner, req); ok {
		if err != nil {
			fail(w, 409, "SUBMISSION", "此提交编号已被使用")
			return
		}
		respond(w, 200, map[string]string{"id": req.ID})
		return
	}
	jwt := token()
	accounts, err := a.api.List(r.Context(), jwt)
	if err != nil {
		a.sessionFailure(w, s, jwt, err)
		return
	}
	byID := make(map[int64]AccountView, len(accounts))
	for _, a := range accounts {
		byID[a.ID] = a
	}
	a.updater.mu.Lock()
	defer a.updater.mu.Unlock()
	if !a.updater.detectionReady() {
		fail(w, 409, "UPDATE_ACTIVE", "程序正在更新或更新状态无法确认，请稍后再开始检测。")
		return
	}
	if err = a.queue.start(s.ctx, owner, token, req, byID, qs); err != nil {
		fail(w, 409, "CANNOT_START", err.Error())
		return
	}
	respond(w, 202, map[string]string{"id": req.ID})
}
func (a *app) runDetail(w http.ResponseWriter, r *http.Request, owner int64) {
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
		if a.queue.stopRun(owner, id) != nil {
			fail(w, 404, "NOT_FOUND", errAbsent.Error())
			return
		}
		respond(w, 200, map[string]bool{"ok": true})
		return
	}
	if !exactMethod(w, r, "GET") {
		return
	}
	run, err := a.queue.get(owner, id)
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
	if cfg.ProbeAPIKey == "" {
		return gate("CONFIG", "检测器未配置 Probe API Key")
	}
	client, err := NewConfiguredAdapter(cfg.Base, cfg.ProbeAPIKey, cfg.ProbeGroupName)
	if err != nil {
		return err
	}
	a, err := newApp(cfg, client)
	if err != nil {
		return err
	}
	defer a.queue.close()
	listener, activated, err := detectorListener(cfg.Listen)
	if err != nil {
		return err
	}
	activity := newIdleTracker()
	srv := &http.Server{Handler: activity.track(a), ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 20 * time.Second, WriteTimeout: 65 * time.Second, IdleTimeout: 45 * time.Second, MaxHeaderBytes: 16 << 10}
	stop, done := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer done()
	go func() {
		tick := time.NewTicker(5 * time.Second)
		defer tick.Stop()
		for {
			select {
			case <-stop.Done():
			case <-tick.C:
				a.prune()
				if !activated || !activity.idle(time.Now(), 2*time.Minute, a.queue.busy()) {
					continue
				}
			}
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			_ = srv.Shutdown(ctx)
			return
		}
	}()
	log.Printf("private detector listening on %s", cfg.Listen)
	err = srv.Serve(listener)
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
