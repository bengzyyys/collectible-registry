package registry

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
	"syscall"
	"time"
)

const (
	manifestFileName = "registry.manifest"
	logFileName      = "registry.log"
	lockFileName     = "registry.lock"
	recordVersion    = 1
)

// manifest 是登记册的格式清单，创建时原子写入。
type manifest struct {
	Format    int       `json:"format"`
	CreatedAt time.Time `json:"created_at"`
}

// envelope 是日志的外层封装，record 为记录原文，hash 为其 SHA-256 校验和。
type envelope struct {
	Version int             `json:"v"`
	Hash    string          `json:"hash"`
	Record  json.RawMessage `json:"record"`
}

// record 是追加写入日志的一条请求记录：包含幂等键、业务结果与状态事件。
// 一条记录原子地包含结果与事件，重放时要么全部生效，要么（尾部撕裂）整体丢弃，
// 不会出现“藏品已换人却没有对应历史”的中间状态。
type record struct {
	Kind        string          `json:"kind"`
	Operator    string          `json:"operator"`
	RequestNo   string          `json:"request_no"`
	Fingerprint string          `json:"fingerprint"`
	OK          bool            `json:"ok"`
	Code        string          `json:"code,omitempty"`
	Message     string          `json:"message,omitempty"`
	Result      json.RawMessage `json:"result,omitempty"`
	Event       *event          `json:"event,omitempty"`
}

// event 是状态变更事件，重放时据此重建内存状态。
type event struct {
	Type     string            `json:"type"`
	ID       string            `json:"id,omitempty"`
	SeriesID string            `json:"series_id,omitempty"`
	BatchNo  string            `json:"batch_no,omitempty"`
	Metadata map[string]string `json:"metadata,omitempty"`
	Creator  string            `json:"creator,omitempty"`
	From     string            `json:"from,omitempty"`
	To       string            `json:"to,omitempty"`
	Holder   string            `json:"holder,omitempty"`
	Version  int               `json:"version,omitempty"`
	Operator string            `json:"operator,omitempty"`
	Reason   string            `json:"reason,omitempty"`
	At       time.Time         `json:"at"`
}

// state 是重放日志得到的内存状态。
type state struct {
	accounts  map[string]*Account
	series    map[string]*Series
	artifacts map[string]*Artifact
	history   map[string][]HistoryEntry
	requests  map[string]*record
}

func newState() *state {
	return &state{
		accounts:  make(map[string]*Account),
		series:    make(map[string]*Series),
		artifacts: make(map[string]*Artifact),
		history:   make(map[string][]HistoryEntry),
		requests:  make(map[string]*record),
	}
}

// applyEvent 将一条状态事件应用到内存状态。
func (s *state) applyEvent(e *event) {
	switch e.Type {
	case "account_register":
		s.accounts[e.ID] = &Account{ID: e.ID, Active: true, CreatedAt: e.At}
	case "account_deactivate":
		if a := s.accounts[e.ID]; a != nil {
			a.Active = false
			t := e.At
			a.DeactivatedAt = &t
		}
	case "series_create":
		s.series[e.ID] = &Series{
			ID:        e.ID,
			CreatorID: e.Creator,
			Metadata:  cloneMap(e.Metadata),
			Sealed:    false,
			CreatedAt: e.At,
		}
	case "series_seal":
		if s := s.series[e.ID]; s != nil {
			s.Sealed = true
			t := e.At
			s.SealedAt = &t
		}
	case "artifact_issue":
		s.artifacts[e.ID] = &Artifact{
			ID:          e.ID,
			SeriesID:    e.SeriesID,
			BatchNo:     e.BatchNo,
			Metadata:    cloneMap(e.Metadata),
			Holder:      e.Holder,
			Version:     1,
			IssuedAt:    e.At,
			IssuedBy:    e.Operator,
			IssueReason: e.Reason,
		}
		s.history[e.ID] = []HistoryEntry{{
			ArtifactID: e.ID,
			Seq:        1,
			Op:         "issue",
			Operator:   e.Operator,
			Reason:     e.Reason,
			From:       "",
			To:         e.Holder,
			Version:    1,
			At:         e.At,
		}}
	case "artifact_transfer":
		a := s.artifacts[e.ID]
		if a == nil {
			return
		}
		a.Holder = e.To
		a.Version = e.Version
		h := s.history[e.ID]
		s.history[e.ID] = append(h, HistoryEntry{
			ArtifactID: e.ID,
			Seq:        len(h) + 1,
			Op:         "transfer",
			Operator:   e.Operator,
			Reason:     e.Reason,
			From:       e.From,
			To:         e.To,
			Version:    e.Version,
			At:         e.At,
		})
	}
}

