package registry

import (
	"encoding/json"
	"fmt"
	"strings"
)

// ---- 整批发行 ----

func (req BatchIssueRequest) validatePresent() error {
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
	if len(req.Items) == 0 {
		missing = append(missing, "items")
	}
	if len(missing) > 0 {
		return fmt.Errorf("%w: 整批发行请求缺少必填字段 %s", ErrInvalidArgument, strings.Join(missing, ", "))
	}
	// 编号不重复；文字元数据仍可为空。逐件校验必填内容。
	seen := make(map[string]bool, len(req.Items))
	for i, it := range req.Items {
		if strings.TrimSpace(it.ItemID) == "" {
			return fmt.Errorf("%w: 第 %d 件藏品编号不能为空", ErrInvalidArgument, i+1)
		}
		if strings.TrimSpace(it.HolderID) == "" {
			return fmt.Errorf("%w: 第 %d 件初始持有人不能为空", ErrInvalidArgument, i+1)
		}
		if seen[it.ItemID] {
			return fmt.Errorf("%w: 藏品编号 %s 在清单内重复", ErrInvalidArgument, it.ItemID)
		}
		seen[it.ItemID] = true
	}
	return nil
}

func batchIssueParamsSig(req BatchIssueRequest) string {
	type sigItem struct {
		ItemID   string `json:"item_id"`
		Metadata string `json:"metadata"`
		HolderID string `json:"holder_id"`
	}
	items := make([]sigItem, len(req.Items))
	for i, it := range req.Items {
		items[i] = sigItem{ItemID: it.ItemID, Metadata: it.Metadata, HolderID: it.HolderID}
	}
	b, _ := json.Marshal(struct {
		Kind     string    `json:"kind"`
		SeriesID string    `json:"series_id"`
		BatchNo  string    `json:"batch_no"`
		Reason   string    `json:"reason"`
		Items    []sigItem `json:"items"`
	}{"batch_issue", req.SeriesID, req.BatchNo, req.Reason, items})
	return string(b)
}

