package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"
)

const rememberPeriod = 7 * 24 * time.Hour
const cookieName = "s2fp_session"

type session struct {
	mu                            sync.Mutex // Sub2API rotates refresh tokens. One request at a time may refresh or log out this session.
	key, csrf, jwt, refresh, temp string
	owner                         int64
	end, accessEnd                time.Time
	remember, verified, dirty     bool
	ctx                           context.Context
	cancel                        context.CancelFunc
}

type savedSession struct {
	Site    string    `json:"site"`
	Owner   int64     `json:"owner"`
	Refresh string    `json:"refresh"`
	CSRF    string    `json:"csrf"`
	End     time.Time `json:"expires"`
}

func cookieKey(value string) string {
	digest := sha256.Sum256([]byte(value))
	return hex.EncodeToString(digest[:])
}

func (a *app) loadLogins() error {
	if !filepath.IsAbs(a.stateDir) || filepath.Clean(a.stateDir) == filepath.Clean(a.cfg.Data) {
		return errors.New("invalid private login directory")
	}
	if err := os.MkdirAll(a.stateDir, 0700); err != nil {
		return err
	}
	stat, err := os.Lstat(a.stateDir)
	if err != nil || !stat.IsDir() || runtime.GOOS != "windows" && stat.Mode().Perm()&0077 != 0 {
		return errors.New("login directory must be private")
	}
	files, err := os.ReadDir(a.stateDir)
	if err != nil {
		return err
	}
	for _, file := range files {
		name := file.Name()
		if key, ok := strings.CutSuffix(name, ".pending"); ok && len(key) == 64 && file.Type().IsRegular() {
			if _, err := hex.DecodeString(key); err == nil {
				if err = os.Remove(filepath.Join(a.stateDir, name)); err != nil {
					return err
				}
				continue
			}
		}
		key, ok := strings.CutSuffix(name, ".session")
		if !ok || len(key) != 64 {
			continue
		}
		if _, err = hex.DecodeString(key); err != nil {
			continue
		}
		path := filepath.Join(a.stateDir, name)
		st, err := file.Info()
		if err != nil {
			return err
		}
		if !st.Mode().IsRegular() || st.Size() > 64<<10 || (runtime.GOOS != "windows" && st.Mode().Perm()&0077 != 0) {
			return errors.New("invalid private login file")
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		var rec savedSession
		if json.Unmarshal(data, &rec) != nil || rec.Owner <= 0 || rec.Refresh == "" || len(rec.CSRF) != 48 {
			return errors.New("invalid saved login")
		}
		if rec.Site != a.cfg.Base || !time.Now().Before(rec.End) {
			if err = os.Remove(path); err != nil {
				return err
			}
			continue
		}
		if len(a.sessions) >= 48 {
			return errors.New("too many saved logins")
		}
		ctx, cancel := context.WithDeadline(context.Background(), rec.End)
		a.sessions[key] = &session{key: key, csrf: rec.CSRF, owner: rec.Owner, refresh: rec.Refresh, end: rec.End, remember: true, ctx: ctx, cancel: cancel}
	}
	return nil
}

func (a *app) saveLogin(s *session) error {
	data, err := json.Marshal(savedSession{a.cfg.Base, s.owner, s.refresh, s.csrf, s.end})
	if err != nil {
		return err
	}
	// A fixed temporary file belongs to this session only and is overwritten after an interrupted write.
	tmp := filepath.Join(a.stateDir, s.key+".pending")
	file, err := os.OpenFile(tmp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	defer os.Remove(tmp)
	if _, err = file.Write(data); err == nil {
		err = file.Sync()
	}
	closeErr := file.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	if err = os.Rename(tmp, filepath.Join(a.stateDir, s.key+".session")); err != nil {
		return err
	}
	return syncDir(a.stateDir)
}

func (a *app) dropLogin(s *session) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.sessions[s.key] != s {
		return nil
	}
	if s.remember && s.owner > 0 {
		err := os.Remove(filepath.Join(a.stateDir, s.key+".session"))
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		if err = syncDir(a.stateDir); err != nil {
			return err
		}
	}
	delete(a.sessions, s.key)
	s.cancel()
	return nil
}

func (a *app) prune() {
	a.mu.Lock()
	var expired []*session
	for _, s := range a.sessions {
		if !time.Now().Before(s.end) {
			expired = append(expired, s)
		}
	}
	a.mu.Unlock()
	for _, s := range expired {
		s.mu.Lock()
		_ = a.dropLogin(s)
		s.mu.Unlock()
	}
}

func (a *app) lookup(r *http.Request) *session {
	c, err := r.Cookie(cookieName)
	if err != nil || len(c.Value) != 48 {
		return nil
	}
	a.mu.Lock()
	s := a.sessions[cookieKey(c.Value)]
	a.mu.Unlock()
	if s == nil {
		return nil
	}
	s.mu.Lock()
	a.mu.Lock()
	valid := a.sessions[s.key] == s && time.Now().Before(s.end)
	a.mu.Unlock()
	if !valid {
		s.mu.Unlock()
		return nil
	}
	return s
}

func accessTTL(seconds int) time.Duration {
	if seconds > 0 && seconds < 1800 {
		return time.Duration(seconds) * time.Second
	}
	return 30 * time.Minute
}

func (a *app) replace(w http.ResponseWriter, old *session, info LoginResult, remember bool) (*session, bool) {
	if old != nil {
		if err := a.dropLogin(old); err != nil {
			fault(w, gate("SESSION_STORE", "登录状态暂时无法保存，请重试"))
			return nil, false
		}
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if len(a.sessions) >= 48 {
		fail(w, 429, "SESSION_LIMIT", "本机打开的会话过多，请稍后重试")
		return nil, false
	}
	now := time.Now()
	lifetime := 15 * time.Minute
	switch {
	case info.Temp != "":
		lifetime = 5 * time.Minute
	case info.JWT != "":
		remember = remember && info.Refresh != ""
		lifetime = accessTTL(info.ExpiresIn)
		if remember {
			lifetime = rememberPeriod
		}
	default:
		remember = false
	}
	end := now.Add(lifetime)
	ctx, cancel := context.WithDeadline(context.Background(), end)
	value := randomID()
	s := &session{key: cookieKey(value), csrf: randomID(), jwt: info.JWT, refresh: info.Refresh, temp: info.Temp, owner: info.UserID, end: end, accessEnd: now.Add(accessTTL(info.ExpiresIn)), remember: remember, verified: info.Admin, ctx: ctx, cancel: cancel}
	if s.remember && s.owner > 0 {
		if err := a.saveLogin(s); err != nil {
			cancel()
			fail(w, 503, "SESSION_STORE", "本机暂时无法保存登录状态，请重试")
			return nil, false
		}
	}
	a.sessions[s.key] = s
	cookie := &http.Cookie{Name: cookieName, Value: value, HttpOnly: true, SameSite: http.SameSiteStrictMode, Path: "/"}
	if s.remember && s.owner > 0 {
		cookie.Expires = end
		cookie.MaxAge = int(rememberPeriod / time.Second)
	}
	http.SetCookie(w, cookie)
	return s, true
}

func (a *app) authorize(ctx context.Context, s *session) error {
	if s.owner <= 0 || !s.remember {
		return nil
	}
	if s.jwt == "" || !time.Now().Before(s.accessEnd) {
		info, err := a.api.Refresh(ctx, s.refresh)
		if err != nil {
			return err
		}
		if info.JWT == "" || info.Refresh == "" {
			return gate("SESSION_INVALID", "登录已失效，请重新登录")
		}
		s.jwt = info.JWT
		s.refresh = info.Refresh
		s.accessEnd = time.Now().Add(accessTTL(info.ExpiresIn))
		s.verified = false
		s.dirty = true
	}
	if s.dirty {
		// Commit the rotated refresh token before any further network call.
		if err := a.saveLogin(s); err != nil {
			return gate("SESSION_STORE", "本机暂时无法保存续期，请稍后重试")
		}
		s.dirty = false
	}
	if !s.verified {
		if err := a.api.VerifyAdmin(ctx, s.jwt, s.owner); err != nil {
			if err == accessExpired {
				s.accessEnd = time.Time{}
			}
			return err
		}
		s.verified = true
	}
	return nil
}

func invalidLogin(err error) bool {
	var g *GateError
	return errors.As(err, &g) && (g.Code == "SESSION_INVALID" || g.Code == "UNAUTHORIZED")
}
func (a *app) sessionFailure(w http.ResponseWriter, s *session, err error) {
	if invalidLogin(err) {
		if a.dropLogin(s) != nil {
			fail(w, 503, "SESSION_STORE", "无法清除已失效的登录状态")
			return
		}
		fail(w, 401, "SESSION", "登录已失效，请重新登录")
		return
	}
	fault(w, err)
}