// Registry 是一个本地登记册实例。所有写操作先追加日志并 fsync，再更新内存状态；
// 配合进程级文件锁与实例级互斥，并发操作满足完整的先后次序。
type Registry struct {
	dir     string
	mu      sync.Mutex
	lockFd  *os.File
	logFd   *os.File
	logSize int64
	closed  bool
	failed  bool
	state   *state
}

var (
	errClosed = &Error{Code: ErrClosed, Message: "登记册已关闭"}
	errFailed = &Error{Code: ErrIO, Message: "登记册因写入失败已不可用，请重新打开"}
)

// Create 在 dir 处创建一个全新的登记册。目录必须不存在或为空；
// 已有数据时返回错误，不会覆盖。
func Create(dir string) (*Registry, error) {
	if dir == "" {
		return nil, &Error{Code: ErrInvalidRequest, Message: "数据目录不能为空"}
	}
	abs, err := filepath.Abs(dir)
	if err != nil {
		return nil, &Error{Code: ErrIO, Message: "无法解析数据目录: " + err.Error()}
	}
	if err := os.MkdirAll(abs, 0o755); err != nil {
		return nil, &Error{Code: ErrIO, Message: "无法创建数据目录: " + err.Error()}
	}
	entries, err := os.ReadDir(abs)
	if err != nil {
		return nil, &Error{Code: ErrIO, Message: "无法读取数据目录: " + err.Error()}
	}
	if len(entries) > 0 {
		return nil, &Error{Code: ErrAlreadyExists, Message: "登记册目录已存在且非空: " + abs}
	}

	r := &Registry{dir: abs, state: newState()}
	if err := r.initFiles(); err != nil {
		r.closeFds()
		return nil, err
	}
	return r, nil
}

// Open 打开 dir 处已有的登记册并重放日志。
// 数据缺失、无法读取或校验失败时明确报错，不会被当成空登记册。
func Open(dir string) (*Registry, error) {
	if dir == "" {
		return nil, &Error{Code: ErrInvalidRequest, Message: "数据目录不能为空"}
	}
	abs, err := filepath.Abs(dir)
	if err != nil {
		return nil, &Error{Code: ErrIO, Message: "无法解析数据目录: " + err.Error()}
	}
	info, err := os.Stat(abs)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, &Error{Code: ErrNotFound, Message: "登记册不存在: " + abs}
		}
		return nil, &Error{Code: ErrIO, Message: "无法访问数据目录: " + err.Error()}
	}
	if !info.IsDir() {
		return nil, &Error{Code: ErrInvalidRequest, Message: "路径不是目录: " + abs}
	}

	manifestPath := filepath.Join(abs, manifestFileName)
	mb, err := os.ReadFile(manifestPath)
	if err != nil {
		return nil, &Error{Code: ErrCorruptData, Message: "登记册清单无法读取: " + err.Error()}
	}
	var mf manifest
	if err := json.Unmarshal(mb, &mf); err != nil || mf.Format != recordVersion {
		return nil, &Error{Code: ErrCorruptData, Message: "登记册清单格式无法识别"}
	}

	r := &Registry{dir: abs, state: newState()}
	if err := r.acquireLock(); err != nil {
		return nil, err
	}

	logPath := filepath.Join(abs, logFileName)
	lf, err := os.OpenFile(logPath, os.O_RDWR|os.O_APPEND, 0o600)
	if err != nil {
		r.closeFds()
		return nil, &Error{Code: ErrCorruptData, Message: "登记册日志无法打开: " + err.Error()}
	}
	r.logFd = lf

	if err := r.replay(); err != nil {
		r.closeFds()
		return nil, err
	}
	return r, nil
}

