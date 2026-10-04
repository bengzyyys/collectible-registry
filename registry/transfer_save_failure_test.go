package registry

import (
	"errors"
	"testing"
)

// 本文件为单件转让补充"拒绝结果保存失败"场景的回归保障：状态类业务拒绝
// （持有信息不符、收发同人、账户停用等）在落盘失败时，必须返回保存错误
// 而非业务错误，结果为空，请求号与历史序号都不被这次未保存的拒绝占用；
// 保存条件恢复后用完全相同的请求重提，按当时的业务状态重新判断——拒绝
// 条件仍在则重新保存并返回业务错误，状态已变为满足请求则正常转出。
// 与整批转让的对应场景（batch_transfer_save_failure_test.go）行为一致。
//
// 失败注入方式与整批相同：在临时文件路径上预建目录使 save 在替换数据
// 文件之前失败，原快照完好可读；删除该目录即恢复。

// transferSaveFailureReq 是一个会被持有信息不符拒绝的单件转让请求：
// i1 当前为 bob 版本 1，请求却期望版本 2。
func transferSaveFailureReq() TransferRequest {
	return TransferRequest{
		Operator: "bob", Reason: "转出", RequestID: "rt-fail", ItemID: "i1",
		ExpectedOwner: "bob", ExpectedVer: 2, ToID: "carol", Price: 10000,
	}
}

// assertTransferRejectSaveFailure 核对"拒绝保存失败"的返回：error 是保存
// 错误而非任何业务拒绝；结果为空——无藏品编号、无持有变化、无历史序号、
// 无金额、业务错误为空、不标回放。
func assertTransferRejectSaveFailure(t *testing.T, res TransferResult, err error) {
	t.Helper()
	assertSaveFailureError(t, err)
	if res.ItemID != "" || res.FromID != "" || res.ToID != "" ||
		res.Version != 0 || res.TxSeq != 0 || res.Replayed || res.Err != nil ||
		res.Price != 0 || res.Payables != nil || res.Remainder != 0 {
		t.Fatalf("拒绝保存失败必须返回空结果: %+v", res)
	}
}

// assertTransferWorldUntouched 核对失败（及重试）后 i1 保持提交前内容：
// 持有人 bob 版本 1，只有发行一条历史，历史序号仍是发行占用的 5，
// 也没有任何应付记录。请求号是否占用由各用例按阶段单独核对。
func assertTransferWorldUntouched(t *testing.T, r *Registry, req TransferRequest) {
	t.Helper()
	h, err := r.GetHolding(req.ItemID)
	if err != nil {
		t.Fatalf("GetHolding %s: %v", req.ItemID, err)
	}
	if h.OwnerID != "bob" || h.Version != 1 {
		t.Fatalf("失败后 %s 持有被改变: %+v", req.ItemID, h)
	}
	hist, err := r.History(req.ItemID)
	if err != nil {
		t.Fatalf("History %s: %v", req.ItemID, err)
	}
	if len(hist) != 1 || hist[0].Kind != "issue" {
		t.Fatalf("失败后 %s 历史多出本次请求: %+v", req.ItemID, hist)
	}
	if r.state.NextSeq != 5 {
		t.Fatalf("NextSeq = %d，失败不应消耗历史序号，仍应为 5", r.state.NextSeq)
	}
	if len(r.state.Royalties) != 0 {
		t.Fatalf("失败后不应产生版税应付: %+v", r.state.Royalties)
	}
}

// TestTransferRejectSaveFailureSameRegistry 覆盖核心场景：拒绝保存失败后
// 调用者继续使用同一个已打开的登记册，即使暂时无法重新读取原数据，也不
// 能把这次拒绝当成已保存；保存条件恢复、拒绝条件仍在时用完全相同的请求
// 重提，重新保存此次拒绝并返回业务错误（不标回放），此后相同请求回放该
// 拒绝。
func TestTransferRejectSaveFailureSameRegistry(t *testing.T) {
	r := mustCreate(t, tempDir(t))
	setupBatchTransferWorld(t, r)
	req := transferSaveFailureReq()

	blockBatchSave(t, r)
	res, err := r.Transfer(req)
	assertTransferRejectSaveFailure(t, res, err)

	// 同一个已打开的登记册上立即核对：藏品持有、版本、历史与应付保持
	// 提交前内容，历史序号与请求号都未被消耗。
	assertTransferWorldUntouched(t, r, req)
	if _, ok := r.state.Requests[requestKey(req.Operator, req.RequestID)]; ok {
		t.Fatal("未保存的拒绝不应占用请求号")
	}

	// 保存条件恢复、拒绝条件（期望版本 2 与实际版本 1 不符）仍在：重提
	// 重新保存此次拒绝，返回对应业务错误与藏品编号，不标回放。
	restoreBatchSave(t, r)
	res, err = r.Transfer(req)
	if !errors.Is(err, ErrConflict) {
		t.Fatalf("重提应在保存成功后返回业务拒绝 ErrConflict: %v", err)
	}
	if res.Replayed || res.ItemID != "i1" || !errors.Is(res.Err, ErrConflict) {
		t.Fatalf("重提结果异常: %+v", res)
	}
	// 这次拒绝已保存：请求号被占用，藏品状态仍未改变。
	if _, ok := r.state.Requests[requestKey(req.Operator, req.RequestID)]; !ok {
		t.Fatal("拒绝保存成功后应登记请求结果")
	}
	assertTransferWorldUntouched(t, r, req)

	// 拒绝已保存：相同请求重提回放首次拒绝并标明重复。
	res, err = r.Transfer(req)
	if !errors.Is(err, ErrConflict) || !res.Replayed ||
		res.ItemID != "i1" || !errors.Is(res.Err, ErrConflict) {
		t.Fatalf("保存后的重提应回放原拒绝: %+v, err %v", res, err)
	}
}

