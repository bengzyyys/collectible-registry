package registry

import (
	"encoding/json"
	"fmt"
	"strings"
)

// ---- 整批发行 ----

func (req IssueBatchRequest) validatePresent() error {
	missing := []string{}
	if strings.TrimSpace(req.Operator) == "" {
		missing = append(missing, "operator")
	}
	if strings.TrimSpace(req.Reason) == "" {
		missing = append(missing, "reason")
	}
	if strings.TrimSpace(req.RequestID) == "" {
		missing = append(missing, "request_id")
	}
	if strings.TrimSpace(req.SeriesID) == "" {
		missing = append(missing, "series_id")
	}
	if strings.TrimSpace(req.BatchNo) == "" {
		missing = append(missing, "batch_no")
	}
	if len(missing) > 0 {
		return fmt.Errorf("%w: 整批发行请求缺少必填字段 %s", ErrInvalidArgument, strings.Join(missing, ", "))
	}
	if len(req.Entries) == 0 {
		return fmt.Errorf("%w: 整批发行清单不能为空", ErrInvalidArgument)
	}
	seen := make(map[string]bool, len(req.Entries))
	for i, e := range req.Entries {
		if strings.TrimSpace(e.ItemID) == "" {
			return fmt.Errorf("%w: 清单第 %d 条缺少藏品编号", ErrInvalidArgument, i+1)
		}
		if strings.TrimSpace(e.HolderID) == "" {
			return fmt.Errorf("%w: 清单第 %d 条（藏品 %s）缺少初始持有人", ErrInvalidArgument, i+1, e.ItemID)
		}
		if seen[e.ItemID] {
			return fmt.Errorf("%w: 清单内藏品编号 %s 重复", ErrInvalidArgument, e.ItemID)
		}
		seen[e.ItemID] = true
	}
	return nil
}

func issueBatchParamsSig(req IssueBatchRequest) string {
	// 条目按提交顺序进入签名：改动任一条目内容或条目顺序都会得到不同
	// 签名，按请求号冲突拒绝。
	type entry struct {
		ItemID   string `json:"item_id"`
		Metadata string `json:"metadata"`
		HolderID string `json:"holder_id"`
	}
	entries := make([]entry, 0, len(req.Entries))
	for _, e := range req.Entries {
		entries = append(entries, entry{e.ItemID, e.Metadata, e.HolderID})
	}
	b, _ := json.Marshal(struct {
		Kind     string  `json:"kind"`
		SeriesID string  `json:"series_id"`
		BatchNo  string  `json:"batch_no"`
		Reason   string  `json:"reason"`
		Entries  []entry `json:"entries"`
	}{"issue_batch", req.SeriesID, req.BatchNo, req.Reason, entries})
	return string(b)
}

