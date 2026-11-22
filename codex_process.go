package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
)

const (
	codexProviderName         = "sub2_fingerprint"
	codexQuestionInstructions = "Answer the user’s numerical challenge directly. Do not use tools."
)

// The pinned client's bundled Astra catalog selects code_mode_only even when local
// tool features are off. Its supported model_catalog_json setting lets this isolated
// client retain Astra's Responses Lite and low settings with no executable tools.
const codexProbeCatalog = `{"models":[{
  "slug":"` + probeModel + `", "display_name":"GPT-6-Astra", "base_instructions":"",
  "default_reasoning_level":"low", "supported_reasoning_levels":[{"effort":"low","description":"Fast responses with lighter reasoning"}],
  "use_responses_lite":true, "supports_reasoning_effort_updates":true, "default_reasoning_summary":"none",
  "support_verbosity":true, "default_verbosity":"low", "context_window":272000, "max_context_window":872000,
  "truncation_policy":{"mode":"tokens","limit":10000}, "visibility":"list", "supported_in_api":true, "priority":2,
  "tool_mode":"direct", "shell_type":"disabled", "experimental_supported_tools":[]
}]}`

// The relay serves model requests under a per-question path. Every other service,
// including HTTP(S) proxies, points at the same loopback listener and is denied.
const codexProbeConfig = `model = %q
model_catalog_json = %q
model_provider = "sub2_fingerprint"
model_reasoning_effort = "low"
approval_policy = "never"
sandbox_mode = "read-only"
include_permissions_instructions = false
include_apps_instructions = false
include_collaboration_mode_instructions = false
include_environment_context = false
include_environment_context_time = false
web_search = "disabled"
chatgpt_base_url = %q
openai_base_url = %q
cli_auth_credentials_store = "ephemeral"
[model_providers.sub2_fingerprint]
name = "Sub2 Fingerprint"
base_url = %q
wire_api = "responses"
requires_openai_auth = true
supports_websockets = false
request_max_retries = 0
stream_max_retries = 0
[skills]
include_instructions = false
[skills.bundled]
enabled = false
[analytics]
enabled = false
[history]
persistence = "none"
[agents]
enabled = false
[tools.experimental_request_user_input]
enabled = false
[tools.update_plan]
enabled = false
[features]
apps = false
plugins = false
hooks = false
sleep_tool = false
multi_agent = false
multi_agent_v2 = { enabled = false, root_agent_usage_hint_text = "", multi_agent_mode_hint_text = "" }
skip_host_skill_discovery = true
goals = false
shell_tool = false
unified_exec = false
view_image = false
image_generation = false
standalone_web_search = false
tool_suggest = false
`

// Ephemeral clients can still create logs. On Linux keep them on tmpfs, preferably
// in the directory systemd removes when the socket-activated service stops.
func codexTempParent() (string, error) {
	if runtime.GOOS != "linux" {
		return filepath.Abs(os.TempDir())
	}
	mounts, err := os.ReadFile("/proc/self/mountinfo")
	if err != nil {
		return "", gate("CODEX_RUNTIME", "无法验证官方客户端的临时内存目录")
	}
	const serviceDir = "/run/sub2-fingerprint"
	if os.Getenv("RUNTIME_DIRECTORY") == serviceDir && codexTmpfsContains(string(mounts), serviceDir) {
		if info, err := os.Lstat(serviceDir); err == nil && info.IsDir() && info.Mode().Perm() == 0700 {
			return serviceDir, nil
		}
	}
	if codexTmpfsContains(string(mounts), "/dev/shm") {
		return "/dev/shm", nil
	}
	return "", gate("CODEX_RUNTIME", "官方客户端需要可写的临时内存目录")
}

