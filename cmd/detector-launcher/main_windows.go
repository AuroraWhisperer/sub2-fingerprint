//go:build windows

package main

import (
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
	"time"
)

const port = 17861
const home = "http://127.0.0.1:17861/"
const forward = "127.0.0.1:17861:127.0.0.1:17861"

var targetName = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9._@:-]{0,252}$`)
var portBusy = errors.New("本机端口 17861 已由其他程序使用。若旧启动脚本仍在运行，请先在其窗口内断开。")

func targetFor(exe string) (string, error) {
	raw, err := os.ReadFile(filepath.Join(filepath.Dir(exe), "launcher.local.json"))
	if err != nil {
		return "", errors.New("请将同目录的 launcher.example.json 复制为 launcher.local.json，并把 ssh_target 设为你的 SSH 配置别名。")
	}
	var c struct {
		Target string `json:"ssh_target"`
	}
	if err = json.Unmarshal(raw, &c); err != nil || !targetName.MatchString(c.Target) || c.Target == "YOUR_SSH_ALIAS" {
		return "", errors.New("launcher.local.json 的 ssh_target 需要填写有效的 SSH 配置别名。")
	}
	return c.Target, nil
}

func checkPage(url string) error {
	c := http.Client{Timeout: 8 * time.Second, Transport: &http.Transport{DisableKeepAlives: true}, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	for _, path := range []string{"healthz", ""} {
		r, err := c.Get(url + path)
		if err != nil {
			return fmt.Errorf("无法连接检测器：%w", err)
		}
		var health struct {
			Status  string
			Version string
		}
		if path == "healthz" {
			err = json.NewDecoder(io.LimitReader(r.Body, 4096)).Decode(&health)
		}
		r.Body.Close()
		if r.StatusCode != 200 {
			return fmt.Errorf("检测器访问 /%s 返回 HTTP %d", path, r.StatusCode)
		}
		if path == "healthz" && (err != nil || health.Status != "ok" || health.Version == "") {
			return errors.New("检测器健康接口未返回有效版本。")
		}
	}
	return nil
}

func openPage(check bool) error {
	if err := checkPage(home); err != nil {
		return err
	}
	if check {
		return nil
	}
	return launchBrowser(home)
}

func launch(exe, target string, check bool) error {
	// Scope both kernel objects to this launcher installation and SSH target.
	scope := fmt.Sprintf(`Local\Sub2Fingerprint-%x`, sha256.Sum256([]byte(strings.ToLower(exe)+"\x00"+target)))
	instance, exists, err := namedMutex(scope)
	if err != nil {
		return err
	}
	defer syscall.CloseHandle(instance)
	if exists {
		// A first launch may still be authenticating to SSH when the user double-clicks again.
		deadline := time.Now().Add(18 * time.Second)
		for time.Now().Before(deadline) {
			state, err := portState(port)
			if err != nil {
				return err
			}
			if state.pid != 0 && markerExists(fmt.Sprintf("%s-%d", scope, state.pid)) {
				return openPage(check)
			}
			time.Sleep(100 * time.Millisecond)
		}
		return errors.New("正在运行的启动器尚未建立连接，请稍后重试。")
	}
	state, err := portState(port)
	if err != nil {
		return err
	}
	if state.pid != 0 {
		return portBusy
	}
	system, err := systemDir()
	if err != nil {
		return err
	}
	ssh := exec.Command(filepath.Join(system, "OpenSSH", "ssh.exe"), "-N", "-T", "-a", "-x", "-o", "BatchMode=yes", "-o", "StrictHostKeyChecking=yes", "-o", "ExitOnForwardFailure=yes", "-o", "ControlMaster=no", "-o", "ControlPath=none", "-o", "ConnectTimeout=10", "-o", "ServerAliveInterval=30", "-o", "ServerAliveCountMax=2", "-L", forward, target)
	ssh.SysProcAttr = &syscall.SysProcAttr{HideWindow: true}
	if err = ssh.Start(); err != nil {
		return fmt.Errorf("无法启动 Windows OpenSSH 客户端：%w", err)
	}
	job, err := ownChild(ssh.Process.Pid)
	if err != nil {
		_ = ssh.Process.Kill()
		_ = ssh.Wait()
		return err
	}
	exited := make(chan struct{})
	go func() { _ = ssh.Wait(); close(exited) }()
	defer func() { syscall.CloseHandle(job); <-exited }()
	pid := uint32(ssh.Process.Pid)
	deadline := time.Now().Add(16 * time.Second)
	for {
		state, err = portState(port)
		if err != nil {
			return err
		}
		if state.pid == pid {
			break
		}
		if state.pid != 0 {
			return portBusy
		}
		if time.Now().After(deadline) {
			return errors.New("SSH 转发未就绪。请检查 SSH 别名、密钥、已核验的主机记录及网络连接。")
		}
		select {
		case <-exited:
			return errors.New("Windows OpenSSH 未能建立连接。请检查 SSH 配置别名、密钥和已核验的主机记录。")
		case <-time.After(150 * time.Millisecond):
		}
	}
	marker, _, err := namedMutex(fmt.Sprintf("%s-%d", scope, pid))
	if err != nil {
		return err
	}
	defer syscall.CloseHandle(marker)
	if err = openPage(check); err != nil || check {
		return err
	}
	return waitForIdle(pid, port, 5*time.Minute, 2*time.Second)
}

func waitForIdle(pid uint32, localPort uint16, idle, interval time.Duration) error {
	last := time.Now()
	var prev map[uint16]bool
	for {
		time.Sleep(interval)
		state, err := portState(localPort)
		if err != nil {
			return err
		}
		if state.pid != pid {
			return errors.New("SSH 隧道已断开。再次双击启动器即可重新连接。")
		}
		if state.connected {
			last = time.Now()
		}
		for peer := range state.peers {
			if !prev[peer] {
				last = time.Now()
				break
			}
		}
		prev = state.peers
		if time.Since(last) >= idle {
			return nil
		}
	}
}

func main() {
	check := len(os.Args) == 2 && os.Args[1] == "-check"
	exe, err := os.Executable()
	if err == nil {
		var target string
		target, err = targetFor(exe)
		if err == nil {
			err = launch(exe, target, check)
		}
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		if !check {
			showError(err.Error())
		}
		os.Exit(1)
	}
	if check {
		fmt.Println("Detector is ready at " + home)
	}
}
