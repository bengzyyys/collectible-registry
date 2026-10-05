package registry

import (
	"errors"
	"os"
	"testing"
	"time"
)

// 本文件为创建拆分意向补充"保存失败"场景的回归保障，与单件/整批发行、
// 单件/整批转让、创建代转授权（issue/transfer/batch/batch_transfer/
// authorization_save_failure_test.go）行为对齐：成功或状态类业务拒绝
// （意向编号占用、账户停用、持有版本不符）都以保存完成为准。本次结果
// 尚未写入原登记册而保存失败时，必须返回实际保存错误——不能报告创建
// 成功，也不能只返回原业务拒绝；结果为空（意向编号与状态为空、绑定
// 版本为零、不标回放、业务错误为空），请求号、意向历史序号都不被这次
// 未保存的操作消耗，即使失败后原数据暂时无法读取、状态未能按磁盘重建，
// 也不留下任何新增意向、创建记录或请求号占用。保存条件恢复后用完全相同
// 的请求重提，按当时的业务状态重新判断：拒绝条件仍在则重新保存此次拒绝
// 并返回对应业务错误，状态已满足请求且方案仍未到期则正常创建，首次重提
// 不标回放。
//
// 失败注入方式与其他场景共用：在临时文件路径 .registry.json.tmp 上预先
// 建一个目录，save 在 OpenFile 阶段即以 EISDIR 失败，原快照完好可读；
// 另有用例同时破坏数据文件，覆盖"原数据暂时无法读取"的情形。

// assertCreateIntentSaveFailureEmpty 核对"保存失败"的返回：error 是保存
// 错误而非创建成功或任何业务拒绝；结果为空——不携带意向编号、状态或
// 绑定版本，业务错误为空，也不标记为重复返回。
func assertCreateIntentSaveFailureEmpty(t *testing.T, res CreateSplitIntentResult, err error) {
	t.Helper()
	assertSaveFailureError(t, err)
	if res.IntentID != "" || res.Status != "" || res.GrantVer != 0 ||
		res.Replayed || res.Err != nil {
		t.Fatalf("保存失败必须返回空结果: %+v", res)
	}
}

// assertIntentRequestFree 核对请求号未被这次未保存的操作占用、意向历史
// 序号未被消耗（在同一个已打开的登记册上检查）。
func assertIntentRequestFree(t *testing.T, r *Registry, req CreateSplitIntentRequest, wantNextIntentSeq int64) {
	t.Helper()
	if _, ok := r.state.Requests[requestKey(req.Operator, req.RequestID)]; ok {
		t.Fatalf("未保存的结果不应占用请求号 %s", req.RequestID)
	}
	if r.state.NextIntentSeq != wantNextIntentSeq {
		t.Fatalf("NextIntentSeq = %d, want %d", r.state.NextIntentSeq, wantNextIntentSeq)
	}
}

// assertIntentAbsent 核对失败的新意向查询为不存在，且藏品的意向历史保持
// 操作前的内容与条数。
func assertIntentAbsent(t *testing.T, r *Registry, intentID, itemID string, wantEvents int) {
	t.Helper()
	if _, err := r.GetSplitIntent(intentID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("未保存的意向 %s 应查询为不存在: %v", intentID, err)
	}
	evs, err := r.SplitIntentHistory(itemID)
	if err != nil {
		t.Fatalf("SplitIntentHistory %s: %v", itemID, err)
	}
	if len(evs) != wantEvents {
		t.Fatalf("意向历史应保持 %d 条: %+v", wantEvents, evs)
	}
}

