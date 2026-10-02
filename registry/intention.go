package registry

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"
)

// ---- 藏品拆分意向 ----
//
// 意向只保存持有人对未来拆分方案的约定：不改变藏品编号、持有人、持有
// 版本与版税应付；既有发行、转让与代转功能继续使用。意向本身不触发任何
// 实际拆分，仅记录各方是否同意方案。

// normalizeIntentionShares 校验并规范化意向份额：份额账户去重、每份范围
// 1..10000、合计为 10000、至少两个账户（均为无状态的参数校验），并按
// 账户排序，使同一方案与书写顺序无关——仅调整名单排列视为相同内容。
// 返回规范化后的份额；参数不合法时返回 ErrInvalidArgument。
func normalizeIntentionShares(shares []IntentionShare) ([]intentionShare, error) {
	if len(shares) < 2 {
		return nil, fmt.Errorf("%w: 拆分意向至少需要两个份额账户", ErrInvalidArgument)
	}
	seen := make(map[string]bool, len(shares))
	out := make([]intentionShare, 0, len(shares))
	var total int64
	for _, sh := range shares {
		if strings.TrimSpace(sh.AccountID) == "" {
			return nil, fmt.Errorf("%w: 份额账户不能为空", ErrInvalidArgument)
		}
		if sh.Rate < 1 || sh.Rate > RoyaltyRateBase {
			return nil, fmt.Errorf("%w: 账户 %s 的份额 %d 不在 1..%d 内",
				ErrInvalidArgument, sh.AccountID, sh.Rate, RoyaltyRateBase)
		}
		if seen[sh.AccountID] {
			return nil, fmt.Errorf("%w: 份额账户 %s 重复", ErrInvalidArgument, sh.AccountID)
		}
		seen[sh.AccountID] = true
		total += sh.Rate
		out = append(out, intentionShare{AccountID: sh.AccountID, Rate: sh.Rate})
	}
	if total != RoyaltyRateBase {
		return nil, fmt.Errorf("%w: 份额合计 %d 必须为 %d",
			ErrInvalidArgument, total, RoyaltyRateBase)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].AccountID < out[j].AccountID })
	return out, nil
}

func (req CreateIntentionRequest) validatePresent() error {
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
	if strings.TrimSpace(req.IntentionID) == "" {
		missing = append(missing, "intention_id")
	}
	if strings.TrimSpace(req.ItemID) == "" {
		missing = append(missing, "item_id")
	}
	if strings.TrimSpace(req.ExpectedOwner) == "" {
		missing = append(missing, "expected_owner")
	}
	if req.ExpectedVer <= 0 {
		missing = append(missing, "expected_version")
	}
	if req.ExpiresAt.IsZero() {
		missing = append(missing, "expires_at")
	}
	if len(missing) > 0 {
		return fmt.Errorf("%w: 创建意向请求缺少必填字段 %s", ErrInvalidArgument, strings.Join(missing, ", "))
	}
	if _, err := normalizeIntentionShares(req.Shares); err != nil {
		return err
	}
	return nil
}

func createIntentionParamsSig(req CreateIntentionRequest, shares []intentionShare) string {
	// 份额按账户排序后进入签名：仅调整名单排列视为相同内容。
	b, _ := json.Marshal(struct {
		Kind          string           `json:"kind"`
		IntentionID   string           `json:"intention_id"`
		ItemID        string           `json:"item_id"`
		ExpectedOwner string           `json:"expected_owner"`
		ExpectedVer   int64            `json:"expected_version"`
		ExpiresAt     string           `json:"expires_at"`
		Shares        []intentionShare `json:"shares"`
		Reason        string           `json:"reason"`
	}{"intention_create", req.IntentionID, req.ItemID, req.ExpectedOwner, req.ExpectedVer,
		req.ExpiresAt.UTC().Format(time.RFC3339Nano), shares, req.Reason})
	return string(b)
}

