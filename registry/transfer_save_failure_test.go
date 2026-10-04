package registry

import (
	"errors"
	"reflect"
	"testing"
)

// 本文件为单件转让补充"拒绝结果保存失败"场景的回归保障，与整批转让
// （batch_transfer_save_failure_test.go）行为对齐：状态类业务拒绝（持有
// 信息不符、账户停用、收发同人）在落盘失败时，必须返回保存错误而非业务
// 错误，结果为空，请求号与历史序号都不被这次未保存的拒绝消耗；保存条件
// 恢复后用完全相同的请求重提，按当时的业务状态重新判断——拒绝条件仍在
// 则重新保存此次拒绝并返回对应业务错误，状态已变为满足请求则正常转出。
//
// 失败注入方式与整批共用：在临时文件路径 .registry.json.tmp 上预先建一个
// 目录，save 在 OpenFile 阶段即以 EISDIR 失败，原快照完好可读。

// xferSaveReq 构造 bob 发起的单件转让请求。
func xferSaveReq(rid, item, expectedOwner string, ver int64, to string, price int64) TransferRequest {
	return TransferRequest{
		Operator: "bob", Reason: "转出", RequestID: rid, ItemID: item,
		ExpectedOwner: expectedOwner, ExpectedVer: ver, ToID: to, Price: price,
	}
}

// assertTransferSaveFailureEmpty 核对"拒绝结果保存失败"的返回：error 是
// 保存错误而非任何业务拒绝；结果为空——不携带藏品编号、前后持有人、版本、
// 历史序号或金额，业务错误为空，也不标记为重复返回。
func assertTransferSaveFailureEmpty(t *testing.T, res TransferResult, err error) {
	t.Helper()
	assertSaveFailureError(t, err)
	if res.ItemID != "" || res.FromID != "" || res.ToID != "" ||
		res.Version != 0 || res.TxSeq != 0 || res.Replayed || res.Err != nil ||
		res.Price != 0 || res.Payables != nil || res.Remainder != 0 {
		t.Fatalf("拒绝保存失败必须返回空结果: %+v", res)
	}
}

// assertXferRequestFree 核对请求号未被这次未保存的拒绝占用、历史序号未被
// 消耗（在同一个已打开的登记册上检查）。
func assertXferRequestFree(t *testing.T, r *Registry, req TransferRequest, wantNextSeq int64) {
	t.Helper()
	if _, ok := r.state.Requests[requestKey(req.Operator, req.RequestID)]; ok {
		t.Fatalf("未保存的拒绝不应占用请求号 %s", req.RequestID)
	}
	if r.state.NextSeq != wantNextSeq {
		t.Fatalf("NextSeq = %d, want %d", r.state.NextSeq, wantNextSeq)
	}
}

// assertHoldingUnchanged 核对藏品持有与历史保持提交前内容。
func assertHoldingUnchanged(t *testing.T, r *Registry, itemID, owner string, ver int64, histLen int) {
	t.Helper()
	h, err := r.GetHolding(itemID)
	if err != nil {
		t.Fatalf("GetHolding %s: %v", itemID, err)
	}
	if h.OwnerID != owner || h.Version != ver {
		t.Fatalf("失败后 %s 持有被改变: %+v", itemID, h)
	}
	hist, err := r.History(itemID)
	if err != nil {
		t.Fatalf("History %s: %v", itemID, err)
	}
	if len(hist) != histLen {
		t.Fatalf("失败后 %s 历史多出内容: %+v", itemID, hist)
	}
}

