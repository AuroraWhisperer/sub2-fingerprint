package main

import (
	"bytes"
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"mime"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"sync/atomic"
	"time"
)

var (
	codexWireMismatch = gate("CODEX_PROTOCOL", "官方客户端请求与所选账号、模型或独立题目不符，本题未计分")
	codexDuplicate    = gate("CODEX_CONTINUATION", "官方客户端尝试续轮或重复发题，本题未计分")
	codexRequestUUID  = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[47][0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)
)

// A Responses Lite request may place tool schemas or hidden messages inside input.
// Only the fixed local instruction and the single, selected user challenge may leave the gate.
func codexQuestionBody(raw []byte, prompt string) ([]byte, error) {
	type message struct {
		Type, Role string
		Content    []struct{ Type, Text string }
		Tools      []json.RawMessage
	}
	var body struct {
		Model, Instructions  string
		Reasoning            struct{ Effort string }
		Store                *bool
		Stream, Background   bool
		Tools                []json.RawMessage
		ToolChoice           string          `json:"tool_choice"`
		Previous             json.RawMessage `json:"previous_response_id"`
		Conversation, Prompt json.RawMessage
		Input                []message
	}
	if json.Unmarshal(raw, &body) != nil || body.Model != probeModel || body.Reasoning.Effort != "low" || body.Store == nil || *body.Store || !body.Stream {
		return nil, codexWireMismatch
	}
	if body.Background || len(body.Previous) != 0 || len(body.Conversation) != 0 || len(body.Prompt) != 0 || len(body.Tools) != 0 || (body.ToolChoice != "" && body.ToolChoice != "auto" && body.ToolChoice != "none") {
		return nil, codexWireMismatch
	}
	if (body.Instructions != "" && body.Instructions != codexQuestionInstructions) || len(body.Input) == 0 {
		return nil, codexWireMismatch
	}
	for i, item := range body.Input {
		if len(item.Tools) != 0 {
			return nil, codexWireMismatch
		}
		last := i == len(body.Input)-1
		if !last && item.Type == "additional_tools" && item.Role == "developer" && len(item.Content) == 0 {
			continue // An empty native declaration grants no capability.
		}
		if (item.Type != "message" && item.Type != "") || len(item.Content) != 1 || item.Content[0].Type != "input_text" {
			return nil, codexWireMismatch
		}
		role, text := "developer", codexQuestionInstructions
		if last {
			role, text = "user", prompt
		}
		if item.Role != role || item.Content[0].Text != text {
			return nil, codexWireMismatch
		}
	}
	return raw, nil
}

func codexOutboundHeaders(in, account http.Header) (http.Header, string, error) {
	for _, key := range []string{"Authorization", "ChatGPT-Account-Id"} {
		if len(in.Values(key)) != 1 || account.Get(key) == "" || subtle.ConstantTimeCompare([]byte(in.Get(key)), []byte(account.Get(key))) != 1 {
			return nil, "", codexWireMismatch
		}
	}
	ua, origin := in.Get("User-Agent"), in.Get("Originator")
	if len(in.Values("User-Agent")) != 1 || len(in.Values("Originator")) != 1 || origin != codexProviderName || len(ua) > 512 || !strings.HasPrefix(ua, origin+"/") || !headerValue.MatchString(ua) || !clientUA.MatchString(ua) {
		return nil, "", codexWireMismatch
	}
	if configured := account.Get("User-Agent"); configured != "" && configured != ua {
		return nil, "", gate("IDENTITY", "账号指定的客户端标识与官方客户端实际标识不符")
	}
	id := in.Get("X-Client-Request-Id")
	if id == "" {
		id = in.Get("Session-Id")
	}
	if id == "" {
		id = in.Get("Session_id")
	}
	if !codexRequestUUID.MatchString(id) {
		return nil, "", codexWireMismatch
	}
	h := make(http.Header)
	for _, key := range []string{"Authorization", "ChatGPT-Account-Id", "User-Agent", "Originator", "Version", "Accept", "Content-Type", "OpenAI-Beta", "X-Client-Request-Id", "Session-Id", "Session_id", "Thread-Id", "X-Codex-Beta-Features", "X-Codex-Window-Id", "X-Codex-Turn-Metadata", "X-OpenAI-Internal-Codex-Responses-Lite"} {
		if values := in.Values(key); len(values) > 0 {
			if len(values) != 1 || !headerValue.MatchString(values[0]) {
				return nil, "", codexWireMismatch
			}
			h.Set(key, values[0])
		}
	}
	if fed := account.Get("X-OpenAI-Fedramp"); fed != "" {
		h.Set("X-OpenAI-Fedramp", fed)
	}
	return h, id, nil
}