// BatchIssue 整批发行：系列创建账户一次提交同一系列、同一批次号下的多件
// 藏品，每件给出藏品编号、文字元数据与初始持有人，整批共用操作者、原因
// 和请求号。成功后按提交顺序返回每件的编号、初始持有人、版本 1 与发行
// 历史序号；历史序号连续递增，整批记录之间不能插入其他发行或转让。
//
// 空清单、清单内编号重复或必填内容缺失按参数错误拒绝（文字元数据仍可为
// 空）；操作者、系列或任一初始持有人未登记按对象不存在拒绝；非系列创建
// 账户、系列已封存、操作者或初始持有人停用、任一藏品编号已占用沿用现有
// 对应业务拒绝。整批拒绝不新增任何藏品、持有或发行历史，尚未占用的编号
// 仍可用于后续发行；未发行过的系列也不因此固定版税规则。
//
// 同一 (操作者, 请求号) 且业务参数相同的重复提交回放整批首次结果；改动
// 原因、系列、批次号、条目顺序或任一条目内容返回 ErrRequestConflict。
// 参数错误与引用不存在不占用请求号，修正后可用原号重新提交。
func (r *Registry) BatchIssue(req BatchIssueRequest) (BatchIssueResult, error) {
	if err := req.validatePresent(); err != nil {
		return BatchIssueResult{}, err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if err := r.checkOpen(); err != nil {
		return BatchIssueResult{}, err
	}

	key := requestKey(req.Operator, req.RequestID)
	sig := batchIssueParamsSig(req)
	if prev, ok := r.state.Requests[key]; ok {
		return r.replayBatchIssue(prev, sig)
	}

	bizErr := r.checkBatchIssue(req)
	if bizErr != nil {
		if !isValidationErr(bizErr) {
			// 状态类业务拒绝（封存、停用、无权、编号已用等）占用请求号并
			// 落盘：相同参数重提永远返回这一次拒绝，即使状态后来变化。
			r.state.Requests[key] = request{
				Operator: req.Operator, RequestID: req.RequestID, Kind: "batch_issue",
				Params: sig, Rejected: true, Reason: errCode(bizErr),
			}
			_ = r.commit()
		}
		// 校验类错误（引用不存在等）不占用请求号，也不改变任何状态。
		return BatchIssueResult{Err: bizErr}, bizErr
	}

	// 整批一次落盘：藏品、持有、历史与请求结果在同一临界区内原子替换，
	// 历史序号连续递增，整批记录之间不能插入其他发行或转让。
	results := make([]BatchIssueItemResult, len(req.Items))
	batchItems := make([]requestBatchItem, len(req.Items))
	for i, it := range req.Items {
		seq := r.state.NextSeq + 1
		r.state.NextSeq = seq
		r.state.Items[it.ItemID] = item{
			ID: it.ItemID, SeriesID: req.SeriesID, BatchNo: req.BatchNo,
			Metadata: it.Metadata, IssuedTxID: seq,
		}
		r.state.Holdings[it.ItemID] = holding{ItemID: it.ItemID, OwnerID: it.HolderID, Version: 1}
		r.state.History = append(r.state.History, historyEntry{
			Seq: seq, Kind: "issue", ItemID: it.ItemID, Operator: req.Operator,
			Reason: req.Reason, RequestID: req.RequestID,
			FromID: "", ToID: it.HolderID, FromVersion: 0, ToVersion: 1,
		})
		results[i] = BatchIssueItemResult{ItemID: it.ItemID, OwnerID: it.HolderID, Version: 1, TxSeq: seq}
		batchItems[i] = requestBatchItem{ItemID: it.ItemID, ToID: it.HolderID, Version: 1, TxSeq: seq}
	}
	r.state.Requests[key] = request{
		Operator: req.Operator, RequestID: req.RequestID, Kind: "batch_issue",
		Params: sig, BatchItems: batchItems,
	}
	if err := r.commit(); err != nil {
		return BatchIssueResult{}, err
	}
	return BatchIssueResult{Items: results}, nil
}

// checkBatchIssue 逐件校验整批发行的业务条件。操作者、系列或任一初始持有
// 人未登记按对象不存在拒绝；非系列创建账户、系列已封存、操作者或初始持有
// 人停用、任一藏品编号已占用沿用现有对应业务拒绝。涉及某件时错误信息指出
// 该件编号。
func (r *Registry) checkBatchIssue(req BatchIssueRequest) error {
	op, ok := r.state.Accounts[req.Operator]
	if !ok {
		return fmt.Errorf("%w: 操作者账户 %s", ErrNotFound, req.Operator)
	}
	if !op.Active {
		return fmt.Errorf("%w: 操作者账户 %s", ErrAccountInactive, req.Operator)
	}
	s, ok := r.state.Series[req.SeriesID]
	if !ok {
		return fmt.Errorf("%w: 系列 %s", ErrNotFound, req.SeriesID)
	}
	if s.Sealed {
		return fmt.Errorf("%w: 系列 %s", ErrSeriesSealed, req.SeriesID)
	}
	if s.CreatorID != req.Operator {
		return fmt.Errorf("%w: 只有系列创建账户 %s 可以发行", ErrForbidden, s.CreatorID)
	}
	// 逐件校验：编号占用、持有人存在且可用。按提交顺序报告首个问题件。
	for _, it := range req.Items {
		if _, ok := r.state.Items[it.ItemID]; ok {
			return fmt.Errorf("%w: 藏品编号 %s 已被使用", ErrAlreadyExists, it.ItemID)
		}
		h, ok := r.state.Accounts[it.HolderID]
		if !ok {
			return fmt.Errorf("%w: 初始持有人账户 %s（藏品 %s）", ErrNotFound, it.HolderID, it.ItemID)
		}
		if !h.Active {
			return fmt.Errorf("%w: 初始持有人账户 %s（藏品 %s）", ErrAccountInactive, it.HolderID, it.ItemID)
		}
	}
	return nil
}

func (r *Registry) replayBatchIssue(prev request, sig string) (BatchIssueResult, error) {
	if prev.Kind != "batch_issue" || prev.Params != sig {
		return BatchIssueResult{}, fmt.Errorf("%w: 操作者 %s 的请求号 %s 已用于不同请求",
			ErrRequestConflict, prev.Operator, prev.RequestID)
	}
	res := BatchIssueResult{Replayed: true}
	if prev.Rejected {
		res.Err = codeErr(prev.Reason)
		return res, res.Err
	}
	items := make([]BatchIssueItemResult, len(prev.BatchItems))
	for i, bi := range prev.BatchItems {
		items[i] = BatchIssueItemResult{
			ItemID: bi.ItemID, OwnerID: bi.ToID, Version: bi.Version, TxSeq: bi.TxSeq,
		}
	}
	res.Items = items
	return res, nil
}