// initFiles 创建锁文件、清单与空日志。
func (r *Registry) initFiles() error {
	if err := r.acquireLock(); err != nil {
		return err
	}
	mf := manifest{Format: recordVersion, CreatedAt: time.Now().UTC()}
	mb, err := json.Marshal(mf)
	if err != nil {
		return err
	}
	if err := writeFileAtomic(r.dir, manifestFileName, mb); err != nil {
		return &Error{Code: ErrIO, Message: "无法写入登记册清单: " + err.Error()}
	}
	logPath := filepath.Join(r.dir, logFileName)
	lf, err := os.OpenFile(logPath, os.O_RDWR|os.O_CREATE|os.O_APPEND, 0o600)
	if err != nil {
		return &Error{Code: ErrIO, Message: "无法创建登记册日志: " + err.Error()}
	}
	r.logFd = lf
	if err := r.logFd.Sync(); err != nil {
		return &Error{Code: ErrIO, Message: "无法同步登记册日志: " + err.Error()}
	}
	return nil
}

// acquireLock 获取跨进程文件锁，登记册生命周期内独占。
func (r *Registry) acquireLock() error {
	lockPath := filepath.Join(r.dir, lockFileName)
	lf, err := os.OpenFile(lockPath, os.O_RDWR|os.O_CREATE, 0o644)
	if err != nil {
		return &Error{Code: ErrIO, Message: "无法打开锁文件: " + err.Error()}
	}
	if err := syscall.Flock(int(lf.Fd()), syscall.LOCK_EX); err != nil {
		lf.Close()
		return &Error{Code: ErrIO, Message: "无法获取登记册锁: " + err.Error()}
	}
	r.lockFd = lf
	return nil
}

// replay 读取并重放全部日志记录。尾部最后一条记录若不完整（进程在写入途中被终止），
// 视为撕裂写入并截断；中间记录损坏则明确报错。
func (r *Registry) replay() error {
	if _, err := r.logFd.Seek(0, io.SeekStart); err != nil {
		return &Error{Code: ErrIO, Message: "无法读取登记册日志: " + err.Error()}
	}
	data, err := io.ReadAll(r.logFd)
	if err != nil {
		return &Error{Code: ErrIO, Message: "无法读取登记册日志: " + err.Error()}
	}

	offset := 0
	for offset < len(data) {
		nl := bytes.IndexByte(data[offset:], '\n')
		var line []byte
		var next int
		if nl < 0 {
			line = data[offset:]
			next = len(data)
		} else {
			line = data[offset : offset+nl]
			next = offset + nl + 1
		}
		if len(bytes.TrimSpace(line)) == 0 {
			offset = next
			continue
		}
		torn := next == len(data)

		var env envelope
		if err := json.Unmarshal(line, &env); err != nil {
			if torn {
				return r.truncateTail(offset)
			}
			return &Error{Code: ErrCorruptData, Message: "登记册日志损坏: 记录无法解析"}
		}
		sum := sha256.Sum256(env.Record)
		if env.Version != recordVersion || env.Hash != hex.EncodeToString(sum[:]) {
			if torn {
				return r.truncateTail(offset)
			}
			return &Error{Code: ErrCorruptData, Message: "登记册日志损坏: 记录校验失败"}
		}
		var rec record
		if err := json.Unmarshal(env.Record, &rec); err != nil {
			if torn {
				return r.truncateTail(offset)
			}
			return &Error{Code: ErrCorruptData, Message: "登记册日志损坏: 记录内容无法解析"}
		}

		r.state.requests[requestKey(rec.Operator, rec.RequestNo)] = &rec
		if rec.Event != nil {
			r.state.applyEvent(rec.Event)
		}
		offset = next
	}
	r.logSize = int64(len(data))
	return nil
}

// truncateTail 截断不完整的尾部记录并同步。
func (r *Registry) truncateTail(offset int) error {
	if err := r.logFd.Truncate(int64(offset)); err != nil {
		return &Error{Code: ErrIO, Message: "恢复登记册日志失败: " + err.Error()}
	}
	if err := r.logFd.Sync(); err != nil {
		return &Error{Code: ErrIO, Message: "同步登记册日志失败: " + err.Error()}
	}
	r.logSize = int64(offset)
	return nil
}

