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
// 拆分意向只记录各方对未来拆分分配方案的约定：创建、答复与撤回都不改变
// 藏品编号、持有人、持有版本与版税应付，既有发行、转让与代转功能不受
// 影响。意向在创建时绑定持有版本；持有版本变化或发起人、任一参与账户
// 停用后意向失效（派生状态，不落盘），未失效时从到期时间点起过期。

// normalizeSplitShares 校验并规范化分配方案：至少两个账户、账户不重复、
// 每份 1..10000、合计恰好 10000，并按账户排序，使同一方案与书写顺序
// 无关（仅调整名单排列视为相同内容）。均为无状态的参数校验；不合法时
// 返回 ErrInvalidArgument，不占用请求号。
func normalizeSplitShares(shares []SplitShare) ([]intentShare, error) {
	if len(shares) < 2 {
		return nil, fmt.Errorf("%w: 分配方案至少需要两个账户", ErrInvalidArgument)
	}
	seen := make(map[string]bool, len(shares))
	out := make([]intentShare, 0, len(shares))
	var total int64
	for _, sh := range shares {
		if strings.TrimSpace(sh.AccountID) == "" {
			return nil, fmt.Errorf("%w: 参与账户不能为空", ErrInvalidArgument)
		}
		if sh.Share < 1 || sh.Share > RoyaltyRateBase {
			return nil, fmt.Errorf("%w: 账户 %s 的份额 %d 不在 1..%d 内",
				ErrInvalidArgument, sh.AccountID, sh.Share, RoyaltyRateBase)
		}
		if seen[sh.AccountID] {
			return nil, fmt.Errorf("%w: 参与账户 %s 重复", ErrInvalidArgument, sh.AccountID)
		}
		seen[sh.AccountID] = true
		total += sh.Share
		out = append(out, intentShare{AccountID: sh.AccountID, Share: sh.Share})
	}
	if total != RoyaltyRateBase {
		return nil, fmt.Errorf("%w: 份额合计 %d 必须等于 %d", ErrInvalidArgument, total, RoyaltyRateBase)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].AccountID < out[j].AccountID })
	return out, nil
}

// intentBaseStatus 由各方答复计算未终结意向的基础状态：全部同意为
// 已达成，否则为待确认。
func intentBaseStatus(rec intentRec) string {
	for _, sh := range rec.Shares {
		if sh.Answer != SplitAnswerAgree {
			return SplitPending
		}
	}
	return SplitAgreed
}

// intentCurrentStatus 计算意向当前状态。拒绝与撤回是落盘的终态；失效
// （持有版本变化，或发起人、任一参与账户停用）与过期不单独落盘，按当前
// 状态与时间实时判断，失效优先于过期。调用时需持有锁。
func (r *Registry) intentCurrentStatus(rec intentRec, now time.Time) string {
	switch rec.Status {
	case "rejected":
		return SplitRejected
	case "withdrawn":
		return SplitWithdrawn
	}
	h, ok := r.state.Holdings[rec.ItemID]
	if !ok || h.Version != rec.GrantVer {
		// 持有版本单调递增，藏品即使转回原持有人也不会回到旧版本，
		// 失效据此不可恢复。
		return SplitInvalid
	}
	if a, ok := r.state.Accounts[rec.InitiatorID]; !ok || !a.Active {
		return SplitInvalid
	}
	for _, sh := range rec.Shares {
		if a, ok := r.state.Accounts[sh.AccountID]; !ok || !a.Active {
			return SplitInvalid
		}
	}
	if !rec.ExpiresAt.IsZero() && !now.Before(rec.ExpiresAt) {
		return SplitExpired
	}
	return intentBaseStatus(rec)
}

func (req CreateSplitIntentRequest) validatePresent() error {
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
	if strings.TrimSpace(req.IntentID) == "" {
		missing = append(missing, "intent_id")
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
		return fmt.Errorf("%w: 创建拆分意向请求缺少必填字段 %s", ErrInvalidArgument, strings.Join(missing, ", "))
	}
	return nil
}