// TestTransferRejectSaveFailureRetryAfterStateChange 覆盖：未保存的拒绝不
// 阻碍后续重提——失败后藏品经另一笔合法转让恰好达到原请求要求的持有人与
// 版本时，用完全相同的请求重提应正常转出，产生一笔历史及既定规则的应付，
// 并返回实际的新版本。
func TestTransferRejectSaveFailureRetryAfterStateChange(t *testing.T) {
	r := mustCreate(t, tempDir(t))
	setupBatchTransferWorld(t, r)
	// i5 当前为 carol 版本 1；请求由 bob 发起，期望 bob 版本 2——持有信息
	// 不符，先被拒绝。
	req := TransferRequest{
		Operator: "bob", Reason: "受让后再转出", RequestID: "rt-fail2", ItemID: "i5",
		ExpectedOwner: "bob", ExpectedVer: 2, ToID: "alice", Price: 1000,
	}

	blockBatchSave(t, r)
	res, err := r.Transfer(req)
	assertTransferRejectSaveFailure(t, res, err)
	if _, ok := r.state.Requests[requestKey(req.Operator, req.RequestID)]; ok {
		t.Fatal("未保存的拒绝不应占用请求号")
	}
	if r.state.NextSeq != 5 {
		t.Fatalf("NextSeq = %d, want 5", r.state.NextSeq)
	}

	// 保存条件恢复后，carol 把 i5 合法转让给 bob：i5 变为 bob 版本 2，
	// 恰好达到原请求要求的持有人与版本（历史序号 6）。
	restoreBatchSave(t, r)
	if _, err := r.Transfer(TransferRequest{
		Operator: "carol", Reason: "合法转让", RequestID: "rt-i5", ItemID: "i5",
		ExpectedOwner: "carol", ExpectedVer: 1, ToID: "bob", Price: 500,
	}); err != nil {
		t.Fatal(err)
	}

	// 用完全相同的原请求重提：按当前状态重新判断，正常转出一次——不是
	// 回放那次未保存的拒绝。i5 归 alice 版本 3，历史序号 7；s1 版税
	// carol 10% + dave 5%，余款归转让前持有人 bob。
	res, err = r.Transfer(req)
	if err != nil {
		t.Fatalf("状态满足后重提应正常转出，不能回放未保存的拒绝: %v", err)
	}
	if res.Replayed || res.ItemID != "i5" || res.FromID != "bob" || res.ToID != "alice" ||
		res.Version != 3 || res.TxSeq != 7 || res.Err != nil {
		t.Fatalf("重提结果异常: %+v", res)
	}
	if res.Price != 1000 || res.Remainder != 850 {
		t.Fatalf("重提金额异常: %+v", res)
	}
	wantPayables := []RoyaltyPayable{
		{AccountID: "carol", Rate: 1000, Amount: 100},
		{AccountID: "dave", Rate: 500, Amount: 50},
	}
	if len(res.Payables) != 2 || res.Payables[0] != wantPayables[0] || res.Payables[1] != wantPayables[1] {
		t.Fatalf("重提应付 = %+v, want %+v", res.Payables, wantPayables)
	}
	h, err := r.GetHolding("i5")
	if err != nil {
		t.Fatal(err)
	}
	if h.OwnerID != "alice" || h.Version != 3 {
		t.Fatalf("成功后 i5 持有 = %+v", h)
	}
	hist, err := r.History("i5")
	if err != nil {
		t.Fatal(err)
	}
	if len(hist) != 3 || hist[2].Seq != 7 || hist[2].Kind != "transfer" ||
		hist[2].Operator != "bob" || hist[2].RequestID != req.RequestID ||
		hist[2].FromID != "bob" || hist[2].ToID != "alice" ||
		hist[2].FromVersion != 2 || hist[2].ToVersion != 3 {
		t.Fatalf("i5 历史异常: %+v", hist)
	}
	if r.state.NextSeq != 7 {
		t.Fatalf("NextSeq = %d, want 7", r.state.NextSeq)
	}

	// 成功结果保存后，相同请求回放首次成功。
	replay, err := r.Transfer(req)
	if err != nil || !replay.Replayed || replay.Version != 3 || replay.TxSeq != 7 {
		t.Fatalf("成功后重提应回放: %+v, err %v", replay, err)
	}
}