func codexTmpfsContains(mounts, target string) bool {
	var deepest, fsType string
	for _, line := range strings.Split(mounts, "\n") {
		left, right, found := strings.Cut(line, " - ")
		fields, filesystem := strings.Fields(left), strings.Fields(right)
		if !found || len(fields) <= 4 || len(filesystem) == 0 {
			continue
		}
		mount := fields[4]
		if len(mount) >= len(deepest) && (target == mount || strings.HasPrefix(target, strings.TrimSuffix(mount, "/")+"/")) {
			deepest, fsType = mount, filesystem[0]
		}
	}
	return fsType == "tmpfs"
}

func runCodexProcess(ctx context.Context, binary, endpoint, token, accountID, prompt string) (result error) {
	if err := ctx.Err(); err != nil {
		return httpFailure(ctx, err, "官方客户端")
	}
	u, err := url.Parse(endpoint)
	if err != nil || u.Scheme != "http" || u.Hostname() != "127.0.0.1" || u.Port() == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || !strings.HasSuffix(u.Path, "/v1") || !filepath.IsAbs(binary) {
		return gate("CODEX_RUNTIME", "官方客户端或本地中转地址配置无效")
	}
	tmp, err := codexTempParent()
	if err != nil {
		return err
	}
	root, err := os.MkdirTemp(tmp, "sub2-codex-")
	if err != nil {
		return gate("CODEX_RUNTIME", "无法创建官方客户端的临时目录")
	}
	if filepath.Dir(root) != filepath.Clean(tmp) || !strings.HasPrefix(filepath.Base(root), "sub2-codex-") {
		return gate("CODEX_RUNTIME", "官方客户端的临时目录位置异常")
	}
	defer func() {
		if err := os.RemoveAll(root); err != nil && result == nil {
			result = gate("CODEX_RUNTIME", "官方客户端的临时目录未能清理")
		}
	}()
	codexHome, work, scratch := filepath.Join(root, "codex"), filepath.Join(root, "empty"), filepath.Join(root, "tmp")
	for _, dir := range []string{codexHome, work, scratch} {
		if err := os.Mkdir(dir, 0700); err != nil {
			return gate("CODEX_RUNTIME", "无法准备官方客户端的临时目录")
		}
	}
	origin := "http://" + u.Host
	catalog := filepath.Join(codexHome, "catalog.json")
	if err := os.WriteFile(catalog, []byte(codexProbeCatalog), 0600); err != nil {
		return gate("CODEX_RUNTIME", "无法准备官方客户端的测试模型配置")
	}
	config := fmt.Sprintf(codexProbeConfig, probeModel, catalog, origin+"/disabled/", endpoint, endpoint)
	if err := os.WriteFile(filepath.Join(codexHome, "config.toml"), []byte(config), 0600); err != nil {
		return gate("CODEX_RUNTIME", "无法准备官方客户端的运行配置")
	}
	args := []string{"--listen", "stdio://"}
	if strings.EqualFold(strings.TrimSuffix(filepath.Base(binary), ".exe"), "codex") {
		args = append([]string{"app-server"}, args...)
	}
	cmd := exec.CommandContext(ctx, binary, args...)
	cmd.Dir = work
	cmd.Env = []string{
		"HOME=" + root, "USERPROFILE=" + root, "CODEX_HOME=" + codexHome, "XDG_CONFIG_HOME=" + root, "XDG_CACHE_HOME=" + scratch,
		"XDG_DATA_HOME=" + root, "XDG_STATE_HOME=" + scratch, "APPDATA=" + root, "LOCALAPPDATA=" + scratch, "PROGRAMDATA=" + root,
		"TMPDIR=" + scratch, "TEMP=" + scratch, "TMP=" + scratch, "PATH=" + filepath.Dir(binary),
		"OPENAI_BASE_URL=" + endpoint, "HTTP_PROXY=" + origin, "HTTPS_PROXY=" + origin, "ALL_PROXY=" + origin, "NO_PROXY=127.0.0.1",
		"http_proxy=" + origin, "https_proxy=" + origin, "all_proxy=" + origin, "no_proxy=127.0.0.1", "TOKIO_WORKER_THREADS=1", "RAYON_NUM_THREADS=1", "OTEL_SDK_DISABLED=true", "DO_NOT_TRACK=1", "RUST_LOG=off", "GIT_CONFIG_NOSYSTEM=1", "GIT_TERMINAL_PROMPT=0",
	}
	if runtime.GOOS == "windows" {
		for _, key := range []string{"SYSTEMROOT", "WINDIR", "COMSPEC", "PATHEXT", "SystemDrive"} {
			if value, ok := os.LookupEnv(key); ok {
				cmd.Env = append(cmd.Env, key+"="+value)
			}
		}
	}
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return gate("CODEX_RUNTIME", "无法连接官方客户端")
	}
	defer stdin.Close()
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return gate("CODEX_RUNTIME", "无法连接官方客户端")
	}
	defer stdout.Close()
	// stderr is deliberately discarded: Codex may include a raw provider error in it.
	if err := cmd.Start(); err != nil {
		return gate("CODEX_RUNTIME", "无法启动官方 Codex 客户端")
	}
	stop := context.AfterFunc(ctx, func() { _ = stdout.Close(); _ = stdin.Close() })
	defer func() {
		stop()
		_ = stdin.Close()
		_ = stdout.Close()
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		if err := ctx.Err(); err != nil {
			result = httpFailure(ctx, err, "官方客户端")
		}
	}()
	return codexProbeSession(stdout, stdin, work, token, accountID, prompt)
}