// appendRecord 原子地追加一条请求记录：写入、fsync 后才算成功。
// 写入或同步失败时回滚到写入前的长度，并将登记册标记为不可用。
func (r *Registry) appendRecord(rec *record) error {
	if r.failed {
		return errFailed
	}
	recJSON, err := json.Marshal(rec)
	if err != nil {
		return &Error{Code: ErrIO, Message: "无法编码请求记录: " + err.Error()}
	}
	sum := sha256.Sum256(recJSON)
	env := envelope{Version: recordVersion, Hash: hex.EncodeToString(sum[:]), Record: recJSON}
	line, err := json.Marshal(env)
	if err != nil {
		return &Error{Code: ErrIO, Message: "无法编码日志记录: " + err.Error()}
	}
	line = append(line, '\n')

	before := r.logSize
	if _, err := r.logFd.Write(line); err != nil {
		r.rollback(before)
		return &Error{Code: ErrIO, Message: "写入登记册日志失败: " + err.Error()}
	}
	if err := r.logFd.Sync(); err != nil {
		r.rollback(before)
		return &Error{Code: ErrIO, Message: "同步登记册日志失败: " + err.Error()}
	}
	r.logSize = before + int64(len(line))
	return nil
}

// rollback 将日志回滚到指定长度；回滚本身失败则标记登记册不可用。
func (r *Registry) rollback(size int64) {
	if err := r.logFd.Truncate(size); err != nil {
		r.failed = true
	}
}

// Close 正常关闭登记册：同步日志、释放文件锁。
func (r *Registry) Close() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return nil
	}
	r.closed = true
	var syncErr error
	if r.logFd != nil {
		if err := r.logFd.Sync(); err != nil && syncErr == nil {
			syncErr = err
		}
	}
	r.closeFds()
	if syncErr != nil {
		return &Error{Code: ErrIO, Message: "关闭时同步日志失败: " + syncErr.Error()}
	}
	return nil
}

func (r *Registry) closeFds() {
	if r.logFd != nil {
		_ = r.logFd.Close()
		r.logFd = nil
	}
	if r.lockFd != nil {
		_ = syscall.Flock(int(r.lockFd.Fd()), syscall.LOCK_UN)
		_ = r.lockFd.Close()
		r.lockFd = nil
	}
}

// write 是所有写操作的统一入口：必填校验、幂等去重、业务执行、持久化。
// 业务拒绝也会被持久化，重试时返回首次结果；业务参数改变则报请求号冲突。
func write[T any](r *Registry, kind, operator, requestNo, reason string, biz any, produce func() (*event, T, error)) (T, error) {
	var zero T
	if operator == "" || requestNo == "" || reason == "" {
		return zero, &Error{Code: ErrInvalidRequest, Message: "操作者、请求号和原因均为必填"}
	}
	fp := fingerprint(biz)
	key := requestKey(operator, requestNo)

	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return zero, errClosed
	}
	if r.failed {
		return zero, errFailed
	}

	if prev, ok := r.state.requests[key]; ok {
		if prev.Fingerprint != fp {
			return zero, &Error{Code: ErrRequestConflict, Message: fmt.Sprintf("操作者 %q 的请求号 %q 已用于不同业务参数", operator, requestNo)}
		}
		if !prev.OK {
			return zero, &Error{Code: ErrorCode(prev.Code), Message: prev.Message}
		}
		var res T
		if err := json.Unmarshal(prev.Result, &res); err != nil {
			return zero, &Error{Code: ErrCorruptData, Message: "已保存的请求结果无法解析"}
		}
		return res, nil
	}

	ev, result, berr := produce()
	rec := &record{
		Kind:        kind,
		Operator:    operator,
		RequestNo:   requestNo,
		Fingerprint: fp,
		OK:          berr == nil,
	}
	if berr != nil {
		var be *Error
		if errors.As(berr, &be) {
			rec.Code = string(be.Code)
			rec.Message = be.Message
		} else {
			rec.Code = string(ErrIO)
			rec.Message = berr.Error()
		}
	} else {
		resJSON, err := json.Marshal(result)
		if err != nil {
			return zero, &Error{Code: ErrIO, Message: "无法编码请求结果: " + err.Error()}
		}
		rec.Result = resJSON
		rec.Event = ev
	}

	if err := r.appendRecord(rec); err != nil {
		return zero, err
	}
	r.state.requests[key] = rec
	if ev != nil {
		r.state.applyEvent(ev)
	}
	if berr != nil {
		return zero, berr
	}
	return result, nil
}

// ---------- 账户 ----------