// TestCreateIntentSuccessSaveFailure 覆盖：创建拆分意向的成功结果落盘失败
// 时返回保存错误、结果为空；同一个仍打开的登记册中意向查询为不存在、意向
// 历史为空、请求号与意向历史序号都未被消耗、藏品持有不变。保存条件恢复后，
// 其他操作成功保存不会把这次未保存的意向带入登记册；用完全相同的请求重提
// 正常创建（不标回放），此后相同请求回放首次成功。
func TestCreateIntentSuccessSaveFailure(t *testing.T) {
	r := mustCreate(t, tempDir(t))
	intentWorld(t, r)
	req := createIntentReq(r, "sp1")

	blockBatchSave(t, r)
	res, err := r.CreateSplitIntent(req)
	assertCreateIntentSaveFailureEmpty(t, res, err)
	assertIntentRequestFree(t, r, req, 0)
	assertIntentAbsent(t, r, "sp1", "i1", 0)
	assertHoldingUnchanged(t, r, "i1", "alice", 1, 1)

	// 保存条件未恢复时再次提交同一请求：仍重新尝试保存并失败，不能把
	// 上次未保存的结果当成已保存回放。
	res, err = r.CreateSplitIntent(req)
	assertCreateIntentSaveFailureEmpty(t, res, err)
	assertIntentRequestFree(t, r, req, 0)

	// 保存条件恢复后，其他操作成功保存也不能把这次未保存的意向或创建
	// 记录带入登记册。
	restoreBatchSave(t, r)
	if err := r.RegisterAccount("erin", "路人"); err != nil {
		t.Fatal(err)
	}
	assertIntentRequestFree(t, r, req, 0)
	assertIntentAbsent(t, r, "sp1", "i1", 0)

	// 用完全相同的请求重提：正常创建，不标回放。
	res, err = r.CreateSplitIntent(req)
	if err != nil {
		t.Fatalf("恢复后重提应正常创建: %v", err)
	}
	if res.Replayed || res.Err != nil || res.IntentID != "sp1" ||
		res.Status != SplitPending || res.GrantVer != 1 {
		t.Fatalf("重提结果异常: %+v", res)
	}
	in, err := r.GetSplitIntent("sp1")
	if err != nil {
		t.Fatal(err)
	}
	if in.Status != SplitPending || in.GrantVer != 1 || in.InitiatorID != "alice" {
		t.Fatalf("意向内容异常: %+v", in)
	}
	evs, err := r.SplitIntentHistory("i1")
	if err != nil || len(evs) != 1 || evs[0].Seq != 1 || evs[0].Kind != "create" {
		t.Fatalf("意向历史异常: %+v, %v", evs, err)
	}

	// 成功结果保存后，相同请求回放首次成功。
	res, err = r.CreateSplitIntent(req)
	if err != nil || !res.Replayed || res.IntentID != "sp1" ||
		res.Status != SplitPending || res.GrantVer != 1 {
		t.Fatalf("成功后重提应回放: %+v, err %v", res, err)
	}
}

// TestCreateIntentSuccessSaveFailureUnreadableDisk 覆盖：保存失败且原数据
// 暂时无法读取（状态未能按磁盘重建）时，未保存的意向、创建记录与请求号
// 占用也不能留在当前登记册中；恢复正常读写后，其他操作成功保存不带入
// 这次未保存的内容，原请求重提正常创建。
func TestCreateIntentSuccessSaveFailureUnreadableDisk(t *testing.T) {
	r := mustCreate(t, tempDir(t))
	intentWorld(t, r)
	req := createIntentReq(r, "sp1")

	// 让保存失败，同时让原数据暂时无法读取：commit 无法按磁盘重建状态。
	orig, err := os.ReadFile(dataFile(r.dir))
	if err != nil {
		t.Fatal(err)
	}
	blockBatchSave(t, r)
	if err := os.WriteFile(dataFile(r.dir), []byte("{corrupt"), fileMode); err != nil {
		t.Fatal(err)
	}

	res, err := r.CreateSplitIntent(req)
	assertCreateIntentSaveFailureEmpty(t, res, err)
	// 状态未能重建时也必须显式撤销：同一个仍打开的登记册中查不到这次
	// 未保存的意向与创建记录，请求号与意向历史序号都未被消耗。
	assertIntentRequestFree(t, r, req, 0)
	assertIntentAbsent(t, r, "sp1", "i1", 0)
	assertHoldingUnchanged(t, r, "i1", "alice", 1, 1)

	// 恢复正常读写。
	if err := os.WriteFile(dataFile(r.dir), orig, fileMode); err != nil {
		t.Fatal(err)
	}
	restoreBatchSave(t, r)

	// 其他操作成功保存不能把这次未保存的意向或请求号占用带入登记册。
	if err := r.RegisterAccount("erin", "路人"); err != nil {
		t.Fatal(err)
	}
	assertIntentRequestFree(t, r, req, 0)
	assertIntentAbsent(t, r, "sp1", "i1", 0)

	// 用完全相同的请求重提：正常创建，不标回放。
	res, err = r.CreateSplitIntent(req)
	if err != nil {
		t.Fatalf("恢复后重提应正常创建: %v", err)
	}
	if res.Replayed || res.IntentID != "sp1" || res.Status != SplitPending || res.GrantVer != 1 {
		t.Fatalf("重提结果异常: %+v", res)
	}
	if r.state.NextIntentSeq != 1 {
		t.Fatalf("意向历史序号应连续: NextIntentSeq = %d", r.state.NextIntentSeq)
	}
}

