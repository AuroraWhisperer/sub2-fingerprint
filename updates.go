package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

const updateRepository = "AuroraWhisperer/sub2-fingerprint"

var stableVersion = regexp.MustCompile(`^v(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)$`)

type updateStatus struct {
	Current   string `json:"current"`
	Latest    string `json:"latest,omitempty"`
	Supported bool   `json:"supported"`
	Available bool   `json:"available"`
	State     string `json:"state"`
	Message   string `json:"message"`
	Release   string `json:"release"`
}

type updater struct {
	mu          sync.Mutex // Serializes installation requests with detection submissions.
	checkMu     sync.Mutex // Coalesces checks without blocking status or detection.
	requestPath string
	statusPath  string
	unitPath    string
	latestURL   string
	client      *http.Client
	latest      string
	checked     time.Time
}

func newUpdater() *updater {
	return &updater{
		requestPath: "/run/sub2-fingerprint/update.request",
		statusPath:  "/run/sub2-fingerprint-update/status.json",
		unitPath:    "/etc/systemd/system/sub2-fingerprint-update.path",
		latestURL:   "https://api.github.com/repos/" + updateRepository + "/releases/latest",
		client:      &http.Client{Transport: newTransport(), Timeout: 15 * time.Second, CheckRedirect: rejectRedirect},
	}
}

func newerVersion(candidate, current string) bool {
	left, right := stableVersion.FindStringSubmatch(candidate), stableVersion.FindStringSubmatch(current)
	if left == nil || right == nil {
		return false
	}
	for i := 1; i <= 3; i++ {
		l, le := strconv.ParseUint(left[i], 10, 32)
		r, re := strconv.ParseUint(right[i], 10, 32)
		if le != nil || re != nil {
			return false
		}
		if l != r {
			return l > r
		}
	}
	return false
}

func (u *updater) status() (updateStatus, error) {
	s := updateStatus{Current: strings.TrimSpace(releaseVersion), Latest: u.latest,
		State: "idle", Message: "检查 GitHub 最新正式版。", Release: "https://github.com/" + updateRepository + "/releases"}
	s.Available = newerVersion(s.Latest, s.Current)
	if info, err := os.Stat(u.unitPath); err == nil && info.Mode().IsRegular() {
		s.Supported = true
	}
	data, err := os.ReadFile(u.statusPath)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return s, err
	}
	if len(data) > 0 {
		var result struct{ State string }
		if len(data) > 4096 || json.Unmarshal(data, &result) != nil {
			return s, errors.New("invalid update status")
		}
		switch result.State {
		case "running":
			s.State, s.Message = "running", "正在下载、校验并安装更新，完成后自动重新连接。"
		case "done":
			s.State, s.Message = "done", "更新已完成，检测服务已通过健康检查。"
		case "failed":
			s.State, s.Message = "failed", "更新未完成。请查看服务器更新服务日志，确认恢复状态后重试。"
		default:
			return s, errors.New("unknown update status")
		}
	}
	if info, err := os.Stat(u.requestPath); err == nil {
		if s.State != "running" && time.Since(info.ModTime()) > 2*time.Minute {
			if err := os.Remove(u.requestPath); err != nil && !errors.Is(err, os.ErrNotExist) {
				return s, err
			}
			s.State, s.Message = "failed", "更新服务未响应，请检查服务器上的更新服务是否启用。"
		} else {
			s.State, s.Message = "queued", "已提交更新，正在等待更新服务。"
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return s, err
	}
	return s, nil
}

func updateBusy(s updateStatus) bool { return s.State == "queued" || s.State == "running" }

// The caller holds u.mu when checking or reserving an installation or detection.
func (u *updater) detectionReady() bool {
	s, err := u.status()
	return err == nil && !updateBusy(s)
}

func (u *updater) check(ctx context.Context) error {
	u.checkMu.Lock()
	defer u.checkMu.Unlock()
	u.mu.Lock()
	fresh := time.Since(u.checked) < time.Minute
	u.mu.Unlock()
	if fresh {
		return nil
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.latestURL, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("User-Agent", "sub2-fingerprint-updater")
	resp, err := u.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("release status %d", resp.StatusCode)
	}
	var release struct {
		Tag        string `json:"tag_name"`
		Draft      bool   `json:"draft"`
		Prerelease bool   `json:"prerelease"`
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, (512<<10)+1))
	if err != nil || len(data) > 512<<10 || json.Unmarshal(data, &release) != nil ||
		release.Draft || release.Prerelease || !stableVersion.MatchString(release.Tag) {
		return errors.New("invalid stable release")
	}
	u.mu.Lock()
	u.latest, u.checked = release.Tag, time.Now()
	u.mu.Unlock()
	return nil
}

func (a *app) updateRoute(w http.ResponseWriter, r *http.Request) {
	u := a.updater
	method := http.MethodPost
	if r.URL.Path == "/api/update" {
		method = http.MethodGet
	}
	if !exactMethod(w, r, method) {
		return
	}
	if method == http.MethodPost {
		if !decode(w, r, &struct{}{}) {
			return
		}
	}
	if r.URL.Path == "/api/update/check" {
		if err := u.check(r.Context()); err != nil {
			fail(w, 502, "UPDATE_CHECK", "无法读取 GitHub 最新版本，请稍后重试。")
			return
		}
	}
	u.mu.Lock()
	defer u.mu.Unlock()
	for {
		s, err := u.status()
		if err != nil {
			fail(w, 503, "UPDATE_STATUS", "无法读取更新状态，请检查服务器更新服务。")
			return
		}
		if r.URL.Path != "/api/update/install" {
			respond(w, 200, s)
			return
		}
		if !s.Supported {
			fail(w, 409, "UPDATE_UNSUPPORTED", "此安装尚未启用页面更新，请在服务器再次运行命令行更新以补齐服务。")
			return
		}
		if updateBusy(s) {
			respond(w, 202, s)
			return
		}
		if a.queue.busy() {
			fail(w, 409, "DETECTION_ACTIVE", "检测尚未结束，请等待账号释放后再更新。")
			return
		}
		if time.Since(u.checked) < time.Minute {
			a.requestUpdate(w, s)
			return
		}
		u.mu.Unlock()
		err = u.check(r.Context())
		u.mu.Lock()
		if err != nil {
			fail(w, 502, "UPDATE_CHECK", "无法核对 GitHub 最新版本，请稍后重试。")
			return
		}
	}
}

func (a *app) requestUpdate(w http.ResponseWriter, s updateStatus) {
	u := a.updater
	if !newerVersion(u.latest, s.Current) {
		s.Latest, s.Available, s.Message = u.latest, false, "当前已是最新正式版。"
		respond(w, 200, s)
		return
	}
	file, err := os.OpenFile(filepath.Clean(u.requestPath), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		fail(w, 503, "UPDATE_REQUEST", "无法提交更新，请检查服务器更新服务。")
		return
	}
	file.Close()
	log.Print("administrator requested repository update")
	s.State, s.Message, s.Latest, s.Available = "queued", "已提交更新，完成后自动重新连接。", u.latest, true
	respond(w, 202, s)
}