var codexProtocolError = gate("CODEX_PROTOCOL", "官方客户端未返回预期的独立测试结果")

type codexMessage struct {
	ID     json.RawMessage `json:"id"`
	Method string          `json:"method"`
	Params struct {
		ThreadID string                      `json:"threadId"`
		TurnID   string                      `json:"turnId"`
		Turn     struct{ ID, Status string } `json:"turn"`
		Item     struct {
			Type, Text, Phase string
			Content           []struct{ Type, Text string } `json:"content"`
		} `json:"item"`
	} `json:"params"`
	Result json.RawMessage `json:"result"`
	Error  json.RawMessage `json:"error"`
}

type codexProbeRPC struct {
	scan                          *bufio.Scanner
	write                         *json.Encoder
	thread, turn, prompt          string
	userSeen, finalSeen, finished bool
}

func (p *codexProbeRPC) notification(m *codexMessage) error {
	if len(m.ID) != 0 {
		return gate("CODEX_CLIENT_ACTION", "官方客户端请求了测试不需要的额外操作")
	}
	v := &m.Params
	switch m.Method {
	case "model/rerouted":
		return gate("UPSTREAM_MODEL", "官方客户端报告模型切换，本题未计分")
	case "error":
		return gate("CODEX_TURN_FAILED", "官方客户端未完成测试请求")
	case "item/started", "item/completed", "turn/completed":
	default:
		return nil
	}
	turnID := v.TurnID
	if m.Method == "turn/completed" {
		turnID = v.Turn.ID
	}
	if p.thread == "" || v.ThreadID != p.thread || turnID == "" || p.turn != "" && p.turn != turnID {
		return codexProtocolError
	}
	p.turn = turnID
	if m.Method == "turn/completed" {
		if v.Turn.Status != "completed" {
			return gate("CODEX_TURN_FAILED", "官方客户端未完成测试请求")
		}
		if !p.userSeen || !p.finalSeen {
			return codexProtocolError
		}
		p.finished = true
		return nil
	}
	switch v.Item.Type {
	case "reasoning":
	case "userMessage":
		if m.Method == "item/completed" {
			if p.userSeen || len(v.Item.Content) != 1 || v.Item.Content[0].Type != "text" || v.Item.Content[0].Text != p.prompt {
				return codexProtocolError
			}
			p.userSeen = true
		}
	case "agentMessage":
		if m.Method == "item/completed" && v.Item.Phase != "commentary" {
			if p.finalSeen || strings.TrimSpace(v.Item.Text) == "" {
				return codexProtocolError
			}
			p.finalSeen = true
		}
	default:
		return gate("UPSTREAM_REFUSAL", "官方客户端返回了工具或其他非答案内容，本题未计分")
	}
	return nil
}