// TestCreateIntentRejectSaveFailureRetrySameState 覆盖：持有版本不符的拒绝
// 落盘失败时返回保存错误、结果为空、请求号不被占用；保存恢复且拒绝条件
// 仍在时，用完全相同的请求重提会重新保存此次拒绝，保存成功后才返回业务
// 错误（首次重提不标回放）；此后相同请求回放该拒绝。
func TestCreateIntentRejectSaveFailureRetrySameState(t *testing.T) {
	r := mustCreate(t, tempDir(t))
	intentWorld(t, r)
	// i1 当前为 alice 版本 1，期望版本 2：ErrConflict。
	req := createIntentReq(r, "sp1")
	req.ExpectedVer = 2

	blockBatchSave(t, r)
	res, err := r.CreateSplitIntent(req)
	assertCreateIntentSaveFailureEmpty(t, res, err)
	assertIntentRequestFree(t, r, req, 0)
	assertIntentAbsent(t, r, "sp1", "i1", 0)

	// 保存条件未恢复时再次提交同一请求：仍按当时状态重新判断并再次尝试
	// 保存，不能把上次未保存的拒绝当成已保存的拒绝回放。
	res, err = r.CreateSplitIntent(req)
	assertCreateIntentSaveFailureEmpty(t, res, err)
	assertIntentRequestFree(t, r, req, 0)

	// 保存条件恢复、拒绝条件仍在：重提重新保存此次拒绝，保存成功后返回
	// 业务错误本身并附意向编号，这次重新处理不标记为重复。
	restoreBatchSave(t, r)
	res, err = r.CreateSplitIntent(req)
	if !errors.Is(err, ErrConflict) {
		t.Fatalf("重提应在保存成功后返回业务拒绝 ErrConflict: %v", err)
	}
	if res.Replayed || res.IntentID != "sp1" || !errors.Is(res.Err, ErrConflict) ||
		res.Status != "" || res.GrantVer != 0 {
		t.Fatalf("重提结果异常: %+v", res)
	}
	if _, ok := r.state.Requests[requestKey(req.Operator, req.RequestID)]; !ok {
		t.Fatal("拒绝保存成功后应登记请求结果")
	}
	// 拒绝不创建意向，也不消耗意向历史序号。
	assertIntentAbsent(t, r, "sp1", "i1", 0)
	if r.state.NextIntentSeq != 0 {
		t.Fatalf("拒绝不应消耗意向历史序号: NextIntentSeq = %d", r.state.NextIntentSeq)
	}

	// 拒绝已保存：相同内容重提回放原拒绝。
	res, err = r.CreateSplitIntent(req)
	if !errors.Is(err, ErrConflict) || !res.Replayed || res.IntentID != "sp1" ||
		!errors.Is(res.Err, ErrConflict) {
		t.Fatalf("保存后的重提应回放原拒绝: %+v, err %v", res, err)
	}
}