func createIntentParamsSig(req CreateSplitIntentRequest, shares []intentShare) string {
	type share struct {
		AccountID string `json:"account_id"`
		Share     int64  `json:"share"`
	}
	norm := make([]share, 0, len(shares))
	for _, sh := range shares {
		norm = append(norm, share{sh.AccountID, sh.Share})
	}
	b, _ := json.Marshal(struct {
		Kind          string  `json:"kind"`
		IntentID      string  `json:"intent_id"`
		ItemID        string  `json:"item_id"`
		ExpectedOwner string  `json:"expected_owner"`
		ExpectedVer   int64   `json:"expected_version"`
		ExpiresAt     string  `json:"expires_at"`
		Shares        []share `json:"shares"`
		Reason        string  `json:"reason"`
	}{"intent_create", req.IntentID, req.ItemID, req.ExpectedOwner, req.ExpectedVer,
		req.ExpiresAt.UTC().Format(time.RFC3339Nano), norm, req.Reason})
	return string(b)
}

// CreateSplitIntent 由当前持有人登记一份藏品拆分意向：指定唯一意向编号、
// 藏品、期望持有人与版本、绝对到期时间，以及至少两个账户和各自份额
// （万分之一，每份 1..10000，合计必须为 10000，账户不能重复）。发起人与
// 参与账户都须已登记且可用；发起人可以列入方案，其份额在创建时视为已
// 同意。系列封存不妨碍登记意向；藏品已有仍有效的待确认或已达成意向时，
// 新建按状态冲突拒绝。意向只保存未来拆分的约定，不改变持有与版税应付。
//
// 请求号与发行、转让等操作共用同一操作者的请求号范围：相同业务参数
// （份额名单仅排列不同视为相同）重提返回首次结果，参数不同返回
// ErrRequestConflict；参数错误与引用不存在不占用请求号。
//
// 状态类拒绝与成功结果的落盘失败时，与发行、创建代转授权一致：返回保存
// 错误（保留实际写入错误）而非该业务错误或成功结果，结果为空（无意向
// 编号、无状态、绑定版本为零、业务错误为空、不标回放），请求号与意向
// 历史序号都不被这次未保存的操作消耗；保存条件恢复后用完全相同的请求
// 重提，按当时的业务状态重新判断：拒绝条件仍在则重新保存此次拒绝并返回
// 对应业务错误，状态已变为满足请求则正常创建意向。
func (r *Registry) CreateSplitIntent(req CreateSplitIntentRequest) (CreateSplitIntentResult, error) {
	if err := req.validatePresent(); err != nil {
		return CreateSplitIntentResult{}, err
	}
	// 方案的形状校验（账户数量、重复、份额范围、合计）不依赖状态，在取锁
	// 前完成；不合法时不占用请求号。
	shares, err := normalizeSplitShares(req.Shares)
	if err != nil {
		return CreateSplitIntentResult{IntentID: req.IntentID, Err: err}, err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if err := r.checkOpen(); err != nil {
		return CreateSplitIntentResult{}, err
	}

	now := r.now()
	key := requestKey(req.Operator, req.RequestID)
	sig := createIntentParamsSig(req, shares)
	if prev, ok := r.state.Requests[key]; ok {
		return r.replayCreateIntent(prev, sig)
	}

	if bizErr := r.checkCreateSplitIntent(req, shares, now); bizErr != nil {
		if !isValidationErr(bizErr) {
			// 状态类业务拒绝（编号占用、停用、持有版本不符、状态冲突等）
			// 占用请求号并落盘；参数错误与引用不存在不占用。
			r.state.Requests[key] = request{
				Operator: req.Operator, RequestID: req.RequestID, Kind: "intent_create",
				Params: sig, Rejected: true, Reason: errCode(bizErr),
				IntentID: req.IntentID, ItemID: req.ItemID,
			}
			if err := r.commit(); err != nil {
				// 拒绝结果落盘失败：这次拒绝没有被记住，与发行、创建代转
				// 授权一致——不能只用业务错误掩盖保存错误，调用者稍后用
				// 原请求重提时必须按当时状态重新判断，而不是回放一个实际
				// 没有保存的拒绝。commit 失败时已按磁盘内容重建状态，请求
				// 记录随之撤销；此处再删一次以覆盖磁盘暂时不可读、状态未
				// 能重建的情形。整体返回空结果：无意向编号、无状态、绑定
				// 版本为零、不标回放、结果中业务错误为空，error 保留实际
				// 写入错误。
				delete(r.state.Requests, key)
				return CreateSplitIntentResult{}, err
			}
		}
		return CreateSplitIntentResult{IntentID: req.IntentID, Err: bizErr}, bizErr
	}

	h := r.state.Holdings[req.ItemID]
	// 发起人列入方案时，其份额在创建时视为已同意。
	for i := range shares {
		if shares[i].AccountID == req.Operator {
			shares[i].Answer = SplitAnswerAgree
		}
	}
	rec := intentRec{
		ID: req.IntentID, ItemID: req.ItemID, InitiatorID: req.Operator,
		Shares: shares, GrantVer: h.Version, ExpiresAt: req.ExpiresAt,
		CreatedAt: now,
	}
	// 回滚基点：保存失败且磁盘暂时不可读、状态未能按磁盘重建时，本次
	// 未保存的意向、创建记录与请求号占用都必须显式撤销，否则后续任何
	// 一次成功保存都会把这份未保存的意向带进登记册。
	prevIntentSeq := r.state.NextIntentSeq
	prevEvents := len(r.state.IntentEvents)
	r.state.Intents[req.IntentID] = rec
	status := r.intentCurrentStatus(rec, now)
	seq := prevIntentSeq + 1
	r.state.NextIntentSeq = seq
	r.state.IntentEvents = append(r.state.IntentEvents, intentEvent{
		Seq: seq, IntentID: req.IntentID, ItemID: req.ItemID, Kind: "create",
		Operator: req.Operator, Reason: req.Reason, RequestID: req.RequestID,
		FromStatus: "", ToStatus: status, OccurredAt: now,
	})
	r.state.Requests[key] = request{
		Operator: req.Operator, RequestID: req.RequestID, Kind: "intent_create",
		Params: sig, IntentID: req.IntentID, ItemID: req.ItemID,
		Version: h.Version, Status: status,
	}
	if err := r.commit(); err != nil {
		// commit 失败时已尝试按磁盘内容重建状态：重建成功则本次改动已
		// 随旧状态整体撤销（请求记录不存在），无需再动；磁盘暂时不可读、
		// 状态未能重建时请求记录仍在，据此把本次未保存的改动全部撤销。
		if _, ok := r.state.Requests[key]; ok {
			delete(r.state.Requests, key)
			delete(r.state.Intents, req.IntentID)
			r.state.IntentEvents = r.state.IntentEvents[:prevEvents]
			r.state.NextIntentSeq = prevIntentSeq
		}
		return CreateSplitIntentResult{}, err
	}
	return CreateSplitIntentResult{IntentID: req.IntentID, Status: status, GrantVer: h.Version}, nil
}

func (r *Registry) checkCreateSplitIntent(req CreateSplitIntentRequest, shares []intentShare, now time.Time) error {
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
	for _, sh := range shares {
		if _, ok := r.state.Accounts[sh.AccountID]; !ok {
			return fmt.Errorf("%w: 参与账户 %s", ErrNotFound, sh.AccountID)
		}
	}
	if !op.Active {
		return fmt.Errorf("%w: 发起人账户 %s", ErrAccountInactive, req.Operator)
	}
	for _, sh := range shares {
		if !r.state.Accounts[sh.AccountID].Active {
			return fmt.Errorf("%w: 参与账户 %s", ErrAccountInactive, sh.AccountID)
		}
	}
	// 意向编号占用是对象级冲突，优先于当前时间与持有版本检查。
	if _, exists := r.state.Intents[req.IntentID]; exists {
		return fmt.Errorf("%w: 意向编号 %s 已被占用", ErrAlreadyExists, req.IntentID)
	}
	// 到期时间不晚于当前时间即为参数错误；从到期时间点起不再有效。
	if !req.ExpiresAt.After(now) {
		return fmt.Errorf("%w: 到期时间 %s 必须晚于当前时间",
			ErrInvalidArgument, req.ExpiresAt.Format(time.RFC3339Nano))
	}
	if h.OwnerID != req.ExpectedOwner || h.Version != req.ExpectedVer ||
		req.Operator != h.OwnerID {
		return fmt.Errorf("%w: 藏品 %s 当前为 %s 版本 %d",
			ErrConflict, req.ItemID, h.OwnerID, h.Version)
	}
	// 同一藏品已有仍有效的待确认或已达成意向时，新建按状态冲突拒绝；
	// 已拒绝、已撤回、已失效或已过期的意向不妨碍另建方案。
	for _, other := range r.state.Intents {
		if other.ItemID != req.ItemID {
			continue
		}
		switch r.intentCurrentStatus(other, now) {
		case SplitPending, SplitAgreed:
			return fmt.Errorf("%w: 藏品 %s 已有仍有效的拆分意向 %s",
				ErrConflict, req.ItemID, other.ID)
		}
	}
	return nil
}

func (r *Registry) replayCreateIntent(prev request, sig string) (CreateSplitIntentResult, error) {
	if prev.Kind != "intent_create" || prev.Params != sig {
		return CreateSplitIntentResult{}, fmt.Errorf("%w: 操作者 %s 的请求号 %s 已用于不同请求",
			ErrRequestConflict, prev.Operator, prev.RequestID)
	}
	res := CreateSplitIntentResult{IntentID: prev.IntentID, Status: prev.Status,
		GrantVer: prev.Version, Replayed: true}
	if prev.Rejected {
		res.Err = codeErr(prev.Reason)
		res.Status = ""
		res.GrantVer = 0
		return res, res.Err
	}
	return res, nil
}

func answerIntentParamsSig(req AnswerSplitIntentRequest) string {
	b, _ := json.Marshal(struct {
		Kind     string `json:"kind"`
		IntentID string `json:"intent_id"`
		Agree    bool   `json:"agree"`
		Reason   string `json:"reason"`
	}{"intent_answer", req.IntentID, req.Agree, req.Reason})
	return string(b)
}

// AnswerSplitIntent 由方案中的参与账户答复自己的份额（同意或拒绝）。
// 意向待确认或已达成且仍有效时才能答复：全部同意则意向显示已达成，
// 任一拒绝则显示已拒绝。重复相同答复成功但不新增记录；已答复后改答
// 拒绝（ErrSplitAnswered）。名单外账户无权答复（ErrForbidden）；已拒绝、
// 已撤回、已失效或已过期的意向不再接受答复，分别明确拒绝。发起人列入
// 方案时创建即视为已同意，再作不同答复按改答拒绝。
//
// 请求号语义与其他操作一致：相同参数重提回放首次结果，参数不同返回
// ErrRequestConflict；参数错误与引用不存在不占用请求号。
//
// 一次答复是否生效始终以保存完成为准：本次答复（同意或拒绝）、意向终结
// 状态、意向历史与请求结果尚未写入原登记册而保存失败时，返回实际保存
// 错误，不返回成功，也不用随后读取数据的错误或原业务拒绝替代它；结果
// 为空（无意向编号、无状态、业务错误为空、不标回放），请求号与意向历史
// 序号都不被这次未保存的答复消耗。即使失败后原数据暂时无法读取、状态未
// 能按磁盘重建，同一个仍打开的登记册中各方此前已保存的答复、份额与意向
// 结束时间仍保持原样，意向状态继续按原有答复、当前持有与账户状态及当前
// 时间判断，意向历史不出现这次答复；后续其他操作成功保存也不会把这次
// 未保存的答复或终结状态带入登记册。保存条件恢复后用原请求号和相同内容
// 重提，按当时的意向状态重新处理（首次重新成功不标回放；意向此时已过期
// 或失效则沿用现有对应拒绝），真正保存成功后相同请求才回放首次结果。
func (r *Registry) AnswerSplitIntent(req AnswerSplitIntentRequest) (AnswerSplitIntentResult, error) {
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
	if strings.TrimSpace(req.IntentID) == "" {
		missing = append(missing, "intent_id")
	}
	if len(missing) > 0 {
		return AnswerSplitIntentResult{}, fmt.Errorf("%w: 答复拆分意向请求缺少必填字段 %s",
			ErrInvalidArgument, strings.Join(missing, ", "))
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if err := r.checkOpen(); err != nil {
		return AnswerSplitIntentResult{}, err
	}

	now := r.now()
	key := requestKey(req.Operator, req.RequestID)
	sig := answerIntentParamsSig(req)
	if prev, ok := r.state.Requests[key]; ok {
		return r.replayAnswerIntent(prev, sig)
	}

	rec, ok := r.state.Intents[req.IntentID]
	if !ok {
		// 引用不存在不占用请求号。
		err := fmt.Errorf("%w: 拆分意向 %s", ErrNotFound, req.IntentID)
		return AnswerSplitIntentResult{IntentID: req.IntentID, Err: err}, err
	}
	idx := -1
	for i, sh := range rec.Shares {
		if sh.AccountID == req.Operator {
			idx = i
			break
		}
	}
	if idx < 0 {
		bizErr := fmt.Errorf("%w: 账户 %s 不在意向 %s 的分配方案中",
			ErrForbidden, req.Operator, req.IntentID)
		if err := r.saveIntentRequestRejection(key, sig, req.Operator, req.RequestID,
			"intent_answer", req.IntentID, bizErr); err != nil {
			return AnswerSplitIntentResult{}, err
		}
		return AnswerSplitIntentResult{IntentID: req.IntentID, Err: bizErr}, bizErr
	}
	answer := SplitAnswerReject
	if req.Agree {
		answer = SplitAnswerAgree
	}
	// 已终结、已失效或已过期的意向不再接受答复，分别说明原因。
	switch status := r.intentCurrentStatus(rec, now); status {
	case SplitRejected, SplitWithdrawn, SplitInvalid, SplitExpired:
		bizErr := intentEndedErr(status, req.IntentID)
		if err := r.saveIntentRequestRejection(key, sig, req.Operator, req.RequestID,
			"intent_answer", req.IntentID, bizErr); err != nil {
			return AnswerSplitIntentResult{}, err
		}
		return AnswerSplitIntentResult{IntentID: req.IntentID, Err: bizErr}, bizErr
	}
	if prev := rec.Shares[idx].Answer; prev != "" {
		if prev == answer {
			// 重复相同答复：成功但不新增变更记录；该请求号仍登记为成功，
			// 相同请求重放回放同一结果。
			status := r.intentCurrentStatus(rec, now)
			r.state.Requests[key] = request{
				Operator: req.Operator, RequestID: req.RequestID, Kind: "intent_answer",
				Params: sig, IntentID: req.IntentID, Status: status,
			}
			if err := r.commit(); err != nil {
				// 这次重复答复只登记请求结果、不改动意向与历史；保存失败且
				// 状态未能按磁盘重建时，显式撤销未保存的请求号占用，整体
				// 返回空结果与实际保存错误。
				delete(r.state.Requests, key)
				return AnswerSplitIntentResult{}, err
			}
			return AnswerSplitIntentResult{IntentID: req.IntentID, Status: status}, nil
		}
		// 已答复后改答拒绝。
		bizErr := fmt.Errorf("%w: 账户 %s 已答复 %s，不能改为 %s",
			ErrSplitAnswered, req.Operator, prev, answer)
		if err := r.saveIntentRequestRejection(key, sig, req.Operator, req.RequestID,
			"intent_answer", req.IntentID, bizErr); err != nil {
			return AnswerSplitIntentResult{}, err
		}
		return AnswerSplitIntentResult{IntentID: req.IntentID, Err: bizErr}, bizErr
	}

	from := r.intentCurrentStatus(rec, now)
	// 回滚基点：保存失败且磁盘暂时不可读、状态未能按磁盘重建时，本次
	// 未保存的答复、意向终结状态、答复记录与请求号占用都必须显式撤销，
	// 否则随后查询会把这次答复当作已生效，后续任何一次成功保存还会把这份
	// 未保存的答复与终结状态带进登记册。
	prevIntentSeq := r.state.NextIntentSeq
	prevEvents := len(r.state.IntentEvents)
	prevRec := rec
	// rec.Shares 与 prevRec 共享底层数组，先复制再改，回滚副本才能保留
	// 提交前各方已保存的答复。
	rec.Shares = append([]intentShare(nil), rec.Shares...)
	rec.Shares[idx].Answer = answer
	if answer == SplitAnswerReject {
		// 任一参与账户拒绝即终结为已拒绝（落盘终态）。
		rec.Status = "rejected"
		rec.EndedAt = now
	}
	r.state.Intents[req.IntentID] = rec
	to := r.intentCurrentStatus(rec, now)
	seq := prevIntentSeq + 1
	r.state.NextIntentSeq = seq
	r.state.IntentEvents = append(r.state.IntentEvents, intentEvent{
		Seq: seq, IntentID: req.IntentID, ItemID: rec.ItemID, Kind: "answer",
		Operator: req.Operator, Reason: req.Reason, RequestID: req.RequestID,
		Answer: answer, FromStatus: from, ToStatus: to, OccurredAt: now,
	})
	r.state.Requests[key] = request{
		Operator: req.Operator, RequestID: req.RequestID, Kind: "intent_answer",
		Params: sig, IntentID: req.IntentID, Status: to,
	}
	if err := r.commit(); err != nil {
		// commit 失败时已尝试按磁盘内容重建状态：重建成功则本次改动已随旧
		// 状态整体撤销（请求记录不存在），无需再动；磁盘暂时不可读、状态
		// 未能重建时请求记录仍在，据此把本次未保存的改动全部撤销——意向
		// 恢复为提交前的答复、份额与结束时间，历史与序号回退，请求号释放。
		if _, ok := r.state.Requests[key]; ok {
			delete(r.state.Requests, key)
			r.state.Intents[req.IntentID] = prevRec
			r.state.IntentEvents = r.state.IntentEvents[:prevEvents]
			r.state.NextIntentSeq = prevIntentSeq
		}
		return AnswerSplitIntentResult{}, err
	}
	return AnswerSplitIntentResult{IntentID: req.IntentID, Status: to}, nil
}

// intentEndedErr 构造意向已终结、失效或过期时的拒绝原因。
func intentEndedErr(status, intentID string) error {
	switch status {
	case SplitRejected:
		return fmt.Errorf("%w: 拆分意向 %s 已拒绝，不再接受答复或撤回", ErrSplitIntentRejected, intentID)
	case SplitWithdrawn:
		return fmt.Errorf("%w: 拆分意向 %s 已撤回，不再接受答复", ErrSplitIntentWithdrawn, intentID)
	case SplitInvalid:
		return fmt.Errorf("%w: 拆分意向 %s 已失效（持有版本变化或参与账户停用）", ErrSplitIntentInvalid, intentID)
	default:
		return fmt.Errorf("%w: 拆分意向 %s 已过期", ErrSplitIntentExpired, intentID)
	}
}

// saveIntentRequestRejection 登记意向类操作的状态类业务拒绝并落盘。保存
// 失败时撤销这次未保存的请求号占用并把实际保存错误返回给调用者，绝不吞掉
// 写入错误、也不让未保存的拒绝留在当前登记册中。
func (r *Registry) saveIntentRequestRejection(key, sig, operator, requestID, kind, intentID string, bizErr error) error {
	r.state.Requests[key] = request{
		Operator: operator, RequestID: requestID, Kind: kind,
		Params: sig, Rejected: true, Reason: errCode(bizErr), IntentID: intentID,
	}
	if err := r.commit(); err != nil {
		// commit 失败时已尝试按磁盘内容重建状态；磁盘暂时不可读、状态未能
		// 重建时请求记录仍在，显式撤销，使这次未保存的拒绝不占用请求号。
		delete(r.state.Requests, key)
		return err
	}
	return nil
}

func (r *Registry) replayAnswerIntent(prev request, sig string) (AnswerSplitIntentResult, error) {
	if prev.Kind != "intent_answer" || prev.Params != sig {
		return AnswerSplitIntentResult{}, fmt.Errorf("%w: 操作者 %s 的请求号 %s 已用于不同请求",
			ErrRequestConflict, prev.Operator, prev.RequestID)
	}
	res := AnswerSplitIntentResult{IntentID: prev.IntentID, Status: prev.Status, Replayed: true}
	if prev.Rejected {
		res.Err = codeErr(prev.Reason)
		res.Status = ""
		return res, res.Err
	}
	return res, nil
}

func withdrawIntentParamsSig(req WithdrawSplitIntentRequest) string {
	b, _ := json.Marshal(struct {
		Kind     string `json:"kind"`
		IntentID string `json:"intent_id"`
		Reason   string `json:"reason"`
	}{"intent_withdraw", req.IntentID, req.Reason})
	return string(b)
}

// WithdrawSplitIntent 由发起人撤回仍有效的待确认或已达成意向。仅发起人
// 可撤回（ErrForbidden）；已拒绝的意向不能撤回（ErrSplitIntentRejected），
// 已失效或已过期的意向拒绝撤回并说明原因；已撤回后再次撤回成功但不新增
// 记录（幂等）。
//
// 请求号语义与其他操作一致：相同参数重提回放首次结果，参数不同返回
// ErrRequestConflict；必填内容缺失与引用不存在不占用请求号。
//
// 一次撤回（含幂等的重复撤回）以及无权撤回、已拒绝、已失效、已过期等
// 状态类拒绝是否生效始终以保存完成为准：撤回后的意向状态、结束时间、
// 撤回历史与请求结果尚未写入原登记册而保存失败时，返回实际保存错误，
// 不返回成功，也不用业务拒绝或随后读取数据的错误替代它；结果为空
// （无意向编号、无状态、业务错误为空、不标回放），请求号与意向历史序号
// 都不被这次未保存的撤回消耗。即使失败后原数据暂时无法读取、状态未能按
// 磁盘重建，同一个仍打开的登记册中原方案、各方此前的答复、创建时间与
// 结束时间仍保持操作前内容：仍有效的方案继续按当前持有、账户状态与时间
// 显示并接受原本允许的答复，继续阻止另一份有效方案的创建，意向历史不
// 出现这次撤回；后续其他操作成功保存也不会把这次未保存的撤回状态、记录
// 或请求结果带入登记册。保存条件恢复后用原请求号和完全相同的内容重提，
// 按当时的意向状态重新处理（首次重新成功不标回放；期间已到期或失效则
// 返回原有的对应拒绝），首次真正保存成功或保存拒绝之后，后续相同请求才
// 回放该结果。撤回不改变藏品持有、版本与版税应付。
func (r *Registry) WithdrawSplitIntent(req WithdrawSplitIntentRequest) (WithdrawSplitIntentResult, error) {
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
	if strings.TrimSpace(req.IntentID) == "" {
		missing = append(missing, "intent_id")
	}
	if len(missing) > 0 {
		return WithdrawSplitIntentResult{}, fmt.Errorf("%w: 撤回拆分意向请求缺少必填字段 %s",
			ErrInvalidArgument, strings.Join(missing, ", "))
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if err := r.checkOpen(); err != nil {
		return WithdrawSplitIntentResult{}, err
	}

	now := r.now()
	key := requestKey(req.Operator, req.RequestID)
	sig := withdrawIntentParamsSig(req)
	if prev, ok := r.state.Requests[key]; ok {
		return r.replayWithdrawIntent(prev, sig)
	}

	rec, ok := r.state.Intents[req.IntentID]
	if !ok {
		// 引用不存在不占用请求号。
		err := fmt.Errorf("%w: 拆分意向 %s", ErrNotFound, req.IntentID)
		return WithdrawSplitIntentResult{IntentID: req.IntentID, Err: err}, err
	}
	if rec.InitiatorID != req.Operator {
		bizErr := fmt.Errorf("%w: 只有发起人 %s 可以撤回意向 %s",
			ErrForbidden, rec.InitiatorID, req.IntentID)
		if err := r.saveIntentRequestRejection(key, sig, req.Operator, req.RequestID,
			"intent_withdraw", req.IntentID, bizErr); err != nil {
			return WithdrawSplitIntentResult{}, err
		}
		return WithdrawSplitIntentResult{IntentID: req.IntentID, Err: bizErr}, bizErr
	}
	status := r.intentCurrentStatus(rec, now)
	switch status {
	case SplitWithdrawn:
		// 已撤回是幂等终态：不新增变更记录；该请求号仍登记为成功。
		r.state.Requests[key] = request{
			Operator: req.Operator, RequestID: req.RequestID, Kind: "intent_withdraw",
			Params: sig, IntentID: req.IntentID, Status: SplitWithdrawn,
		}
		if err := r.commit(); err != nil {
			// 幂等撤回只登记请求结果、不改动意向与历史；保存失败且状态未能
			// 按磁盘重建时，显式撤销未保存的请求号占用，整体返回空结果与
			// 实际保存错误，不能把此前已保存的"已撤回"当成这次操作的结果。
			delete(r.state.Requests, key)
			return WithdrawSplitIntentResult{}, err
		}
		return WithdrawSplitIntentResult{IntentID: req.IntentID, Status: SplitWithdrawn}, nil
	case SplitRejected, SplitInvalid, SplitExpired:
		bizErr := intentEndedErr(status, req.IntentID)
		if err := r.saveIntentRequestRejection(key, sig, req.Operator, req.RequestID,
			"intent_withdraw", req.IntentID, bizErr); err != nil {
			return WithdrawSplitIntentResult{}, err
		}
		return WithdrawSplitIntentResult{IntentID: req.IntentID, Err: bizErr}, bizErr
	}

	// 回滚基点：保存失败且磁盘暂时不可读、状态未能按磁盘重建时，本次未
	// 保存的撤回状态、结束时间、撤回记录、意向历史序号与请求号占用都必须
	// 显式撤销，否则同一个仍打开的登记册会把仍有效的方案显示为已撤回，
	// 参与账户无法继续答复、还可能另建一份有效方案；后续任何一次成功保存
	// 也会把这份未保存的撤回带进登记册。
	prevIntentSeq := r.state.NextIntentSeq
	prevEvents := len(r.state.IntentEvents)
	prevRec := rec
	rec.Status = "withdrawn"
	rec.EndedAt = now
	r.state.Intents[req.IntentID] = rec
	seq := prevIntentSeq + 1
	r.state.NextIntentSeq = seq
	r.state.IntentEvents = append(r.state.IntentEvents, intentEvent{
		Seq: seq, IntentID: req.IntentID, ItemID: rec.ItemID, Kind: "withdraw",
		Operator: req.Operator, Reason: req.Reason, RequestID: req.RequestID,
		FromStatus: status, ToStatus: SplitWithdrawn, OccurredAt: now,
	})
	r.state.Requests[key] = request{
		Operator: req.Operator, RequestID: req.RequestID, Kind: "intent_withdraw",
		Params: sig, IntentID: req.IntentID, Status: SplitWithdrawn,
	}
	if err := r.commit(); err != nil {
		// commit 失败时已尝试按磁盘内容重建状态：重建成功则本次改动已随旧
		// 状态整体撤销（请求记录不存在），无需再动；磁盘暂时不可读、状态
		// 未能重建时请求记录仍在，据此把本次未保存的改动全部撤销——意向
		// 恢复为操作前的状态、答复与结束时间，历史与序号回退，请求号释放。
		if _, ok := r.state.Requests[key]; ok {
			delete(r.state.Requests, key)
			r.state.Intents[req.IntentID] = prevRec
			r.state.IntentEvents = r.state.IntentEvents[:prevEvents]
			r.state.NextIntentSeq = prevIntentSeq
		}
		return WithdrawSplitIntentResult{}, err
	}
	return WithdrawSplitIntentResult{IntentID: req.IntentID, Status: SplitWithdrawn}, nil
}

func (r *Registry) replayWithdrawIntent(prev request, sig string) (WithdrawSplitIntentResult, error) {
	if prev.Kind != "intent_withdraw" || prev.Params != sig {
		return WithdrawSplitIntentResult{}, fmt.Errorf("%w: 操作者 %s 的请求号 %s 已用于不同请求",
			ErrRequestConflict, prev.Operator, prev.RequestID)
	}
	res := WithdrawSplitIntentResult{IntentID: prev.IntentID, Status: prev.Status, Replayed: true}
	if prev.Rejected {
		res.Err = codeErr(prev.Reason)
		res.Status = ""
		return res, res.Err
	}
	return res, nil
}

// ---- 拆分意向查询 ----

// GetSplitIntent 按编号查询拆分意向的方案、各方答复与当前状态。意向不
// 存在时返回包裹 ErrNotFound 的错误；返回的 Status 按当前持有、账户
// 状态与时间实时计算（已失效、已过期为派生状态）。已结束的意向仍可
// 查询。
func (r *Registry) GetSplitIntent(intentID string) (SplitIntent, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if err := r.checkOpen(); err != nil {
		return SplitIntent{}, err
	}
	rec, ok := r.state.Intents[intentID]
	if !ok {
		return SplitIntent{}, fmt.Errorf("%w: 拆分意向 %s", ErrNotFound, intentID)
	}
	parties := make([]SplitParty, 0, len(rec.Shares))
	for _, sh := range rec.Shares {
		parties = append(parties, SplitParty{
			AccountID: sh.AccountID, Share: sh.Share, Answer: sh.Answer,
		})
	}
	return SplitIntent{
		ID: rec.ID, ItemID: rec.ItemID, InitiatorID: rec.InitiatorID,
		Shares: parties, GrantVer: rec.GrantVer, ExpiresAt: rec.ExpiresAt,
		Status:    r.intentCurrentStatus(rec, r.now()),
		CreatedAt: rec.CreatedAt, EndedAt: rec.EndedAt,
	}, nil
}

// SplitIntentHistory 按藏品查看拆分意向变更记录（创建、首次答复、撤回），
// 按发生先后排列；每条记录包含意向编号、操作者、原因、请求号、发生时间
// 与前后状态。藏品不存在时返回包裹 ErrNotFound 的错误。
func (r *Registry) SplitIntentHistory(itemID string) ([]SplitIntentEvent, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if err := r.checkOpen(); err != nil {
		return nil, err
	}
	if _, ok := r.state.Items[itemID]; !ok {
		return nil, fmt.Errorf("%w: 藏品 %s", ErrNotFound, itemID)
	}
	out := make([]SplitIntentEvent, 0)
	for _, e := range r.state.IntentEvents {
		if e.ItemID != itemID {
			continue
		}
		out = append(out, SplitIntentEvent{
			Seq: e.Seq, IntentID: e.IntentID, ItemID: e.ItemID, Kind: e.Kind,
			Operator: e.Operator, Reason: e.Reason, RequestID: e.RequestID,
			Answer: e.Answer, FromStatus: e.FromStatus, ToStatus: e.ToStatus,
			OccurredAt: e.OccurredAt,
		})
	}
	return out, nil
}