func (p *codexProbeRPC) read(id int, result any) error {
	for p.scan.Scan() {
		var m codexMessage
		if json.Unmarshal(p.scan.Bytes(), &m) != nil {
			return codexProtocolError
		}
		if m.Method != "" {
			if err := p.notification(&m); err != nil {
				return err
			}
			if id == 0 && p.finished {
				return nil
			}
			continue
		}
		if id == 0 || string(m.ID) != fmt.Sprint(id) || len(m.Error) > 0 || len(m.Result) == 0 {
			return codexProtocolError
		}
		if result != nil && json.Unmarshal(m.Result, result) != nil {
			return codexProtocolError
		}
		return nil
	}
	return codexProtocolError
}

func (p *codexProbeRPC) call(id int, method string, params map[string]any, result any) error {
	if p.write.Encode(map[string]any{"id": id, "method": method, "params": params}) != nil {
		return codexProtocolError
	}
	return p.read(id, result)
}

func codexProbeSession(out io.Reader, in io.Writer, work, token, accountID, prompt string) error {
	p := codexProbeRPC{scan: bufio.NewScanner(io.LimitReader(out, 4<<20)), write: json.NewEncoder(in), prompt: prompt}
	p.scan.Buffer(make([]byte, 4096), 512<<10)
	version := strings.TrimPrefix(strings.TrimSpace(releaseVersion), "v")
	if err := p.call(1, "initialize", map[string]any{"clientInfo": map[string]string{"name": codexProviderName, "version": version}, "capabilities": map[string]bool{"experimentalApi": true}}, nil); err != nil {
		return err
	}
	if p.write.Encode(map[string]string{"method": "initialized"}) != nil {
		return codexProtocolError
	}
	if err := p.call(2, "account/login/start", map[string]any{"type": "chatgptAuthTokens", "accessToken": token, "chatgptAccountId": accountID}, nil); err != nil {
		return err
	}
	var started struct {
		Model, ModelProvider, ReasoningEffort string
		Thread                                struct {
			ID, Model string
			Ephemeral bool
		}
	}
	threadParams := map[string]any{
		"model": probeModel, "modelProvider": codexProviderName, "allowProviderModelFallback": false,
		"cwd": work, "approvalPolicy": "never", "sandbox": "read-only", "ephemeral": true,
		"environments": []any{}, "dynamicTools": []any{}, "runtimeWorkspaceRoots": []any{},
		"selectedCapabilityRoots": []any{}, "baseInstructions": "", "developerInstructions": codexQuestionInstructions,
	}
	if err := p.call(3, "thread/start", threadParams, &started); err != nil {
		return err
	}
	if started.Thread.ID == "" || !started.Thread.Ephemeral || started.Model != probeModel || started.Thread.Model != "" && started.Thread.Model != probeModel || started.ModelProvider != codexProviderName || started.ReasoningEffort != "low" {
		return codexProtocolError
	}
	p.thread = started.Thread.ID
	var turn struct{ Turn struct{ ID string } }
	if err := p.call(4, "turn/start", map[string]any{"threadId": p.thread, "model": probeModel, "effort": "low", "environments": []any{}, "input": []any{map[string]string{"type": "text", "text": prompt}}}, &turn); err != nil {
		return err
	}
	if turn.Turn.ID == "" || p.turn != "" && p.turn != turn.Turn.ID {
		return codexProtocolError
	}
	p.turn = turn.Turn.ID
	if p.finished {
		return nil
	}
	return p.read(0, nil)
}