// TestCreateIntentRejectSaveFailureRetryAfterStateChange 覆盖：未保存的冲突
// 拒绝不阻碍后续重提——保存条件恢复后，藏品经合法转让恰好达到原请求要求
// 的持有人与版本，且方案仍未到期，用完全相同的请求重提应正常创建。
func TestCreateIntentRejectSaveFailureRetryAfterStateChange(t *testing.T) {
	r := mustCreate(t, tempDir(t))
	setupWorld(t, r)
	for _, id := range []string{"carol", "dave"} {
		if err := r.RegisterAccount(id, "参与人"); err != nil {
			t.Fatal(err)
		}
	}
	// i1 发行给 bob（版本 1）。
	if _, err := r.Issue(issueReq("i1", "bob")); err != nil {
		t.Fatal(err)
	}
	// alice 期望自己持有版本 2：当前为 bob 版本 1，先被冲突拒绝。
	req := CreateSplitIntentRequest{
		Operator: "alice", Reason: "协商拆分", RequestID: "create-sp1",
		IntentID: "sp1", ItemID: "i1", ExpectedOwner: "alice", ExpectedVer: 2,
		ExpiresAt: r.now().Add(time.Hour), Shares: intentShares(),
	}

	blockBatchSave(t, r)
	res, err := r.CreateSplitIntent(req)
	assertCreateIntentSaveFailureEmpty(t, res, err)
	assertIntentRequestFree(t, r, req, 0)

	// 保存条件恢复后，bob 把 i1 合法转让给 alice：i1 变为 alice 版本 2，
	// 恰好满足原请求的期望持有人与版本。
	restoreBatchSave(t, r)
	if _, err := r.Transfer(xferReq("bob", "i1", "alice", 1, "rt-i1")); err != nil {
		t.Fatal(err)
	}

	// 用完全相同的原请求重提：按当前业务状态判断，正常创建不标回放。
	res, err = r.CreateSplitIntent(req)
	if err != nil {
		t.Fatalf("状态满足后重提应正常创建，不能回放未保存的冲突: %v", err)
	}
	if res.Replayed || res.Err != nil || res.IntentID != "sp1" ||
		res.Status != SplitPending || res.GrantVer != 2 {
		t.Fatalf("重提结果异常: %+v", res)
	}
	in, err := r.GetSplitIntent("sp1")
	if err != nil {
		t.Fatal(err)
	}
	if in.Status != SplitPending || in.GrantVer != 2 || in.InitiatorID != "alice" {
		t.Fatalf("意向内容异常: %+v", in)
	}

	// 成功结果保存后，相同请求回放首次成功。
	replay, err := r.CreateSplitIntent(req)
	if err != nil || !replay.Replayed || replay.GrantVer != 2 {
		t.Fatalf("成功后重提应回放: %+v, err %v", replay, err)
	}
}

// TestCreateIntentRejectSaveFailureIntentIDTaken 覆盖：意向编号已被占用的
// 拒绝落盘失败时返回保存错误、结果为空；失败请求使用了已存在的意向
// 编号，原意向及其历史完整保留；保存恢复后重提返回 ErrAlreadyExists。
func TestCreateIntentRejectSaveFailureIntentIDTaken(t *testing.T) {
	r := mustCreate(t, tempDir(t))
	intentWorld(t, r)
	if _, err := r.CreateSplitIntent(createIntentReq(r, "sp1")); err != nil {
		t.Fatal(err)
	}
	// 换一个请求号再次使用已占用的意向编号 sp1：ErrAlreadyExists。
	req := createIntentReq(r, "sp1")
	req.RequestID = "create-sp1-again"

	blockBatchSave(t, r)
	res, err := r.CreateSplitIntent(req)
	assertCreateIntentSaveFailureEmpty(t, res, err)
	assertIntentRequestFree(t, r, req, 1)

	// 原意向及其创建历史完整保留。
	in, err := r.GetSplitIntent("sp1")
	if err != nil {
		t.Fatalf("原意向不应受失败请求影响: %v", err)
	}
	if in.Status != SplitPending || in.InitiatorID != "alice" || in.GrantVer != 1 ||
		len(in.Shares) != 3 {
		t.Fatalf("原意向内容被改变: %+v", in)
	}
	evs, err := r.SplitIntentHistory("i1")
	if err != nil || len(evs) != 1 || evs[0].Kind != "create" || evs[0].IntentID != "sp1" {
		t.Fatalf("原意向历史被改变: %+v, %v", evs, err)
	}

	restoreBatchSave(t, r)
	res, err = r.CreateSplitIntent(req)
	if !errors.Is(err, ErrAlreadyExists) {
		t.Fatalf("重提应在保存成功后返回 ErrAlreadyExists: %v", err)
	}
	if res.Replayed || res.IntentID != "sp1" || !errors.Is(res.Err, ErrAlreadyExists) {
		t.Fatalf("编号占用拒绝的重提结果异常: %+v", res)
	}
	// 原意向内容仍保持不变。
	in, _ = r.GetSplitIntent("sp1")
	if in.Status != SplitPending || in.GrantVer != 1 {
		t.Fatalf("原意向内容被改变: %+v", in)
	}
}