// TestTransferRejectSaveFailureRetrySameState 覆盖：持有信息不符的拒绝落盘
// 失败时返回保存错误、结果为空、请求号不被占用；保存恢复且拒绝条件仍在时，
// 用完全相同的请求重提会重新保存此次拒绝，保存成功后才返回业务错误；此后
// 相同请求回放该拒绝。
func TestTransferRejectSaveFailureRetrySameState(t *testing.T) {
	r := mustCreate(t, tempDir(t))
	setupBatchTransferWorld(t, r)
	// i5 属于 carol（版本 1），与期望的 bob 不符：ErrConflict。
	req := xferSaveReq("rt-rej", "i5", "bob", 1, "dave", 200)

	blockBatchSave(t, r)
	res, err := r.Transfer(req)
	assertTransferSaveFailureEmpty(t, res, err)
	assertXferRequestFree(t, r, req, 5)
	assertHoldingUnchanged(t, r, "i5", "carol", 1, 1)

	// 保存条件未恢复时再次提交同一请求：仍按当时状态重新判断并再次尝试
	// 保存，不能把上次未保存的拒绝当成已保存的拒绝回放。
	res, err = r.Transfer(req)
	assertTransferSaveFailureEmpty(t, res, err)
	assertXferRequestFree(t, r, req, 5)

	// 保存条件恢复、拒绝条件仍在：重提重新保存此次拒绝，保存成功后返回
	// 业务错误本身并附藏品编号，这次重新处理不标记为重复。
	restoreBatchSave(t, r)
	res, err = r.Transfer(req)
	if !errors.Is(err, ErrConflict) {
		t.Fatalf("重提应在保存成功后返回业务拒绝 ErrConflict: %v", err)
	}
	if res.Replayed || res.ItemID != "i5" || !errors.Is(res.Err, ErrConflict) ||
		res.Version != 0 || res.TxSeq != 0 {
		t.Fatalf("重提结果异常: %+v", res)
	}
	if _, ok := r.state.Requests[requestKey(req.Operator, req.RequestID)]; !ok {
		t.Fatal("拒绝保存成功后应登记请求结果")
	}
	assertHoldingUnchanged(t, r, "i5", "carol", 1, 1)

	// 拒绝已保存：相同内容重提回放原拒绝。
	res, err = r.Transfer(req)
	if !errors.Is(err, ErrConflict) || !res.Replayed || res.ItemID != "i5" ||
		!errors.Is(res.Err, ErrConflict) {
		t.Fatalf("保存后的重提应回放原拒绝: %+v, err %v", res, err)
	}
}

// TestTransferRejectSaveFailureRetryAfterStateChange 覆盖：未保存的冲突拒绝
// 不阻碍后续重提——保存条件恢复后，藏品经另一笔合法转让恰好达到原请求要求
// 的持有人与版本，用完全相同的请求重提应正常转出，产生一笔历史及既定规则
// 规定的应付，并返回实际的新版本。
func TestTransferRejectSaveFailureRetryAfterStateChange(t *testing.T) {
	r := mustCreate(t, tempDir(t))
	setupBatchTransferWorld(t, r)
	// i5 期望 bob 持有版本 2：当前为 carol 版本 1，先被冲突拒绝。
	req := xferSaveReq("rt-rej2", "i5", "bob", 2, "dave", 200)

	blockBatchSave(t, r)
	res, err := r.Transfer(req)
	assertTransferSaveFailureEmpty(t, res, err)
	assertXferRequestFree(t, r, req, 5)

	// 保存条件恢复后，carol 把 i5 合法转让给 bob：i5 变为 bob 版本 2，
	// 恰好满足原请求的期望持有人与版本。
	restoreBatchSave(t, r)
	if _, err := r.Transfer(TransferRequest{
		Operator: "carol", Reason: "合法转让", RequestID: "rt-i5", ItemID: "i5",
		ExpectedOwner: "carol", ExpectedVer: 1, ToID: "bob",
	}); err != nil {
		t.Fatal(err)
	}

	// 用完全相同的原请求重提：按当前业务状态判断，正常转出不标回放。
	res, err = r.Transfer(req)
	if err != nil {
		t.Fatalf("状态满足后重提应正常执行，不能回放未保存的冲突: %v", err)
	}
	if res.Replayed || res.Err != nil || res.ItemID != "i5" ||
		res.FromID != "bob" || res.ToID != "dave" || res.Version != 3 || res.TxSeq != 7 {
		t.Fatalf("重提结果异常: %+v", res)
	}
	// 金额按所属系列 s1 的规则（carol 10% + dave 5%）计算，余款归转让前
	// 持有人 bob。
	wantPayables := []RoyaltyPayable{
		{AccountID: "carol", Rate: 1000, Amount: 20},
		{AccountID: "dave", Rate: 500, Amount: 10},
	}
	if res.Price != 200 || res.Remainder != 170 || !reflect.DeepEqual(res.Payables, wantPayables) {
		t.Fatalf("重提金额异常: %+v", res)
	}
	assertHoldingUnchanged(t, r, "i5", "dave", 3, 3)
	tr, err := r.TransferRoyalty(7)
	if err != nil {
		t.Fatalf("TransferRoyalty 7: %v", err)
	}
	if tr.ItemID != "i5" || tr.Price != 200 || tr.Remainder != 170 || tr.OwnerID != "bob" {
		t.Fatalf("版税记录异常: %+v", tr)
	}

	// 成功结果保存后，相同请求回放首次成功。
	replay, err := r.Transfer(req)
	if err != nil || !replay.Replayed || replay.Version != 3 || replay.TxSeq != 7 {
		t.Fatalf("成功后重提应回放: %+v, err %v", replay, err)
	}
}