// CreateIntention 由当前持有人创建拆分意向：填写唯一意向编号、藏品、期望
// 持有人与版本、绝对到期时间，以及至少两个账户的各自份额（万分之一整数，
// 每份 1..10000，合计 10000，账户不重复）。发起人可以列入方案，其份额在
// 创建时视为已同意。发起人与参与账户都须已登记且可用，到期时间须晚于当前
// 时间；系列封存不妨碍登记意向。藏品已有仍有效的待确认或已达成意向时，
// 按状态冲突拒绝。
//
// 意向不改变持有、版本与版税。同一 (操作者, 请求号) 且业务参数相同的
// 重复提交返回首次结果；参数不同返回 ErrRequestConflict；参数错误与
// 引用不存在不占用请求号。
func (r *Registry) CreateIntention(req CreateIntentionRequest) (CreateIntentionResult, error) {
	if err := req.validatePresent(); err != nil {
		return CreateIntentionResult{}, err
	}
	shares, err := normalizeIntentionShares(req.Shares)
	if err != nil {
		return CreateIntentionResult{IntentionID: req.IntentionID, Err: err}, err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if err := r.checkOpen(); err != nil {
		return CreateIntentionResult{}, err
	}

	now := r.now()
	key := requestKey(req.Operator, req.RequestID)
	sig := createIntentionParamsSig(req, shares)
	if prev, ok := r.state.Requests[key]; ok {
		return r.replayCreateIntention(prev, sig)
	}

	if bizErr := r.checkCreateIntention(req, shares, now); bizErr != nil {
		if !isValidationErr(bizErr) {
			// 状态类业务拒绝（编号已用、账户停用、版本不符、状态冲突等）
			// 占用请求号并落盘：相同参数重提永远返回这一次拒绝。
			r.state.Requests[key] = request{
				Operator: req.Operator, RequestID: req.RequestID, Kind: "intention_create",
				Params: sig, Rejected: true, Reason: errCode(bizErr),
				IntentionID: req.IntentionID, ItemID: req.ItemID,
			}
			_ = r.commit()
		}
		// 校验类错误（引用不存在等）不占用请求号，也不改变任何状态。
		return CreateIntentionResult{IntentionID: req.IntentionID, Err: bizErr}, bizErr
	}

	h := r.state.Holdings[req.ItemID]
	it := intentionRec{
		ID: req.IntentionID, ItemID: req.ItemID, OperatorID: req.Operator,
		ExpectedOwner: req.ExpectedOwner, ExpectedVer: req.ExpectedVer,
		ExpiresAt: req.ExpiresAt, Shares: shares, Responses: map[string]string{},
		GrantVer: h.Version, CreatedAt: now,
	}
	r.state.Intentions[req.IntentionID] = it
	seq := r.state.NextIntentionSeq + 1
	r.state.NextIntentionSeq = seq
	r.state.IntentionEvents = append(r.state.IntentionEvents, intentionEvent{
		Seq: seq, IntentionID: req.IntentionID, ItemID: req.ItemID, Kind: "create",
		Operator: req.Operator, Reason: req.Reason, RequestID: req.RequestID,
		FromStatus: "", ToStatus: IntentionPending, OccurredAt: now,
	})
	r.state.Requests[key] = request{
		Operator: req.Operator, RequestID: req.RequestID, Kind: "intention_create",
		Params: sig, IntentionID: req.IntentionID, ItemID: req.ItemID,
	}
	if err := r.commit(); err != nil {
		return CreateIntentionResult{}, err
	}
	return CreateIntentionResult{IntentionID: req.IntentionID, Status: IntentionPending}, nil
}

func (r *Registry) checkCreateIntention(req CreateIntentionRequest, shares []intentionShare, now time.Time) error {
	h, ok := r.state.Holdings[req.ItemID]
	if !ok {
		if _, itemExists := r.state.Items[req.ItemID]; !itemExists {
			return fmt.Errorf("%w: 藏品 %s", ErrNotFound, req.ItemID)
		}
		return fmt.Errorf("%w: 藏品 %s 没有持有记录", ErrNotFound, req.ItemID)
	}
	op, ok := r.state.Accounts[req.Operator]
	if !ok {
		return fmt.Errorf("%w: 发起人账户 %s", ErrNotFound, req.Operator)
	}
	if !op.Active {
		return fmt.Errorf("%w: 发起人账户 %s", ErrAccountInactive, req.Operator)
	}
	for _, sh := range shares {
		acc, ok := r.state.Accounts[sh.AccountID]
		if !ok {
			return fmt.Errorf("%w: 份额账户 %s", ErrNotFound, sh.AccountID)
		}
		if !acc.Active {
			return fmt.Errorf("%w: 份额账户 %s", ErrAccountInactive, sh.AccountID)
		}
	}
	// 到期时间不晚于当前时间即为参数错误；从到期时间点起意向过期。
	if !req.ExpiresAt.After(now) {
		return fmt.Errorf("%w: 到期时间 %s 必须晚于当前时间",
			ErrInvalidArgument, req.ExpiresAt.Format(time.RFC3339Nano))
	}
	// 意向编号占用是对象级冲突。
	if _, exists := r.state.Intentions[req.IntentionID]; exists {
		return fmt.Errorf("%w: 意向编号 %s 已被占用", ErrAlreadyExists, req.IntentionID)
	}
	// 期望持有人或版本不符，包括发起人并非当前持有人的情况。
	if h.OwnerID != req.ExpectedOwner || h.Version != req.ExpectedVer ||
		req.Operator != h.OwnerID {
		return fmt.Errorf("%w: 藏品 %s 当前为 %s 版本 %d",
			ErrConflict, req.ItemID, h.OwnerID, h.Version)
	}
	// 同一藏品已有仍有效的待确认或已达成意向时，新建按状态冲突拒绝；
	// 已拒绝、已撤回、已失效与已过期的意向不阻止新建。
	for _, other := range r.state.Intentions {
		if other.ItemID != req.ItemID {
			continue
		}
		st := intentionCurrentStatus(other, now, r.state)
		if st == IntentionPending || st == IntentionAgreed {
			return fmt.Errorf("%w: 藏品 %s 已有仍有效的拆分意向 %s",
				ErrConflict, req.ItemID, other.ID)
		}
	}
	return nil
}

func (r *Registry) replayCreateIntention(prev request, sig string) (CreateIntentionResult, error) {
	if prev.Kind != "intention_create" || prev.Params != sig {
		return CreateIntentionResult{}, fmt.Errorf("%w: 操作者 %s 的请求号 %s 已用于不同请求",
			ErrRequestConflict, prev.Operator, prev.RequestID)
	}
	res := CreateIntentionResult{IntentionID: prev.IntentionID, Status: IntentionPending, Replayed: true}
	if prev.Rejected {
		res.Err = codeErr(prev.Reason)
		res.Status = ""
		return res, res.Err
	}
	return res, nil
}

// intentionCurrentStatus 计算意向当前状态。撤回与拒绝是落盘的终态；失效
// （藏品持有版本变化，或发起人、任一参与账户停用）与过期按当前时间实时
// 判断，从到期时间点起算；失效判断优先于过期。失效不可恢复：藏品转回原
// 持有人也不会回到旧版本，停用也没有重新启用。
func intentionCurrentStatus(it intentionRec, now time.Time, st *snapshot) string {
	switch it.Status {
	case IntentionRejected:
		return IntentionRejected
	case IntentionWithdrawn:
		return IntentionWithdrawn
	}
	// 失效：持有版本变化（版本单调递增，藏品转回原持有人也不会回到旧版本）。
	if h, ok := st.Holdings[it.ItemID]; !ok || h.Version != it.GrantVer {
		return IntentionInvalid
	}
	// 失效：发起人停用。
	if op, ok := st.Accounts[it.OperatorID]; !ok || !op.Active {
		return IntentionInvalid
	}
	// 失效：任一参与账户停用。
	for _, sh := range it.Shares {
		if acc, ok := st.Accounts[sh.AccountID]; !ok || !acc.Active {
			return IntentionInvalid
		}
	}
	// 过期：没有失效条件时，从到期时间点起算。
	if !now.Before(it.ExpiresAt) {
		return IntentionExpired
	}
	if intentionAllAgreed(it) {
		return IntentionAgreed
	}
	return IntentionPending
}

// intentionAllAgreed 判断份额名单中除发起人（创建时视为同意）外的参与
// 账户是否全部同意。
func intentionAllAgreed(it intentionRec) bool {
	for _, sh := range it.Shares {
		if sh.AccountID == it.OperatorID {
			continue
		}
		if it.Responses[sh.AccountID] != IntentionAnswerAgree {
			return false
		}
	}
	return true
}

// intentionRespondableStatus 判断答复/撤回时意向是否仍可处理：只有待确认
// 与已达成且有效。
func intentionRespondableStatus(status string) bool {
	return status == IntentionPending || status == IntentionAgreed
}

// intentionStatusError 按意向当前状态给出拒绝答复或撤回的明确错误。
func intentionStatusError(status, intentionID string) error {
	switch status {
	case IntentionRejected:
		return fmt.Errorf("%w: 意向 %s 已拒绝", ErrIntentionRejected, intentionID)
	case IntentionWithdrawn:
		return fmt.Errorf("%w: 意向 %s 已撤回", ErrIntentionWithdrawn, intentionID)
	case IntentionInvalid:
		return fmt.Errorf("%w: 意向 %s 已失效", ErrIntentionInvalid, intentionID)
	case IntentionExpired:
		return fmt.Errorf("%w: 意向 %s 已过期", ErrIntentionExpired, intentionID)
	}
	return fmt.Errorf("%w: 意向 %s 状态为 %s", ErrConflict, intentionID, status)
}

func respondIntentionParamsSig(req RespondIntentionRequest) string {
	b, _ := json.Marshal(struct {
		Kind        string `json:"kind"`
		IntentionID string `json:"intention_id"`
		Answer      string `json:"answer"`
		Reason      string `json:"reason"`
	}{"intention_respond", req.IntentionID, req.Answer, req.Reason})
	return string(b)
}

// RespondIntention 由份额名单内的参与账户答复自己的份额：同意或拒绝。
// 待确认且有效时，全部同意显示已达成，任一拒绝显示已拒绝。待确认或已达成
// 且有效时，重复相同答复成功但不新增记录；同意后改答拒绝的，更新答复并
// 使意向进入已拒绝。名单外账户无权答复；发起人创建时视为同意，不能拒绝。
// 已拒绝、已撤回、已失效或已过期的意向不再接受答复，并说明原因。
//
// 请求号与其他操作共用同一操作者的范围：相同内容重提回放首次结果并标明
// 重复，改内容报 ErrRequestConflict；参数错误与引用不存在不占用请求号。
func (r *Registry) RespondIntention(req RespondIntentionRequest) (RespondIntentionResult, error) {
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
	if strings.TrimSpace(req.IntentionID) == "" {
		missing = append(missing, "intention_id")
	}
	if req.Answer != IntentionAnswerAgree && req.Answer != IntentionAnswerReject {
		missing = append(missing, "answer")
	}
	if len(missing) > 0 {
		return RespondIntentionResult{}, fmt.Errorf("%w: 答复请求缺少必填字段 %s",
			ErrInvalidArgument, strings.Join(missing, ", "))
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if err := r.checkOpen(); err != nil {
		return RespondIntentionResult{}, err
	}

	now := r.now()
	key := requestKey(req.Operator, req.RequestID)
	sig := respondIntentionParamsSig(req)
	if prev, ok := r.state.Requests[key]; ok {
		return r.replayRespondIntention(prev, sig)
	}

	it, ok := r.state.Intentions[req.IntentionID]
	if !ok {
		err := fmt.Errorf("%w: 意向 %s", ErrNotFound, req.IntentionID)
		return RespondIntentionResult{IntentionID: req.IntentionID, Err: err}, err
	}
	if _, ok := r.state.Accounts[req.Operator]; !ok {
		err := fmt.Errorf("%w: 答复账户 %s", ErrNotFound, req.Operator)
		return RespondIntentionResult{IntentionID: req.IntentionID, Err: err}, err
	}
	// 名单检查：只有份额名单内的参与账户可以答复。
	inList := false
	for _, sh := range it.Shares {
		if sh.AccountID == req.Operator {
			inList = true
			break
		}
	}
	if !inList {
		bizErr := fmt.Errorf("%w: 账户 %s 不是意向 %s 的参与账户",
			ErrForbidden, req.Operator, req.IntentionID)
		r.recordIntentionRequest(key, sig, req.Operator, req.RequestID, "intention_respond", bizErr, req.IntentionID)
		return RespondIntentionResult{IntentionID: req.IntentionID, Err: bizErr}, bizErr
	}
	// 发起人创建时视为同意，不能拒绝自己发起的方案。
	if req.Operator == it.OperatorID && req.Answer == IntentionAnswerReject {
		bizErr := fmt.Errorf("%w: 发起人 %s 已视为同意，不能拒绝意向 %s",
			ErrForbidden, req.Operator, req.IntentionID)
		r.recordIntentionRequest(key, sig, req.Operator, req.RequestID, "intention_respond", bizErr, req.IntentionID)
		return RespondIntentionResult{IntentionID: req.IntentionID, Err: bizErr}, bizErr
	}

	fromStatus := intentionCurrentStatus(it, now, r.state)
	if !intentionRespondableStatus(fromStatus) {
		bizErr := intentionStatusError(fromStatus, req.IntentionID)
		r.recordIntentionRequest(key, sig, req.Operator, req.RequestID, "intention_respond", bizErr, req.IntentionID)
		return RespondIntentionResult{IntentionID: req.IntentionID, Err: bizErr}, bizErr
	}

	prevAnswer := it.Responses[req.Operator]
	if req.Operator == it.OperatorID {
		prevAnswer = IntentionAnswerAgree // 发起人视为同意
	}
	if prevAnswer == req.Answer {
		// 重复相同答复：成功但不新增记录；该请求号登记为成功，重放仍返回
		// 成功。
		r.state.Requests[key] = request{
			Operator: req.Operator, RequestID: req.RequestID, Kind: "intention_respond",
			Params: sig, IntentionID: req.IntentionID,
		}
		_ = r.commit()
		return RespondIntentionResult{
			IntentionID: req.IntentionID, Answer: req.Answer, Status: fromStatus, Replayed: true,
		}, nil
	}

	// 首次答复或同意后改答拒绝。
	if req.Answer == IntentionAnswerReject {
		it.Responses[req.Operator] = IntentionAnswerReject
		it.Status = IntentionRejected
	} else {
		it.Responses[req.Operator] = IntentionAnswerAgree
		if intentionAllAgreed(it) {
			it.Status = IntentionAgreed
		}
	}
	r.state.Intentions[req.IntentionID] = it
	toStatus := intentionCurrentStatus(it, now, r.state)
	seq := r.state.NextIntentionSeq + 1
	r.state.NextIntentionSeq = seq
	r.state.IntentionEvents = append(r.state.IntentionEvents, intentionEvent{
		Seq: seq, IntentionID: req.IntentionID, ItemID: it.ItemID, Kind: "respond",
		Operator: req.Operator, Reason: req.Reason, RequestID: req.RequestID,
		Answer: req.Answer, FromStatus: fromStatus, ToStatus: toStatus, OccurredAt: now,
	})
	r.state.Requests[key] = request{
		Operator: req.Operator, RequestID: req.RequestID, Kind: "intention_respond",
		Params: sig, IntentionID: req.IntentionID,
	}
	if err := r.commit(); err != nil {
		return RespondIntentionResult{}, err
	}
	return RespondIntentionResult{
		IntentionID: req.IntentionID, Answer: req.Answer, Status: toStatus,
	}, nil
}

func (r *Registry) replayRespondIntention(prev request, sig string) (RespondIntentionResult, error) {
	if prev.Kind != "intention_respond" || prev.Params != sig {
		return RespondIntentionResult{}, fmt.Errorf("%w: 操作者 %s 的请求号 %s 已用于不同请求",
			ErrRequestConflict, prev.Operator, prev.RequestID)
	}
	res := RespondIntentionResult{IntentionID: prev.IntentionID, Replayed: true}
	if prev.Rejected {
		res.Err = codeErr(prev.Reason)
		return res, res.Err
	}
	// 回放时返回当前答复与实时状态。
	if it, ok := r.state.Intentions[prev.IntentionID]; ok {
		ans := it.Responses[prev.Operator]
		if ans == "" && prev.Operator == it.OperatorID {
			ans = IntentionAnswerAgree
		}
		res.Answer = ans
		res.Status = intentionCurrentStatus(it, r.now(), r.state)
	}
	return res, nil
}

func withdrawIntentionParamsSig(req WithdrawIntentionRequest) string {
	b, _ := json.Marshal(struct {
		Kind        string `json:"kind"`
		IntentionID string `json:"intention_id"`
		Reason      string `json:"reason"`
	}{"intention_withdraw", req.IntentionID, req.Reason})
	return string(b)
}

// WithdrawIntention 由发起人撤回仍有效的待确认或已达成意向。已撤回是幂等
// 终态：再次撤回成功但不增加记录；已拒绝不能撤回；已失效或已过期拒绝
// 撤回并说明原因。请求号与其他操作共用同一操作者的范围。
func (r *Registry) WithdrawIntention(req WithdrawIntentionRequest) (WithdrawIntentionResult, error) {
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
	if strings.TrimSpace(req.IntentionID) == "" {
		missing = append(missing, "intention_id")
	}
	if len(missing) > 0 {
		return WithdrawIntentionResult{}, fmt.Errorf("%w: 撤回请求缺少必填字段 %s",
			ErrInvalidArgument, strings.Join(missing, ", "))
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if err := r.checkOpen(); err != nil {
		return WithdrawIntentionResult{}, err
	}

	now := r.now()
	key := requestKey(req.Operator, req.RequestID)
	sig := withdrawIntentionParamsSig(req)
	if prev, ok := r.state.Requests[key]; ok {
		return r.replayWithdrawIntention(prev, sig)
	}

	it, ok := r.state.Intentions[req.IntentionID]
	if !ok {
		err := fmt.Errorf("%w: 意向 %s", ErrNotFound, req.IntentionID)
		return WithdrawIntentionResult{IntentionID: req.IntentionID, Err: err}, err
	}
	if it.OperatorID != req.Operator {
		bizErr := fmt.Errorf("%w: 只有发起人 %s 可以撤回意向 %s",
			ErrForbidden, it.OperatorID, req.IntentionID)
		r.recordIntentionRequest(key, sig, req.Operator, req.RequestID, "intention_withdraw", bizErr, req.IntentionID)
		return WithdrawIntentionResult{IntentionID: req.IntentionID, Err: bizErr}, bizErr
	}

	fromStatus := intentionCurrentStatus(it, now, r.state)
	switch fromStatus {
	case IntentionWithdrawn:
		// 已撤回是幂等终态：不新增意向变更记录；该请求号登记为成功。
		r.state.Requests[key] = request{
			Operator: req.Operator, RequestID: req.RequestID, Kind: "intention_withdraw",
			Params: sig, IntentionID: req.IntentionID,
		}
		_ = r.commit()
		return WithdrawIntentionResult{
			IntentionID: req.IntentionID, Status: IntentionWithdrawn, Replayed: true,
		}, nil
	case IntentionRejected, IntentionInvalid, IntentionExpired:
		bizErr := intentionStatusError(fromStatus, req.IntentionID)
		r.recordIntentionRequest(key, sig, req.Operator, req.RequestID, "intention_withdraw", bizErr, req.IntentionID)
		return WithdrawIntentionResult{IntentionID: req.IntentionID, Err: bizErr}, bizErr
	}

	// 待确认或已达成且有效：撤回。
	it.Status = IntentionWithdrawn
	it.WithdrawnAt = now
	r.state.Intentions[req.IntentionID] = it
	seq := r.state.NextIntentionSeq + 1
	r.state.NextIntentionSeq = seq
	r.state.IntentionEvents = append(r.state.IntentionEvents, intentionEvent{
		Seq: seq, IntentionID: req.IntentionID, ItemID: it.ItemID, Kind: "withdraw",
		Operator: req.Operator, Reason: req.Reason, RequestID: req.RequestID,
		FromStatus: fromStatus, ToStatus: IntentionWithdrawn, OccurredAt: now,
	})
	r.state.Requests[key] = request{
		Operator: req.Operator, RequestID: req.RequestID, Kind: "intention_withdraw",
		Params: sig, IntentionID: req.IntentionID,
	}
	if err := r.commit(); err != nil {
		return WithdrawIntentionResult{}, err
	}
	return WithdrawIntentionResult{IntentionID: req.IntentionID, Status: IntentionWithdrawn}, nil
}

func (r *Registry) replayWithdrawIntention(prev request, sig string) (WithdrawIntentionResult, error) {
	if prev.Kind != "intention_withdraw" || prev.Params != sig {
		return WithdrawIntentionResult{}, fmt.Errorf("%w: 操作者 %s 的请求号 %s 已用于不同请求",
			ErrRequestConflict, prev.Operator, prev.RequestID)
	}
	res := WithdrawIntentionResult{IntentionID: prev.IntentionID, Status: IntentionWithdrawn, Replayed: true}
	if prev.Rejected {
		res.Err = codeErr(prev.Reason)
		res.Status = ""
		return res, res.Err
	}
	return res, nil
}

// recordIntentionRequest 记录意向类操作的状态类业务拒绝并尽力落盘。
func (r *Registry) recordIntentionRequest(key, sig, operator, requestID, kind string, bizErr error, intentionID string) {
	r.state.Requests[key] = request{
		Operator: operator, RequestID: requestID, Kind: kind,
		Params: sig, Rejected: true, Reason: errCode(bizErr),
		IntentionID: intentionID,
	}
	_ = r.commit()
}

// ---- 意向查询 ----

// GetIntention 按编号查询拆分意向的内容、各方答复与当前状态。意向不存在
// 时返回包裹 ErrNotFound 的错误；返回的 Status 按当前时间与登记状态实时
// 计算：失效（持有版本变化、发起人或参与账户停用）与过期实时判断。
func (r *Registry) GetIntention(intentionID string) (Intention, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if err := r.checkOpen(); err != nil {
		return Intention{}, err
	}
	it, ok := r.state.Intentions[intentionID]
	if !ok {
		return Intention{}, fmt.Errorf("%w: 意向 %s", ErrNotFound, intentionID)
	}
	return r.publicIntention(it), nil
}

// publicIntention 把落盘意向转换为公开内容与实时状态。调用时需持有锁。
func (r *Registry) publicIntention(it intentionRec) Intention {
	responses := make([]IntentionResponse, 0, len(it.Shares))
	for _, sh := range it.Shares {
		ans := it.Responses[sh.AccountID]
		if sh.AccountID == it.OperatorID {
			ans = IntentionAnswerAgree // 发起人创建时视为同意
		}
		responses = append(responses, IntentionResponse{AccountID: sh.AccountID, Answer: ans})
	}
	return Intention{
		ID: it.ID, ItemID: it.ItemID, OperatorID: it.OperatorID,
		ExpectedOwner: it.ExpectedOwner, ExpectedVer: it.ExpectedVer,
		ExpiresAt: it.ExpiresAt, Shares: publicIntentionShares(it.Shares),
		Responses:   responses,
		Status:      intentionCurrentStatus(it, r.now(), r.state),
		CreatedAt:   it.CreatedAt,
		WithdrawnAt: it.WithdrawnAt,
	}
}

func publicIntentionShares(shares []intentionShare) []IntentionShare {
	out := make([]IntentionShare, 0, len(shares))
	for _, sh := range shares {
		out = append(out, IntentionShare{AccountID: sh.AccountID, Rate: sh.Rate})
	}
	return out
}

// IntentionHistory 按藏品查看意向变更记录（创建、答复、撤回），按发生
// 先后排列；每条记录包含意向编号、操作者、原因、请求号、时间与前后状态，
// 答复记录还包含答复内容。藏品不存在时返回包裹 ErrNotFound 的错误。
func (r *Registry) IntentionHistory(itemID string) ([]IntentionEvent, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if err := r.checkOpen(); err != nil {
		return nil, err
	}
	if _, ok := r.state.Items[itemID]; !ok {
		return nil, fmt.Errorf("%w: 藏品 %s", ErrNotFound, itemID)
	}
	out := make([]IntentionEvent, 0)
	for _, e := range r.state.IntentionEvents {
		if e.ItemID != itemID {
			continue
		}
		out = append(out, IntentionEvent{
			Seq: e.Seq, IntentionID: e.IntentionID, ItemID: e.ItemID, Kind: e.Kind,
			Operator: e.Operator, Reason: e.Reason, RequestID: e.RequestID,
			Answer: e.Answer, FromStatus: e.FromStatus, ToStatus: e.ToStatus,
			OccurredAt: e.OccurredAt,
		})
	}
	return out, nil
}