func codexUpstreamStatus(resp *http.Response) error {
	switch resp.StatusCode {
	case 200:
		return nil
	case 400:
		return upstreamBadRequest(resp)
	case 401:
		return gate("UPSTREAM_TOKEN", "上游拒绝账号凭据，请在 Sub2API 核对后重试")
	case 403:
		return gate("UPSTREAM_FORBIDDEN", "上游拒绝当前账号或客户端请求")
	case 429:
		return gate("UPSTREAM_RATE_LIMIT", "账号已被上游限流，请稍后重试")
	default:
		return gate("UPSTREAM_HTTP", fmt.Sprintf("上游返回 HTTP %d", resp.StatusCode))
	}
}

// Only release verified frames to the CLI. In particular, an output_item.added
// tool must not be executable even if a terminal event subsequently omits it.
func codexVerifiedStream(r io.Reader) (ProbeAnswer, []byte, error) {
	var captured bytes.Buffer
	answer, err := readProbeStream(io.TeeReader(r, &captured))
	if err != nil {
		return ProbeAnswer{}, nil, err
	}
	refusal := gate("UPSTREAM_REFUSAL", "上游返回工具或非答案内容，本题未计分")
	wrongModel := gate("UPSTREAM_MODEL", "上游未确认所选 Astra 模型或发生模型切换，本题未计分")
	if answer.Model != probeModel {
		return ProbeAnswer{}, nil, wrongModel
	}
	// Keep the original bytes through the terminal frame, including CRLF and
	// multi-line SSE data, but never forward read-ahead after response.completed.
	var data bytes.Buffer
	rest := captured.Bytes()
	for len(rest) > 0 {
		line, tail, found := bytes.Cut(rest, []byte{'\n'})
		if !found {
			break
		}
		rest = tail
		line = bytes.TrimSuffix(line, []byte{'\r'})
		if fragment, ok := bytes.CutPrefix(line, []byte("data:")); ok {
			data.Write(fragment)
			data.WriteByte('\n')
			continue
		}
		if len(line) != 0 || data.Len() == 0 {
			continue
		}
		finished, err := checkCodexFrame(data.Bytes(), refusal, wrongModel)
		data.Reset()
		if err != nil {
			return ProbeAnswer{}, nil, err
		}
		if finished {
			return answer, captured.Bytes()[:captured.Len()-len(rest)], nil
		}
	}

	return ProbeAnswer{}, nil, codexWireMismatch
}

func checkCodexFrame(data []byte, refusal, wrongModel error) (bool, error) {
	var e struct {
		Type       string
		Item, Part struct{ Type, Role string }
		Response   struct{ Model string }
	}
	if json.Unmarshal(data, &e) != nil {
		return false, codexWireMismatch
	}
	if e.Response.Model != "" && e.Response.Model != probeModel {
		return false, wrongModel
	}
	if strings.Contains(e.Type, "call") || strings.Contains(e.Type, "refusal") || strings.Contains(e.Type, "tool") || e.Type == "response.model_rerouted" {
		return false, refusal
	}
	if strings.HasPrefix(e.Type, "response.output_item.") && e.Item.Type != "reasoning" && (e.Item.Type != "message" || e.Item.Role != "assistant") {
		return false, refusal
	}
	if strings.HasPrefix(e.Type, "response.content_part.") && e.Part.Type != "output_text" && e.Part.Type != "reasoning_text" {
		return false, refusal
	}
	return e.Type == "response.completed", nil
}