// TestCreateIntentRejectSaveFailureAccountInactive 覆盖：参与账户停用的
// 拒绝落盘失败时同样返回保存错误、结果为空；保存恢复后重提重新保存拒绝
// 并返回账户错误。
func TestCreateIntentRejectSaveFailureAccountInactive(t *testing.T) {
	r := mustCreate(t, tempDir(t))
	intentWorld(t, r)
	if err := r.DeactivateAccount("bob"); err != nil {
		t.Fatal(err)
	}
	req := createIntentReq(r, "sp1")

	blockBatchSave(t, r)
	res, err := r.CreateSplitIntent(req)
	assertCreateIntentSaveFailureEmpty(t, res, err)
	assertIntentRequestFree(t, r, req, 0)
	assertIntentAbsent(t, r, "sp1", "i1", 0)

	restoreBatchSave(t, r)
	res, err = r.CreateSplitIntent(req)
	if !errors.Is(err, ErrAccountInactive) {
		t.Fatalf("重提应在保存成功后返回 ErrAccountInactive: %v", err)
	}
	if res.IntentID != "sp1" || !errors.Is(res.Err, ErrAccountInactive) || res.Replayed {
		t.Fatalf("账户停用拒绝的重提结果异常: %+v", res)
	}
}

// TestCreateIntentValidationErrorIgnoresSaveFailure 覆盖：必填内容缺失、
// 方案不合法、到期时间不合法与引用对象不存在的请求不占用请求号、不要求
// 保存，即使数据位置暂时不可写也仍返回原参数/引用错误，不能改报保存错误。
func TestCreateIntentValidationErrorIgnoresSaveFailure(t *testing.T) {
	r := mustCreate(t, tempDir(t))
	intentWorld(t, r)

	blockBatchSave(t, r)
	defer restoreBatchSave(t, r)

	// 引用不存在的藏品：返回 ErrNotFound，不是保存错误。
	notFoundReq := createIntentReq(r, "sp-nf")
	notFoundReq.ItemID = "nope"
	res, err := r.CreateSplitIntent(notFoundReq)
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("引用不存在应返回 ErrNotFound，不能改报保存错误: %v", err)
	}
	if res.IntentID != "sp-nf" || !errors.Is(res.Err, ErrNotFound) {
		t.Fatalf("引用不存在的结果异常: %+v", res)
	}
	assertIntentRequestFree(t, r, notFoundReq, 0)

	// 到期时间不晚于当前时间：返回 ErrInvalidArgument，不是保存错误。
	expiredReq := createIntentReq(r, "sp-exp")
	expiredReq.ExpiresAt = r.now().Add(-time.Hour)
	res, err = r.CreateSplitIntent(expiredReq)
	if !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("到期时间不合法应返回 ErrInvalidArgument，不能改报保存错误: %v", err)
	}
	if res.IntentID != "sp-exp" || !errors.Is(res.Err, ErrInvalidArgument) {
		t.Fatalf("到期时间不合法的结果异常: %+v", res)
	}
	assertIntentRequestFree(t, r, expiredReq, 0)

	// 方案份额合计不等于 10000：返回 ErrInvalidArgument，不是保存错误。
	badSharesReq := createIntentReq(r, "sp-bad")
	badSharesReq.Shares = []SplitShare{
		{AccountID: "alice", Share: 5000},
		{AccountID: "bob", Share: 4000},
	}
	res, err = r.CreateSplitIntent(badSharesReq)
	if !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("方案不合法应返回 ErrInvalidArgument，不能改报保存错误: %v", err)
	}
	if res.IntentID != "sp-bad" || !errors.Is(res.Err, ErrInvalidArgument) {
		t.Fatalf("方案不合法的结果异常: %+v", res)
	}
	assertIntentRequestFree(t, r, badSharesReq, 0)

	// 必填内容缺失：返回 ErrInvalidArgument，不是保存错误。
	missingReq := createIntentReq(r, "sp-miss")
	missingReq.ExpectedOwner = ""
	if _, err := r.CreateSplitIntent(missingReq); !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("必填缺失应返回 ErrInvalidArgument，不能改报保存错误: %v", err)
	}
	assertIntentRequestFree(t, r, missingReq, 0)
}