// RegisterAccount 登记一个可用账户。账户编号唯一，重复登记返回错误。
func (r *Registry) RegisterAccount(req RegisterAccountRequest) (*Account, error) {
	biz := registerBiz{AccountID: req.AccountID, Reason: req.Reason}
	return write(r, "account_register", req.Operator, req.RequestNo, req.Reason, biz, func() (*event, *Account, error) {
		if req.AccountID == "" {
			return nil, nil, &Error{Code: ErrInvalidRequest, Message: "账户编号必填"}
		}
		if _, ok := r.state.accounts[req.AccountID]; ok {
			return nil, nil, &Error{Code: ErrAlreadyExists, Message: fmt.Sprintf("账户 %q 已存在", req.AccountID)}
		}
		now := time.Now().UTC()
		ev := &event{Type: "account_register", ID: req.AccountID, At: now}
		acc := &Account{ID: req.AccountID, Active: true, CreatedAt: now}
		return ev, acc, nil
	})
}

// DeactivateAccount 停用一个已有账户。停用后账户仍可查询，但不能发行、发起或接收转让。
func (r *Registry) DeactivateAccount(req DeactivateAccountRequest) (*Account, error) {
	biz := deactivateBiz{AccountID: req.AccountID, Reason: req.Reason}
	return write(r, "account_deactivate", req.Operator, req.RequestNo, req.Reason, biz, func() (*event, *Account, error) {
		if req.AccountID == "" {
			return nil, nil, &Error{Code: ErrInvalidRequest, Message: "账户编号必填"}
		}
		acc, ok := r.state.accounts[req.AccountID]
		if !ok {
			return nil, nil, &Error{Code: ErrNotFound, Message: fmt.Sprintf("账户 %q 不存在", req.AccountID)}
		}
		if !acc.Active {
			return nil, nil, &Error{Code: ErrConflict, Message: fmt.Sprintf("账户 %q 已停用", req.AccountID)}
		}
		now := time.Now().UTC()
		ev := &event{Type: "account_deactivate", ID: req.AccountID, At: now}
		updated := *acc
		updated.Active = false
		t := now
		updated.DeactivatedAt = &t
		return ev, &updated, nil
	})
}

// GetAccount 查询账户；账户不存在时明确返回不存在。
func (r *Registry) GetAccount(id string) (*Account, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return nil, errClosed
	}
	acc, ok := r.state.accounts[id]
	if !ok {
		return nil, &Error{Code: ErrNotFound, Message: fmt.Sprintf("账户 %q 不存在", id)}
	}
	cp := *acc
	return &cp, nil
}

// ---------- 系列 ----------

// CreateSeries 创建系列并登记创建账户与文字元数据。仅创建账户可以发行或封存。
func (r *Registry) CreateSeries(req CreateSeriesRequest) (*Series, error) {
	biz := createSeriesBiz{SeriesID: req.SeriesID, CreatorID: req.CreatorID, Metadata: req.Metadata, Reason: req.Reason}
	return write(r, "series_create", req.Operator, req.RequestNo, req.Reason, biz, func() (*event, *Series, error) {
		if req.SeriesID == "" {
			return nil, nil, &Error{Code: ErrInvalidRequest, Message: "系列编号必填"}
		}
		if req.CreatorID == "" {
			return nil, nil, &Error{Code: ErrInvalidRequest, Message: "创建账户编号必填"}
		}
		if _, ok := r.state.series[req.SeriesID]; ok {
			return nil, nil, &Error{Code: ErrAlreadyExists, Message: fmt.Sprintf("系列 %q 已存在", req.SeriesID)}
		}
		creator, ok := r.state.accounts[req.CreatorID]
		if !ok {
			return nil, nil, &Error{Code: ErrNotFound, Message: fmt.Sprintf("创建账户 %q 不存在", req.CreatorID)}
		}
		if !creator.Active {
			return nil, nil, &Error{Code: ErrInactive, Message: fmt.Sprintf("创建账户 %q 已停用，不能创建系列", req.CreatorID)}
		}
		now := time.Now().UTC()
		meta := cloneMap(req.Metadata)
		ev := &event{Type: "series_create", ID: req.SeriesID, Creator: req.CreatorID, Metadata: meta, At: now}
		s := &Series{ID: req.SeriesID, CreatorID: req.CreatorID, Metadata: meta, Sealed: false, CreatedAt: now}
		return ev, s, nil
	})
}