func (a *Adapter) forwardCodexQuestion(ctx context.Context, r *http.Request, expected http.Header, proxy *url.URL, prompt string) (ProbeAnswer, []byte, error) {
	h, id, err := codexOutboundHeaders(r.Header, expected)
	if err != nil {
		return ProbeAnswer{}, nil, err
	}
	media, _, _ := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if media != "application/json" || r.Header.Get("Content-Encoding") != "" {
		return ProbeAnswer{}, nil, codexWireMismatch
	}
	const inputLimit = 256 << 10
	data, err := io.ReadAll(io.LimitReader(r.Body, inputLimit+1))
	if err != nil || len(data) > inputLimit {
		return ProbeAnswer{}, nil, codexWireMismatch
	}
	body, err := codexQuestionBody(data, prompt)
	if err != nil {
		return ProbeAnswer{}, nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, a.upstream, bytes.NewReader(body))
	if err != nil {
		return ProbeAnswer{}, nil, gate("CONFIG", "上游地址无效")
	}
	req.Header = h
	transport := a.upstreamTransport.Clone()
	transport.DisableKeepAlives = true
	if proxy != nil {
		transport.Proxy = http.ProxyURL(proxy)
	}
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport, CheckRedirect: rejectRedirect}
	resp, err := client.Do(req)
	if err != nil {
		return ProbeAnswer{}, nil, httpFailure(ctx, err, "上游 ")
	}
	defer resp.Body.Close()
	if err := codexUpstreamStatus(resp); err != nil {
		return ProbeAnswer{}, nil, err
	}
	media, _, _ = mime.ParseMediaType(resp.Header.Get("Content-Type"))
	if media != "text/event-stream" {
		return ProbeAnswer{}, nil, gate("UPSTREAM_FORMAT", "上游未返回预期的事件流")
	}
	answer, stream, err := codexVerifiedStream(resp.Body)
	if err != nil {
		return ProbeAnswer{}, nil, err
	}
	answer.RequestID = id
	return answer, stream, nil
}

func (a *Adapter) probeWithCodex(ctx context.Context, headers http.Header, proxy *url.URL, prompt string) (ProbeAnswer, error) {
	ln, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		return ProbeAnswer{}, gate("CODEX_RUNTIME", "无法启动本机的官方客户端请求中转")
	}
	base := "/" + randomID() + "/v1"
	var claimed, repeated atomic.Bool
	type result struct {
		answer ProbeAnswer
		err    error
	}
	done := make(chan result, 1)
	server := &http.Server{ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 10 * time.Second, MaxHeaderBytes: 64 << 10, ErrorLog: log.New(io.Discard, "", 0), BaseContext: func(net.Listener) context.Context { return ctx }}
	server.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.Host != ln.Addr().String() || r.RequestURI != base+"/responses" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		if claimed.Swap(true) {
			repeated.Store(true)
			w.WriteHeader(http.StatusConflict)
			return
		}
		answer, stream, err := a.forwardCodexQuestion(r.Context(), r, headers, proxy, prompt)
		if err != nil {
			w.WriteHeader(http.StatusBadRequest)
		} else {
			w.Header().Set("Content-Type", "text/event-stream")
			_, err = w.Write(stream)
			if err != nil {
				err = codexWireMismatch
			}
		}
		done <- result{answer, err}
	})
	serving := make(chan struct{})
	go func() { defer close(serving); _ = server.Serve(ln) }()
	runErr := a.runCodex(ctx, a.codexBinary, "http://"+ln.Addr().String()+base, strings.TrimPrefix(headers.Get("Authorization"), "Bearer "), headers.Get("ChatGPT-Account-Id"), prompt)
	_ = server.Close()
	<-serving
	if ctx.Err() != nil {
		return ProbeAnswer{}, httpFailure(ctx, ctx.Err(), "官方客户端")
	}
	if claimed.Load() {
		v := <-done
		if v.err != nil {
			return ProbeAnswer{}, v.err
		}
		if repeated.Load() {
			return ProbeAnswer{}, codexDuplicate
		}
		if runErr == nil {
			return v.answer, nil
		}
	}
	var classified *GateError
	if errors.As(runErr, &classified) {
		return ProbeAnswer{}, classified
	}
	return ProbeAnswer{}, gate("CODEX_TURN_FAILED", "官方客户端未完成本次检测")
}