// IssueBatch 由系列创建账户一次提交同一系列、同一批次号下的多件藏品，
// 整批共用操作者、原因与请求号。成功后按提交顺序依次为每件建立登记、
// 初始持有（版本 1）与发行历史，历史序号连续递增，整批记录之间不会插入
// 其他发行或转让；各件随后可通过现有查询读取，并按现有规则转让、创建
// 代转授权与计算版税。整批成功同样固定系列的版税规则。
//
// 空清单、清单内编号重复或必填内容缺失按参数错误拒绝（文字元数据仍可
// 为空）；操作者、系列或任一初始持有人未登记按对象不存在拒绝；非系列
// 创建账户、系列已封存、操作者或初始持有人停用、任一藏品编号已占用
// 沿用单件发行的对应业务拒绝。整批拒绝是原子的：不新增任何藏品、持有
// 或发行历史，尚未占用的编号仍可用于后续发行，系列版税规则也不会因此
// 固定。
//
// 请求号与单件发行、转让等操作共用同一操作者的请求号范围：内容与条目
// 顺序相同的重复提交回放整批首次结果（成功或状态类业务拒绝）并标明
// Replayed；改动原因、系列、批次号、条目顺序或任一条目内容返回
// ErrRequestConflict。成功回放仍给出最初的发行结果，即使藏品已转走、
// 系列已封存或账户已停用。参数错误与引用不存在不占用请求号，修正后可
// 用原号重新提交。
func (r *Registry) IssueBatch(req IssueBatchRequest) (IssueBatchResult, error) {
	if err := req.validatePresent(); err != nil {
		return IssueBatchResult{}, err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if err := r.checkOpen(); err != nil {
		return IssueBatchResult{}, err
	}

	key := requestKey(req.Operator, req.RequestID)
	sig := issueBatchParamsSig(req)
	if prev, ok := r.state.Requests[key]; ok {
		return r.replayIssueBatch(prev, sig)
	}

	itemID, bizErr := r.checkIssueBatch(req)
	if bizErr != nil {
		if !isValidationErr(bizErr) {
			// 状态类业务拒绝（封存、停用、无权、编号已用等）占用请求号并
			// 落盘：相同参数重提永远返回这一次拒绝，即使状态后来变化。
			r.state.Requests[key] = request{
				Operator: req.Operator, RequestID: req.RequestID, Kind: "issue_batch",
				Params: sig, Rejected: true, Reason: errCode(bizErr),
				ItemID: itemID,
			}
			_ = r.commit()
		}
		// 校验类错误（引用不存在等）不占用请求号，也不改变任何状态。
		return IssueBatchResult{ItemID: itemID, Err: bizErr}, bizErr
	}

	// 在同一临界区内依次为每件建立登记、持有与发行历史，序号连续递增；
	// 与其他操作互斥，整批记录之间不会插入其他发行或转让。
	items := make([]IssueBatchItem, 0, len(req.Entries))
	stored := make([]batchItemResult, 0, len(req.Entries))
	for _, e := range req.Entries {
		seq := r.state.NextSeq + 1
		r.state.NextSeq = seq
		r.state.Items[e.ItemID] = item{
			ID: e.ItemID, SeriesID: req.SeriesID, BatchNo: req.BatchNo,
			Metadata: e.Metadata, IssuedTxID: seq,
		}
		r.state.Holdings[e.ItemID] = holding{ItemID: e.ItemID, OwnerID: e.HolderID, Version: 1}
		r.state.History = append(r.state.History, historyEntry{
			Seq: seq, Kind: "issue", ItemID: e.ItemID, Operator: req.Operator,
			Reason: req.Reason, RequestID: req.RequestID,
			FromID: "", ToID: e.HolderID, FromVersion: 0, ToVersion: 1,
		})
		items = append(items, IssueBatchItem{ItemID: e.ItemID, OwnerID: e.HolderID, Version: 1, TxSeq: seq})
		stored = append(stored, batchItemResult{ItemID: e.ItemID, OwnerID: e.HolderID, TxSeq: seq})
	}
	r.state.Requests[key] = request{
		Operator: req.Operator, RequestID: req.RequestID, Kind: "issue_batch",
		Params: sig, Batch: stored,
	}
	if err := r.commit(); err != nil {
		return IssueBatchResult{}, err
	}
	return IssueBatchResult{Items: items}, nil
}

// checkIssueBatch 校验整批发行。返回业务拒绝所涉及的藏品编号（与具体
// 某件无关时为空）与错误本身；任何一件不通过则整批拒绝。
func (r *Registry) checkIssueBatch(req IssueBatchRequest) (string, error) {
	op, ok := r.state.Accounts[req.Operator]
	if !ok {
		return "", fmt.Errorf("%w: 操作者账户 %s", ErrNotFound, req.Operator)
	}
	if !op.Active {
		return "", fmt.Errorf("%w: 操作者账户 %s", ErrAccountInactive, req.Operator)
	}
	s, ok := r.state.Series[req.SeriesID]
	if !ok {
		return "", fmt.Errorf("%w: 系列 %s", ErrNotFound, req.SeriesID)
	}
	if s.Sealed {
		return "", fmt.Errorf("%w: 系列 %s", ErrSeriesSealed, req.SeriesID)
	}
	if s.CreatorID != req.Operator {
		return "", fmt.Errorf("%w: 只有系列创建账户 %s 可以发行", ErrForbidden, s.CreatorID)
	}
	for _, e := range req.Entries {
		if _, ok := r.state.Items[e.ItemID]; ok {
			return e.ItemID, fmt.Errorf("%w: 藏品编号 %s 已被使用", ErrAlreadyExists, e.ItemID)
		}
	}
	for _, e := range req.Entries {
		h, ok := r.state.Accounts[e.HolderID]
		if !ok {
			return e.ItemID, fmt.Errorf("%w: 初始持有人账户 %s", ErrNotFound, e.HolderID)
		}
		if !h.Active {
			return e.ItemID, fmt.Errorf("%w: 初始持有人账户 %s", ErrAccountInactive, e.HolderID)
		}
	}
	return "", nil
}

func (r *Registry) replayIssueBatch(prev request, sig string) (IssueBatchResult, error) {
	if prev.Kind != "issue_batch" || prev.Params != sig {
		return IssueBatchResult{}, fmt.Errorf("%w: 操作者 %s 的请求号 %s 已用于不同请求",
			ErrRequestConflict, prev.Operator, prev.RequestID)
	}
	res := IssueBatchResult{ItemID: prev.ItemID, Replayed: true}
	if prev.Rejected {
		res.Err = codeErr(prev.Reason)
		return res, res.Err
	}
	// 回放整批首次成功的发行结果：即使藏品后来已转走、系列已封存或账户
	// 已停用，仍返回最初的持有人与历史序号，不改写当前状态。
	for _, bi := range prev.Batch {
		res.Items = append(res.Items, IssueBatchItem{
			ItemID: bi.ItemID, OwnerID: bi.OwnerID, Version: 1, TxSeq: bi.TxSeq,
		})
	}
	return res, nil
}