// SealSeries 封存系列。封存不可撤销；封存后不能继续发行，已发行藏品仍可转让。
func (r *Registry) SealSeries(req SealSeriesRequest) (*Series, error) {
	biz := sealBiz{SeriesID: req.SeriesID, Reason: req.Reason}
	return write(r, "series_seal", req.Operator, req.RequestNo, req.Reason, biz, func() (*event, *Series, error) {
		if req.SeriesID == "" {
			return nil, nil, &Error{Code: ErrInvalidRequest, Message: "系列编号必填"}
		}
		s, ok := r.state.series[req.SeriesID]
		if !ok {
			return nil, nil, &Error{Code: ErrNotFound, Message: fmt.Sprintf("系列 %q 不存在", req.SeriesID)}
		}
		if s.CreatorID != req.Operator {
			return nil, nil, &Error{Code: ErrForbidden, Message: fmt.Sprintf("仅系列 %q 的创建账户可以封存", req.SeriesID)}
		}
		if s.Sealed {
			return nil, nil, &Error{Code: ErrConflict, Message: fmt.Sprintf("系列 %q 已封存", req.SeriesID)}
		}
		now := time.Now().UTC()
		ev := &event{Type: "series_seal", ID: req.SeriesID, At: now}
		updated := *s
		updated.Sealed = true
		t := now
		updated.SealedAt = &t
		return ev, &updated, nil
	})
}

// GetSeries 查询系列；系列不存在时明确返回不存在。
func (r *Registry) GetSeries(id string) (*Series, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return nil, errClosed
	}
	s, ok := r.state.series[id]
	if !ok {
		return nil, &Error{Code: ErrNotFound, Message: fmt.Sprintf("系列 %q 不存在", id)}
	}
	cp := *s
	cp.Metadata = cloneMap(s.Metadata)
	return &cp, nil
}

// ---------- 发行 ----------

// Issue 发行藏品。发行时提供唯一藏品编号、所属系列、批次号、元数据及初始持有人；
// 初始持有人必须是已登记且可用的账户。同一编号不能产生第二件藏品；
// 系列封存后不能继续发行。
func (r *Registry) Issue(req IssueRequest) (*Artifact, error) {
	biz := issueBiz{
		ArtifactID:    req.ArtifactID,
		SeriesID:      req.SeriesID,
		BatchNo:       req.BatchNo,
		Metadata:      req.Metadata,
		InitialHolder: req.InitialHolder,
		Reason:        req.Reason,
	}
	return write(r, "artifact_issue", req.Operator, req.RequestNo, req.Reason, biz, func() (*event, *Artifact, error) {
		if req.ArtifactID == "" {
			return nil, nil, &Error{Code: ErrInvalidRequest, Message: "藏品编号必填"}
		}
		if req.SeriesID == "" {
			return nil, nil, &Error{Code: ErrInvalidRequest, Message: "系列编号必填"}
		}
		if req.BatchNo == "" {
			return nil, nil, &Error{Code: ErrInvalidRequest, Message: "批次号必填"}
		}
		if req.InitialHolder == "" {
			return nil, nil, &Error{Code: ErrInvalidRequest, Message: "初始持有人必填"}
		}
		s, ok := r.state.series[req.SeriesID]
		if !ok {
			return nil, nil, &Error{Code: ErrNotFound, Message: fmt.Sprintf("系列 %q 不存在", req.SeriesID)}
		}
		if s.Sealed {
			return nil, nil, &Error{Code: ErrSealed, Message: fmt.Sprintf("系列 %q 已封存，不能继续发行", req.SeriesID)}
		}
		if s.CreatorID != req.Operator {
			return nil, nil, &Error{Code: ErrForbidden, Message: fmt.Sprintf("仅系列 %q 的创建账户可以发行", req.SeriesID)}
		}
		op, ok := r.state.accounts[req.Operator]
		if !ok {
			return nil, nil, &Error{Code: ErrNotFound, Message: fmt.Sprintf("操作者账户 %q 不存在", req.Operator)}
		}
		if !op.Active {
			return nil, nil, &Error{Code: ErrInactive, Message: fmt.Sprintf("操作者账户 %q 已停用，不能发行", req.Operator)}
		}
		if _, dup := r.state.artifacts[req.ArtifactID]; dup {
			return nil, nil, &Error{Code: ErrAlreadyExists, Message: fmt.Sprintf("藏品 %q 已存在，不能重复发行", req.ArtifactID)}
		}
		holder, ok := r.state.accounts[req.InitialHolder]
		if !ok {
			return nil, nil, &Error{Code: ErrNotFound, Message: fmt.Sprintf("初始持有人账户 %q 不存在", req.InitialHolder)}
		}
		if !holder.Active {
			return nil, nil, &Error{Code: ErrInactive, Message: fmt.Sprintf("初始持有人账户 %q 已停用，不能持有藏品", req.InitialHolder)}
		}
		now := time.Now().UTC()
		meta := cloneMap(req.Metadata)
		ev := &event{
			Type:     "artifact_issue",
			ID:       req.ArtifactID,
			SeriesID: req.SeriesID,
			BatchNo:  req.BatchNo,
			Metadata: meta,
			Holder:   req.InitialHolder,
			Version:  1,
			Operator: req.Operator,
			Reason:   req.Reason,
			At:       now,
		}
		art := &Artifact{
			ID:          req.ArtifactID,
			SeriesID:    req.SeriesID,
			BatchNo:     req.BatchNo,
			Metadata:    meta,
			Holder:      req.InitialHolder,
			Version:     1,
			IssuedAt:    now,
			IssuedBy:    req.Operator,
			IssueReason: req.Reason,
		}
		return ev, art, nil
	})
}