// TestTransferRejectSaveFailureAccountInactive 覆盖：操作者账户停用的拒绝
// 落盘失败时同样返回保存错误、结果为空；保存恢复后重提重新保存拒绝并返回
// 账户错误。
func TestTransferRejectSaveFailureAccountInactive(t *testing.T) {
	r := mustCreate(t, tempDir(t))
	setupBatchTransferWorld(t, r)
	if err := r.DeactivateAccount("bob"); err != nil {
		t.Fatal(err)
	}
	req := xferSaveReq("rt-inactive", "i1", "bob", 1, "carol", 0)

	blockBatchSave(t, r)
	res, err := r.Transfer(req)
	assertTransferSaveFailureEmpty(t, res, err)
	assertXferRequestFree(t, r, req, 5)
	assertHoldingUnchanged(t, r, "i1", "bob", 1, 1)

	restoreBatchSave(t, r)
	res, err = r.Transfer(req)
	if !errors.Is(err, ErrAccountInactive) {
		t.Fatalf("重提应在保存成功后返回 ErrAccountInactive: %v", err)
	}
	if res.ItemID != "i1" || !errors.Is(res.Err, ErrAccountInactive) || res.Replayed {
		t.Fatalf("账户停用拒绝的重提结果异常: %+v", res)
	}
}

// TestTransferRejectSaveFailureSameAccount 覆盖：收发为同一账户的拒绝落盘
// 失败时返回保存错误；保存恢复后重提返回 ErrSameAccount。
func TestTransferRejectSaveFailureSameAccount(t *testing.T) {
	r := mustCreate(t, tempDir(t))
	setupBatchTransferWorld(t, r)
	req := xferSaveReq("rt-same", "i1", "bob", 1, "bob", 0)

	blockBatchSave(t, r)
	res, err := r.Transfer(req)
	assertTransferSaveFailureEmpty(t, res, err)
	assertXferRequestFree(t, r, req, 5)
	assertHoldingUnchanged(t, r, "i1", "bob", 1, 1)

	restoreBatchSave(t, r)
	res, err = r.Transfer(req)
	if !errors.Is(err, ErrSameAccount) {
		t.Fatalf("重提应在保存成功后返回 ErrSameAccount: %v", err)
	}
	if res.ItemID != "i1" || !errors.Is(res.Err, ErrSameAccount) || res.Replayed {
		t.Fatalf("收发同人拒绝的重提结果异常: %+v", res)
	}
}

// TestTransferValidationErrorIgnoresSaveFailure 覆盖：参数错误与引用不存在
// 不占用请求号、不要求保存拒绝结果，即使存储暂时不可写也仍返回原参数/
// 引用错误，不能改报保存错误。
func TestTransferValidationErrorIgnoresSaveFailure(t *testing.T) {
	r := mustCreate(t, tempDir(t))
	setupBatchTransferWorld(t, r)

	blockBatchSave(t, r)
	defer restoreBatchSave(t, r)

	// 引用不存在的藏品：返回 ErrNotFound，不是保存错误。
	notFoundReq := xferSaveReq("rt-nf", "nope", "bob", 1, "carol", 0)
	res, err := r.Transfer(notFoundReq)
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("引用不存在应返回 ErrNotFound，不能改报保存错误: %v", err)
	}
	if res.ItemID != "nope" || !errors.Is(res.Err, ErrNotFound) {
		t.Fatalf("引用不存在的结果异常: %+v", res)
	}
	assertXferRequestFree(t, r, notFoundReq, 5)

	// 参数错误（负价款）：返回 ErrInvalidArgument，不是保存错误。
	badReq := xferSaveReq("rt-bad", "i1", "bob", 1, "carol", -1)
	if _, err := r.Transfer(badReq); !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("参数错误应返回 ErrInvalidArgument，不能改报保存错误: %v", err)
	}
	assertXferRequestFree(t, r, badReq, 5)
}
