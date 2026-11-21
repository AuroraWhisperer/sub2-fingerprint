package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"regexp"
	"sync"
	"time"
)

const maxRowsText = 12 << 10
const maxSeenSubmissions = 1024

var uuidFormat = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)
var errHistory = errors.New("当前检测结果暂不可用")
var errAbsent = errors.New("未找到检测记录")

func randomID() string {
	var b [24]byte
	_, err := rand.Read(b[:])
	if err != nil {
		panic("system random unavailable")
	}
	return hex.EncodeToString(b[:])
}
func nowStamp() string { return time.Now().UTC().Format(time.RFC3339Nano) }

type Question struct {
	ID       string `json:"id"`
	Prompt   string `json:"prompt"`
	Expected int    `json:"expected_count"`
}
type RunRequest struct {
	Epoch     string   `json:"epoch"`
	ID        string   `json:"id"`
	Accounts  []int64  `json:"accounts"`
	Questions []string `json:"questions"`
	Parallel  int      `json:"parallel"`
}
type Summary struct {
	ID    string `json:"id"`
	At    string `json:"at"`
	State string `json:"state"`
}
type Row struct {
	AccountID  int64  `json:"account_id"`
	Name       string `json:"name"`
	QuestionID string `json:"question_id"`
	Status     string `json:"status"`
	RequestID  string `json:"request_id,omitempty"`
	Started    string `json:"started,omitempty"`
	Ended      string `json:"ended,omitempty"`
	Text       string `json:"text,omitempty"`
	Model      string `json:"model,omitempty"`
	Error      string `json:"error,omitempty"`
	Code       string `json:"code,omitempty"`
}
type Run struct {
	Summary
	End         string     `json:"end,omitempty"`
	BankVersion string     `json:"bank_version"`
	Parallel    int        `json:"parallel"`
	Questions   []Question `json:"questions"`
	Rows        []Row      `json:"rows"`
}
type ownedRun struct {
	owner int64
	epoch string
	Run
}
type probeFunc func(context.Context, string, int64, string) (ProbeAnswer, error)
type queue struct {
	mu       sync.Mutex
	version  string
	current  *ownedRun
	seen     map[string]struct{}
	cancel   context.CancelFunc
	stopping bool
	wg       sync.WaitGroup
	probe    probeFunc
}