// ---------- 转让 ----------

// Transfer 发起转让。转让由当前持有人发起，接收人必须已登记、可用且不同于当前持有人；
// 请求必须给出期望版本，持有人或版本不符时明确拒绝。发行后的持有版本为 1，
// 每次成功转让版本加 1。
func (r *Registry) Transfer(req TransferRequest) (*Holding, error) {
	biz := transferBiz{
		ArtifactID:      req.ArtifactID,
		From:            req.From,
		To:              req.To,
		ExpectedVersion: req.ExpectedVersion,
		Reason:          req.Reason,
	}
	return write(r, "artifact_transfer", req.Operator, req.RequestNo, req.Reason, biz, func() (*event, *Holding, error) {
		if req.ArtifactID == "" {
			return nil, nil, &Error{Code: ErrInvalidRequest, Message: "藏品编号必填"}
		}
		if req.From == "" {
			return nil, nil, &Error{Code: ErrInvalidRequest, Message: "当前持有人必填"}
		}
		if req.To == "" {
			return nil, nil, &Error{Code: ErrInvalidRequest, Message: "接收人必填"}
		}
		if req.ExpectedVersion < 1 {
			return nil, nil, &Error{Code: ErrInvalidRequest, Message: "期望版本必须大于 0"}
		}
		if req.Operator != req.From {
			return nil, nil, &Error{Code: ErrForbidden, Message: "转让必须由当前持有人发起"}
		}
		op, ok := r.state.accounts[req.Operator]
		if !ok {
			return nil, nil, &Error{Code: ErrNotFound, Message: fmt.Sprintf("操作者账户 %q 不存在", req.Operator)}
		}
		if !op.Active {
			return nil, nil, &Error{Code: ErrInactive, Message: fmt.Sprintf("操作者账户 %q 已停用，不能发起转让", req.Operator)}
		}
		a, ok := r.state.artifacts[req.ArtifactID]
		if !ok {
			return nil, nil, &Error{Code: ErrNotFound, Message: fmt.Sprintf("藏品 %q 不存在", req.ArtifactID)}
		}
		if a.Holder != req.From {
			return nil, nil, &Error{Code: ErrHolderMismatch, Message: fmt.Sprintf("藏品 %q 当前持有人为 %q，与请求的 %q 不符", req.ArtifactID, a.Holder, req.From)}
		}
		if a.Version != req.ExpectedVersion {
			return nil, nil, &Error{Code: ErrVersionMismatch, Message: fmt.Sprintf("藏品 %q 当前版本为 %d，与期望版本 %d 不符", req.ArtifactID, a.Version, req.ExpectedVersion)}
		}
		if req.From == req.To {
			return nil, nil, &Error{Code: ErrConflict, Message: "接收人必须不同于当前持有人"}
		}
		toAcc, ok := r.state.accounts[req.To]
		if !ok {
			return nil, nil, &Error{Code: ErrNotFound, Message: fmt.Sprintf("接收人账户 %q 不存在", req.To)}
		}
		if !toAcc.Active {
			return nil, nil, &Error{Code: ErrInactive, Message: fmt.Sprintf("接收人账户 %q 已停用，不能接收转让", req.To)}
		}
		newVersion := a.Version + 1
		now := time.Now().UTC()
		ev := &event{
			Type:     "artifact_transfer",
			ID:       req.ArtifactID,
			From:     req.From,
			To:       req.To,
			Version:  newVersion,
			Operator: req.Operator,
			Reason:   req.Reason,
			At:       now,
		}
		h := &Holding{ArtifactID: req.ArtifactID, Holder: req.To, Version: newVersion}
		return ev, h, nil
	})
}

