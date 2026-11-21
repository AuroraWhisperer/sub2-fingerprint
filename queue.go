package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
)

const maxRowsText = 12 << 10
const historyLimit = 20 << 20

var uuidFormat = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)
var errHistory = errors.New("检测记录无法安全写入。请检查检测器磁盘空间和权限")
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
type diskRun struct {
	Owner int64 `json:"owner"`
	Run
}
type probeFunc func(context.Context, string, int64, string) (ProbeAnswer, error)
type indexEntry struct {
	Summary
	owner int64
	size  int64
}
type queue struct {
	mu           sync.Mutex
	dir          string
	index        map[string]indexEntry
	total        int64
	version      string
	active       *diskRun
	cancel       context.CancelFunc
	stopping     bool
	storageFault bool
	wg           sync.WaitGroup
	probe        probeFunc
}

func nonterminal(s string) bool { return s == "queued" || s == "running" }
func openQueue(dir, version string, probe probeFunc) (*queue, error) {
	q := &queue{dir: dir, version: version, index: make(map[string]indexEntry), probe: probe}
	if err := os.MkdirAll(dir, 0700); err != nil {
		return nil, err
	}
	files, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	for _, f := range files {
		name := f.Name()
		if strings.HasSuffix(name, ".partial") && uuidFormat.MatchString(strings.TrimSuffix(name, ".partial")) && f.Type().IsRegular() {
			if err := os.Remove(filepath.Join(dir, name)); err != nil {
				return nil, err
			}
			continue
		}
		if !f.Type().IsRegular() || !strings.HasSuffix(name, ".json") || !uuidFormat.MatchString(strings.TrimSuffix(name, ".json")) {
			return nil, fmt.Errorf("unrecognized file in private result directory")
		}
		r, e := q.load(strings.TrimSuffix(name, ".json"))
		if e != nil {
			return nil, e
		}
		if r.ID+".json" != name || r.Owner < 1 {
			return nil, errors.New("invalid result owner or id")
		}
		info, e := f.Info()
		if e != nil {
			return nil, e
		}
		q.index[r.ID] = indexEntry{r.Summary, r.Owner, info.Size()}
		q.total += info.Size()
		if nonterminal(r.State) {
			finishRows(r, "interrupted")
			if e := q.save(r); e != nil {
				return nil, e
			}
		}
	}
	return q, nil
}
func (q *queue) load(id string) (*diskRun, error) {
	if !uuidFormat.MatchString(id) {
		return nil, errAbsent
	}
	f, err := os.Open(filepath.Join(q.dir, id+".json"))
	if err != nil {
		return nil, err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil || st.Size() > historyLimit || !st.Mode().IsRegular() {
		return nil, errHistory
	}
	var r diskRun
	err = json.NewDecoder(f).Decode(&r)
	return &r, err
}

// Write a durable intent before dispatch. A recovered intent is marked interrupted and never replayed.
func (q *queue) save(r *diskRun) error {
	raw, err := json.Marshal(r)
	if err != nil {
		return errHistory
	}
	old := q.index[r.ID]
	if q.total-old.size+int64(len(raw)) > historyLimit {
		return errHistory
	}
	p := filepath.Join(q.dir, r.ID)
	tmp := p + ".partial"
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return errHistory
	}
	defer os.Remove(tmp)
	if _, err = f.Write(raw); err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err != nil || closeErr != nil {
		return errHistory
	}
	if err = os.Rename(tmp, p+".json"); err != nil {
		return errHistory
	}
	if err = syncDir(q.dir); err != nil {
		return errHistory
	}
	q.index[r.ID] = indexEntry{r.Summary, r.Owner, int64(len(raw))}
	q.total += int64(len(raw)) - old.size
	return nil
}
func (q *queue) history(owner int64) []Summary {
	q.mu.Lock()
	defer q.mu.Unlock()
	out := make([]Summary, 0)
	for _, e := range q.index {
		if e.owner == owner {
			out = append(out, e.Summary)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].At > out[j].At })
	return out
}
func (q *queue) get(owner int64, id string) (*Run, error) {
	q.mu.Lock()
	defer q.mu.Unlock()
	e, ok := q.index[id]
	if !ok || e.owner != owner {
		return nil, errAbsent
	}
	r, err := q.load(id)
	if err != nil {
		return nil, errHistory
	}
	return &r.Run, nil
}
func (q *queue) stopRun(owner int64, id string) error {
	q.mu.Lock()
	defer q.mu.Unlock()
	e, ok := q.index[id]
	if !ok || e.owner != owner {
		return errAbsent
	}
	if q.active != nil && q.active.ID == id {
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
}
func finishRows(r *diskRun, state string) {
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
	e, ok := q.index[req.ID]
	if !ok {
		return false, nil
	}
	if e.owner != owner {
		return true, errAbsent
	}
	r, err := q.load(req.ID)
	if err != nil {
		return true, errHistory
	}
	if !sameSubmission(&r.Run, req) {
		return true, errors.New("提交编号已用于另一组账号或题目")
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
	if q.stopping || q.storageFault {
		return errHistory
	}
	if q.active != nil {
		return errors.New("已有一批检测在运行，请等待完成")
	}
	if len(qs) == 0 || len(qs) > 3 || len(req.Accounts) == 0 || len(req.Accounts) > 24 || req.Parallel < 1 || req.Parallel > 2 {
		return errors.New("检测规模无效")
	}
	r := &diskRun{Owner: owner, Run: Run{Summary: Summary{req.ID, nowStamp(), "queued"}, BankVersion: q.version, Parallel: req.Parallel, Questions: qs}}
	for _, id := range req.Accounts {
		a, ok := accounts[id]
		if !ok {
			return errors.New("账号列表已变化，请刷新后再提交")
		}
		for _, question := range qs {
			r.Rows = append(r.Rows, Row{AccountID: id, Name: a.Name, QuestionID: question.ID, Status: "queued"})
		}
	}
	if q.total+int64(len(r.Rows)*(6*maxRowsText+2048)+16*1024) > historyLimit {
		return errors.New("结果目录达到容量预算，请先导出并清理旧记录")
	}
	if err := q.save(r); err != nil {
		return err
	}
	ctx, cancel := context.WithCancel(parent)
	q.active = r
	q.cancel = cancel
	q.wg.Add(1)
	go q.execute(ctx, jwt, r)
	return nil
}
func (q *queue) persistLocked(r *diskRun) bool {
	if q.storageFault {
		return false
	}
	if err := q.save(r); err != nil {
		q.storageFault = true
		q.cancel()
		return false
	}
	return true
}
func (q *queue) execute(ctx context.Context, jwt string, r *diskRun) {
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
	if q.stopping || q.storageFault {
		state = "interrupted"
	} else if ctx.Err() != nil {
		state = "cancelled"
	}
	finishRows(r, state)
	q.persistLocked(r)
	q.cancel()
	q.active = nil
	q.cancel = nil
}
func (q *queue) doRow(ctx context.Context, jwt string, r *diskRun, i int, question Question) bool {
	q.mu.Lock()
	if ctx.Err() != nil || q.storageFault {
		q.mu.Unlock()
		return false
	}
	row := &r.Rows[i]
	row.Status = "running"
	row.Started = nowStamp()
	row.RequestID = randomID()
	r.State = "running"
	saved := q.persistLocked(r)
	q.mu.Unlock()
	if !saved {
		return false
	}
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
		q.persistLocked(r)
		return false
	}
	return q.persistLocked(r) && ctx.Err() == nil
}

func stopsAccount(code string) bool {
	switch code {
	case "", "TIMEOUT", "UPSTREAM_REFUSAL", "UPSTREAM_INCOMPLETE", "UPSTREAM_SIZE", "UPSTREAM_FORMAT":
		return false
	default:
		return true
	}
}