func nonterminal(s string) bool { return s == "queued" || s == "running" }
func openQueue(_ string, version string, probe probeFunc) (*queue, error) {
	return &queue{version: version, probe: probe, seen: make(map[string]struct{})}, nil
}
func (q *queue) busy() bool {
	q.mu.Lock()
	defer q.mu.Unlock()
	return q.cancel != nil
}
func (q *queue) history(owner int64) []Summary {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.current == nil || q.current.owner != owner {
		return []Summary{}
	}
	return []Summary{q.current.Summary}
}
func (q *queue) get(owner int64, id string) (*Run, error) {
	q.mu.Lock()
	defer q.mu.Unlock()
	r := q.current
	if r == nil || r.owner != owner || r.ID != id {
		return nil, errAbsent
	}
	// Workers can finish while the HTTP handler serializes this snapshot.
	result := r.Run
	result.Rows = append([]Row(nil), r.Rows...)
	result.Questions = append([]Question(nil), r.Questions...)
	return &result, nil
}
func (q *queue) stopRun(owner int64, id string) error {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.current == nil || q.current.ID != id || q.current.owner != owner {
		return errAbsent
	}
	if q.cancel != nil {
		q.cancel()
	}
	return nil
}
func (q *queue) close() {
	q.mu.Lock()
	q.stopping = true
	if q.cancel != nil {
		q.cancel()
	}
	q.mu.Unlock()
	q.wg.Wait()
	q.mu.Lock()
	q.current = nil
	q.seen = nil
	q.mu.Unlock()
}
func finishRows(r *ownedRun, state string) {
	r.State = state
	r.End = nowStamp()
	for i := range r.Rows {
		if nonterminal(r.Rows[i].Status) {
			r.Rows[i].Status = state
			r.Rows[i].Ended = r.End
		}
	}
}
func sameSubmission(r *Run, req RunRequest) bool {
	if req.Parallel != r.Parallel || len(req.Questions) != len(r.Questions) || len(r.Rows) != len(req.Questions)*len(req.Accounts) {
		return false
	}
	for i, q := range r.Questions {
		if q.ID != req.Questions[i] {
			return false
		}
	}
	for i, id := range req.Accounts {
		if r.Rows[i*len(r.Questions)].AccountID != id {
			return false
		}
	}
	return true
}
func (q *queue) knownLocked(owner int64, req RunRequest) (bool, error) {
	if _, ok := q.seen[req.ID]; !ok {
		return false, nil
	}
	r := q.current
	if r == nil || r.ID != req.ID || r.owner != owner {
		return true, errAbsent
	}
	if r.epoch != req.Epoch || !sameSubmission(&r.Run, req) {
		return true, errors.New("提交编号已用于不同的检测请求")
	}
	return true, nil
}
func (q *queue) known(owner int64, req RunRequest) (bool, error) {
	q.mu.Lock()
	defer q.mu.Unlock()
	return q.knownLocked(owner, req)
}
func (q *queue) start(parent context.Context, owner int64, jwt string, req RunRequest, accounts map[int64]AccountView, qs []Question) error {
	q.mu.Lock()
	defer q.mu.Unlock()
	if ok, e := q.knownLocked(owner, req); ok {
		return e
	}
	if q.stopping {
		return errors.New("检测服务正在关闭")
	}
	if len(q.seen) >= maxSeenSubmissions {
		return errors.New("本次启动已经达到批次数量上限，请在服务重新启动后重试")
	}
	if q.cancel != nil {
		return errors.New("已有一批检测在运行，请等待完成")
	}
	if !uuidFormat.MatchString(req.ID) || len(qs) == 0 || len(qs) > 3 || len(req.Accounts) == 0 || len(req.Accounts) > 24 || req.Parallel < 1 || req.Parallel > 2 {
		return errors.New("检测规模无效")
	}
	r := &ownedRun{owner: owner, epoch: req.Epoch, Run: Run{Summary: Summary{req.ID, nowStamp(), "queued"}, BankVersion: q.version, Parallel: req.Parallel, Questions: append([]Question(nil), qs...)}}
	for _, id := range req.Accounts {
		a, ok := accounts[id]
		if !ok {
			return errors.New("账号列表已变化，请刷新后再提交")
		}
		for _, question := range qs {
			r.Rows = append(r.Rows, Row{AccountID: id, Name: a.Name, QuestionID: question.ID, Status: "queued"})
		}
	}
	ctx, cancel := context.WithCancel(parent)
	// Publish only after all validation succeeds. Retired IDs stay consumed.
	q.seen[req.ID] = struct{}{}
	q.current = r
	q.cancel = cancel
	q.wg.Add(1)
	go q.execute(ctx, jwt, r)
	return nil
}
func (q *queue) execute(ctx context.Context, jwt string, r *ownedRun) {
	defer q.wg.Done()
	jobs := make(chan int)
	var workers sync.WaitGroup
	for w := 0; w < r.Parallel; w++ {
		workers.Add(1)
		go func() {
			defer workers.Done()
			for a := range jobs {
				for k, question := range r.Questions {
					if !q.doRow(ctx, jwt, r, a*len(r.Questions)+k, question) {
						break
					}
				}
			}
		}()
	}
send:
	for a := 0; a < len(r.Rows)/len(r.Questions); a++ {
		select {
		case jobs <- a:
		case <-ctx.Done():
			break send
		}
	}
	close(jobs)
	workers.Wait()
	q.mu.Lock()
	defer q.mu.Unlock()
	state := "done"
	if q.stopping {
		state = "interrupted"
	} else if ctx.Err() != nil {
		state = "cancelled"
	}
	finishRows(r, state)
	q.cancel()
	q.cancel = nil
}
func (q *queue) doRow(ctx context.Context, jwt string, r *ownedRun, i int, question Question) bool {
	q.mu.Lock()
	if ctx.Err() != nil {
		q.mu.Unlock()
		return false
	}
	row := &r.Rows[i]
	row.Status = "running"
	row.Started = nowStamp()
	row.RequestID = randomID()
	r.State = "running"
	q.mu.Unlock()
	deadline, cancel := context.WithTimeout(ctx, 120*time.Second)
	answer, err := q.probe(deadline, jwt, row.AccountID, question.Prompt)
	expired := errors.Is(deadline.Err(), context.DeadlineExceeded)
	cancel()
	q.mu.Lock()
	defer q.mu.Unlock()
	row.Ended = nowStamp()
	switch {
	case ctx.Err() != nil:
		row.Status = "cancelled"
		if q.stopping {
			row.Status = "interrupted"
		}
	case expired:
		row.Status = "failed"
		row.Code = "TIMEOUT"
		row.Error = "单题已超时"
	case err != nil:
		row.Status = "failed"
		row.Error = "上游请求失败"
		var ge *GateError
		if errors.As(err, &ge) {
			row.Code = ge.Code
			row.Error = ge.Text
		}
	case answer.Text == "" || len(answer.Text) > maxRowsText:
		row.Status = "failed"
		row.Error = "回答为空或超出本题长度上限"
	default:
		row.Status = "done"
		row.Text = answer.Text
		row.Model = answer.Model
	}
	// A refusal or an incomplete answer belongs to this question. Keep trying the other selected questions.
	if row.Status == "failed" && stopsAccount(row.Code) {
		for j := i + 1; j < len(r.Rows) && r.Rows[j].AccountID == row.AccountID; j++ {
			r.Rows[j].Status = "failed"
			r.Rows[j].Code = row.Code
			r.Rows[j].Error = "同账号上一题失败，已跳过：" + row.Error
			r.Rows[j].Ended = row.Ended
		}
		return false
	}
	return ctx.Err() == nil
}

func stopsAccount(code string) bool {
	switch code {
	case "", "TIMEOUT", "UPSTREAM_REFUSAL", "UPSTREAM_INCOMPLETE", "UPSTREAM_SIZE", "UPSTREAM_FORMAT":
		return false
	default:
		return true
	}
}