// ---------- 查询 ----------

// GetArtifact 查询藏品当前状态；藏品不存在时明确返回不存在。
func (r *Registry) GetArtifact(id string) (*Artifact, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return nil, errClosed
	}
	a, ok := r.state.artifacts[id]
	if !ok {
		return nil, &Error{Code: ErrNotFound, Message: fmt.Sprintf("藏品 %q 不存在", id)}
	}
	cp := *a
	cp.Metadata = cloneMap(a.Metadata)
	return &cp, nil
}

// GetHolding 查询藏品当前持有；藏品不存在时明确返回不存在。
func (r *Registry) GetHolding(artifactID string) (*Holding, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return nil, errClosed
	}
	a, ok := r.state.artifacts[artifactID]
	if !ok {
		return nil, &Error{Code: ErrNotFound, Message: fmt.Sprintf("藏品 %q 不存在", artifactID)}
	}
	return &Holding{ArtifactID: a.ID, Holder: a.Holder, Version: a.Version}, nil
}

// GetHistory 按顺序返回藏品的发行及历次成功转让记录；藏品不存在时明确返回不存在。
// 重复提交的请求不增加历史。
func (r *Registry) GetHistory(artifactID string) ([]HistoryEntry, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return nil, errClosed
	}
	if _, ok := r.state.artifacts[artifactID]; !ok {
		return nil, &Error{Code: ErrNotFound, Message: fmt.Sprintf("藏品 %q 不存在", artifactID)}
	}
	h := r.state.history[artifactID]
	out := make([]HistoryEntry, len(h))
	copy(out, h)
	return out, nil
}

// ---------- 内部工具 ----------

func cloneMap(m map[string]string) map[string]string {
	if m == nil {
		return nil
	}
	out := make(map[string]string, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}

func requestKey(operator, requestNo string) string {
	return operator + "\x00" + requestNo
}

func fingerprint(biz any) string {
	b, _ := json.Marshal(biz)
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

func writeFileAtomic(dir, name string, data []byte) error {
	tmp := filepath.Join(dir, name+".tmp")
	final := filepath.Join(dir, name)
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	if err := os.Rename(tmp, final); err != nil {
		return err
	}
	return nil
}

// 业务参数结构体，用于计算幂等指纹（不含操作者与请求号）。
type registerBiz struct {
	AccountID string `json:"account_id"`
	Reason    string `json:"reason"`
}

type deactivateBiz struct {
	AccountID string `json:"account_id"`
	Reason    string `json:"reason"`
}

type createSeriesBiz struct {
	SeriesID  string            `json:"series_id"`
	CreatorID string            `json:"creator_id"`
	Metadata  map[string]string `json:"metadata,omitempty"`
	Reason    string            `json:"reason"`
}

type sealBiz struct {
	SeriesID string `json:"series_id"`
	Reason   string `json:"reason"`
}

type issueBiz struct {
	ArtifactID    string            `json:"artifact_id"`
	SeriesID      string            `json:"series_id"`
	BatchNo       string            `json:"batch_no"`
	Metadata      map[string]string `json:"metadata,omitempty"`
	InitialHolder string            `json:"initial_holder"`
	Reason        string            `json:"reason"`
}

type transferBiz struct {
	ArtifactID      string `json:"artifact_id"`
	From            string `json:"from"`
	To              string `json:"to"`
	ExpectedVersion int    `json:"expected_version"`
	Reason          string `json:"reason"`
}