// TestTransferRejectSaveFailureRetryAfterReopen 覆盖磁盘视角：拒绝保存失败
// 后原登记册仍可正常读取——关闭后重新 Open，看到的仍是提交前状态，请求号
// 未被占用；恢复保存条件后在重开的登记册上重提，重新保存此次拒绝。
func TestTransferRejectSaveFailureRetryAfterReopen(t *testing.T) {
	dir := tempDir(t)
	r := mustCreate(t, dir)
	setupBatchTransferWorld(t, r)
	req := transferSaveFailureReq()

	blockBatchSave(t, r)
	res, err := r.Transfer(req)
	assertTransferRejectSaveFailure(t, res, err)

	// 原数据文件从未被替换：正常关闭并重新打开仍能读取提交前状态。
	if err := r.Close(); err != nil {
		t.Fatal(err)
	}
	r2, err := Open(dir)
	if err != nil {
		t.Fatalf("失败后原登记册必须仍能正常读取: %v", err)
	}
	t.Cleanup(func() { _ = r2.Close() })
	assertTransferWorldUntouched(t, r2, req)
	if _, ok := r2.state.Requests[requestKey(req.Operator, req.RequestID)]; ok {
		t.Fatal("未保存的拒绝不应占用请求号")
	}

	// 恢复保存条件后在重开的登记册上重提：重新保存此次拒绝并返回业务
	// 错误，不标回放。
	restoreBatchSave(t, r2)
	res, err = r2.Transfer(req)
	if !errors.Is(err, ErrConflict) {
		t.Fatalf("重提应在保存成功后返回业务拒绝 ErrConflict: %v", err)
	}
	if res.Replayed || res.ItemID != "i1" || !errors.Is(res.Err, ErrConflict) {
		t.Fatalf("重提结果异常: %+v", res)
	}
}

// TestTransferValidationErrorIgnoresSaveFailure 覆盖：参数不合法或引用对象
// 不存在沿用现有拒绝规则，不保存拒绝、不占用请求号；即使存储暂时不可写，
// 也必须返回原参数错误或对象不存在错误，不能改成保存错误。
func TestTransferValidationErrorIgnoresSaveFailure(t *testing.T) {
	r := mustCreate(t, tempDir(t))
	setupBatchTransferWorld(t, r)

	blockBatchSave(t, r)
	defer restoreBatchSave(t, r)

	// 引用不存在的藏品：返回 ErrNotFound，不是保存错误。
	notFoundReq := TransferRequest{
		Operator: "bob", Reason: "转出", RequestID: "rt-nf", ItemID: "nope",
		ExpectedOwner: "bob", ExpectedVer: 1, ToID: "carol",
	}
	res, err := r.Transfer(notFoundReq)
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("引用不存在应返回 ErrNotFound，不能改报保存错误: %v", err)
	}
	if res.ItemID != "nope" || !errors.Is(res.Err, ErrNotFound) {
		t.Fatalf("引用不存在的结果异常: %+v", res)
	}
	if _, ok := r.state.Requests[requestKey("bob", "rt-nf")]; ok {
		t.Fatal("校验错误不应占用请求号")
	}

	// 参数错误（负价款）：返回 ErrInvalidArgument，不是保存错误。
	badReq := TransferRequest{
		Operator: "bob", Reason: "转出", RequestID: "rt-bad", ItemID: "i1",
		ExpectedOwner: "bob", ExpectedVer: 1, ToID: "carol", Price: -1,
	}
	if _, err := r.Transfer(badReq); !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("参数错误应返回 ErrInvalidArgument，不能改报保存错误: %v", err)
	}
	if _, ok := r.state.Requests[requestKey("bob", "rt-bad")]; ok {
		t.Fatal("参数错误不应占用请求号")
	}
	if r.state.NextSeq != 5 {
		t.Fatalf("NextSeq = %d, want 5", r.state.NextSeq)
	}
}
