package registry

import (
	"errors"
	"fmt"
	"syscall"
	"testing"
)

// 本文件覆盖整批转让"状态类业务拒绝的保存失败"场景：请求已因持有状态
// 冲突、账户停用或收发同人被判定拒绝，但这次拒绝结果在替换原数据之前
// 落盘失败（原登记册仍可正常读取）时，返回必须与整批成功落盘失败一致：
// 明确的保存错误（保留实际写入错误）、空结果、业务错误为空，不标为
// 重复回放；请求号不被占用，全部状态停留在提交前。保存恢复后用完全
// 相同的请求重提，必须按当前业务状态重新判断。
//
// 失败注入沿用 batch_transfer_save_failure_test.go：在临时文件路径上
// 预建目录，save 在 OpenFile 阶段即以 EISDIR 失败，原 registry.json
// 从未被替换。

// assertRejectionSaveFailureResult 核对拒绝保存失败时的返回：error 是
// 保存错误而非任何业务拒绝，且保留底层写入错误（EISDIR）；结果为空、
// 没有失败藏品编号、不是回放、业务错误为空。
func assertRejectionSaveFailureResult(t *testing.T, res TransferBatchResult, err error) {
	t.Helper()
	assertSaveFailureError(t, err)
	if !errors.Is(err, syscall.EISDIR) {
		t.Fatalf("保存错误必须保留实际写入错误 EISDIR: %v", err)
	}
	if len(res.Items) != 0 || res.ItemID != "" || res.Replayed || res.Err != nil {
		t.Fatalf("拒绝保存失败必须与整批保存失败同样返回空结果: %+v", res)
	}
}

// assertBatchRejectionStillPending 在拒绝保存失败返回后核对：整批如同
// 从未提交，且没有任何拒绝结果被记住。setupBatchTransferWorld 之后最后
// 一条历史序号是 5。
func assertBatchRejectionStillPending(t *testing.T, r *Registry, req TransferBatchRequest) {
	t.Helper()
	for _, e := range req.Entries {
		h, err := r.GetHolding(e.ItemID)
		if err != nil {
			t.Fatalf("GetHolding %s: %v", e.ItemID, err)
		}
		// 全部藏品仍是发行时的 bob、版本 1。
		if h.OwnerID != "bob" || h.Version != 1 {
			t.Fatalf("保存失败后 %s 持有被改变: %+v", e.ItemID, h)
		}
		hist, err := r.History(e.ItemID)
		if err != nil {
			t.Fatalf("History %s: %v", e.ItemID, err)
		}
		if len(hist) != 1 || hist[0].Kind != "issue" {
			t.Fatalf("保存失败后 %s 历史多出本次请求: %+v", e.ItemID, hist)
		}
	}
	if r.state.NextSeq != 5 {
		t.Fatalf("NextSeq = %d，拒绝保存失败不应消耗历史序号，仍应为 5", r.state.NextSeq)
	}
	if _, ok := r.state.Requests[requestKey(req.Operator, req.RequestID)]; ok {
		t.Fatal("未保存的拒绝不应占用请求号")
	}
	// 没有任何转让成功过：收款人没有应付明细。
	if ps, err := r.PayablesOf("carol"); err != nil || len(ps) != 0 {
		t.Fatalf("拒绝保存失败后 carol 应付 = %+v, err %v，应为空", ps, err)
	}
	if ps, err := r.PayablesOf("dave"); err != nil || len(ps) != 0 {
		t.Fatalf("拒绝保存失败后 dave 应付 = %+v, err %v，应为空", ps, err)
	}
}

// advanceI1ToBobVer9 通过 8 笔合法转让让 i1 经 alice 转手后回到 bob
// 手中、版本恰好为 9（v2 alice、v3 bob……v9 bob），使原本"期望 bob
// 版本 9"的冲突请求变为完全合法。
func advanceI1ToBobVer9(t *testing.T, r *Registry) {
	t.Helper()
	owner, ver, to := "bob", int64(1), "alice"
	for k := 0; k < 8; k++ {
		if _, err := r.Transfer(TransferRequest{
			Operator: owner, Reason: "调整到期望版本", RequestID: fmt.Sprintf("rx-move-%d", k),
			ItemID: "i1", ExpectedOwner: owner, ExpectedVer: ver, ToID: to,
		}); err != nil {
			t.Fatalf("第 %d 笔调整转让: %v", k+1, err)
		}
		ver++
		owner, to = to, owner
	}
	h, err := r.GetHolding("i1")
	if err != nil {
		t.Fatal(err)
	}
	if h.OwnerID != "bob" || h.Version != 9 {
		t.Fatalf("调整后 i1 应为 bob 版本 9，得到 %+v", h)
	}
}

// TestTransferBatchRejectedSaveFailureRejudges 覆盖核心场景：持有版本
// 冲突的首次拒绝保存失败后，调用者拿到保存错误而非 ErrConflict，请求号
// 未被占用；保存恢复且藏品经合法转让恰好变为请求期望的持有人与版本后，
// 用原请求重提必须正常执行（不回放那次未保存的冲突），成功后再重提才
// 回放首次成功结果。
func TestTransferBatchRejectedSaveFailureRejudges(t *testing.T) {
	r := mustCreate(t, tempDir(t))
	setupBatchTransferWorld(t, r)
	// i1 期望版本 9 与当前版本 1 冲突；i2 本身合法，整批仍拒绝且报 i1。
	req := tbtReq("rb-rej",
		tbtEntry("i1", "carol", 9, 10000),
		tbtEntry("i2", "dave", 1, 333),
	)

	blockBatchSave(t, r)
	res, err := r.TransferBatch(req)
	assertRejectionSaveFailureResult(t, res, err)
	assertBatchRejectionStillPending(t, r, req)

	// 保存条件恢复后，先让 i1 经合法转让变为请求期望的 bob 版本 9。
	restoreBatchSave(t, r)
	advanceI1ToBobVer9(t, r)

	// 用完全相同的请求重提：拒绝从未保存，按当前状态判断已合法，整批
	// 完整执行一次而非回放冲突。8 笔调整转让占用序号 6..13，整批为
	// 14（i1）与 15（i2）。
	res, err = r.TransferBatch(req)
	if err != nil {
		t.Fatalf("状态变合法后原请求应正常执行: %v", err)
	}
	if res.Replayed || res.Err != nil || res.ItemID != "" || len(res.Items) != 2 {
		t.Fatalf("重提应完整执行一次，得到 %+v", res)
	}
	if w := res.Items[0]; w.ItemID != "i1" || w.FromID != "bob" || w.ToID != "carol" ||
		w.Version != 10 || w.TxSeq != 14 {
		t.Fatalf("i1 结果异常: %+v", w)
	}
	if w := res.Items[1]; w.ItemID != "i2" || w.FromID != "bob" || w.ToID != "dave" ||
		w.Version != 2 || w.TxSeq != 15 {
		t.Fatalf("i2 结果异常: %+v", w)
	}
	h1, _ := r.GetHolding("i1")
	if h1.OwnerID != "carol" || h1.Version != 10 {
		t.Fatalf("执行后 i1 持有 = %+v", h1)
	}

	// 成功已保存：再用相同请求重提回放首次成功，不再第二次执行。
	replay, err := r.TransferBatch(req)
	if err != nil {
		t.Fatalf("成功后重提应回放: %v", err)
	}
	if !replay.Replayed || len(replay.Items) != 2 ||
		replay.Items[0].TxSeq != 14 || replay.Items[1].TxSeq != 15 {
		t.Fatalf("成功后回放异常: %+v", replay)
	}
	h, _ := r.GetHolding("i1")
	if h.Version != 10 {
		t.Fatalf("回放不应再次增加版本: %+v", h)
	}
}

// TestTransferBatchRejectedSaveFailureConditionPersists 覆盖拒绝条件
// 仍然存在的分支：保存恢复后用原请求重提，重新保存这次拒绝；保存成功
// 后才返回业务错误。此后相同请求按既有规则回放拒绝（状态再变化也不
// 重新执行）。
func TestTransferBatchRejectedSaveFailureConditionPersists(t *testing.T) {
	r := mustCreate(t, tempDir(t))
	setupBatchTransferWorld(t, r)
	req := tbtReq("rb-rej", tbtEntry("i1", "carol", 9, 1000))

	blockBatchSave(t, r)
	res, err := r.TransferBatch(req)
	assertRejectionSaveFailureResult(t, res, err)
	assertBatchRejectionStillPending(t, r, req)

	// 保存恢复但冲突仍在：重新保存拒绝，成功后返回业务错误（首次保存
	// 成功，不标回放）。
	restoreBatchSave(t, r)
	res, err = r.TransferBatch(req)
	if !errors.Is(err, ErrConflict) {
		t.Fatalf("拒绝条件仍存在时应返回 ErrConflict: %v", err)
	}
	if res.Replayed || res.ItemID != "i1" || !errors.Is(res.Err, ErrConflict) {
		t.Fatalf("重新保存的首次拒绝结果异常: %+v", res)
	}

	// 拒绝现已落盘：相同请求重提回放原拒绝。
	replay, err := r.TransferBatch(req)
	if !errors.Is(err, ErrConflict) || !replay.Replayed || replay.ItemID != "i1" {
		t.Fatalf("已保存拒绝应被回放: res=%+v err=%v", replay, err)
	}

	// 即使之后状态真的变成期望的 bob 版本 9，已保存的拒绝仍被回放，
	// 不重新执行。
	advanceI1ToBobVer9(t, r)
	replay, err = r.TransferBatch(req)
	if !errors.Is(err, ErrConflict) || !replay.Replayed {
		t.Fatalf("拒绝保存成功后状态变化也不应重新执行: res=%+v err=%v", replay, err)
	}
	h, _ := r.GetHolding("i1")
	if h.OwnerID != "bob" || h.Version != 9 {
		t.Fatalf("回放拒绝不应改变持有: %+v", h)
	}
}

// TestTransferBatchInactiveOperatorRejectedSaveFailure 覆盖操作者账户
// 停用的拒绝保存失败：返回保存错误且不附藏品编号；恢复后操作者仍停用，
// 重新保存拒绝并返回 ErrAccountInactive（ItemID 为空）。
func TestTransferBatchInactiveOperatorRejectedSaveFailure(t *testing.T) {
	r := mustCreate(t, tempDir(t))
	setupBatchTransferWorld(t, r)
	if err := r.DeactivateAccount("bob"); err != nil {
		t.Fatal(err)
	}
	req := tbtReq("rb-ina", tbtEntry("i1", "carol", 1, 0), tbtEntry("i2", "dave", 1, 0))

	blockBatchSave(t, r)
	res, err := r.TransferBatch(req)
	assertRejectionSaveFailureResult(t, res, err)
	assertBatchRejectionStillPending(t, r, req)

	restoreBatchSave(t, r)
	res, err = r.TransferBatch(req)
	if !errors.Is(err, ErrAccountInactive) {
		t.Fatalf("操作者仍停用应返回 ErrAccountInactive: %v", err)
	}
	if res.Replayed || res.ItemID != "" || !errors.Is(res.Err, ErrAccountInactive) {
		t.Fatalf("操作者账户错误不附藏品编号: %+v", res)
	}
	// 再次重提回放已保存的拒绝，仍不带藏品编号。
	replay, err := r.TransferBatch(req)
	if !errors.Is(err, ErrAccountInactive) || !replay.Replayed || replay.ItemID != "" {
		t.Fatalf("应回放操作者停用拒绝: %+v %v", replay, err)
	}
}

// TestTransferBatchSameAccountRejectedSaveFailure 覆盖收发同人拒绝的
// 保存失败与恢复后重新保存。
func TestTransferBatchSameAccountRejectedSaveFailure(t *testing.T) {
	r := mustCreate(t, tempDir(t))
	setupBatchTransferWorld(t, r)
	req := tbtReq("rb-same", tbtEntry("i1", "bob", 1, 0))

	blockBatchSave(t, r)
	res, err := r.TransferBatch(req)
	assertRejectionSaveFailureResult(t, res, err)
	assertBatchRejectionStillPending(t, r, req)

	restoreBatchSave(t, r)
	res, err = r.TransferBatch(req)
	if !errors.Is(err, ErrSameAccount) || res.Replayed || res.ItemID != "i1" {
		t.Fatalf("恢复后应重新保存收发同人拒绝: res=%+v err=%v", res, err)
	}
}

// TestTransferBatchRejectedSaveFailureRetryAfterReopen 覆盖磁盘视角：
// 拒绝保存失败后原登记册仍可正常读取，重开后看不到这次拒绝；保存恢复
// 且状态变合法时，在重开的登记册上用原请求同样完整执行一次。
func TestTransferBatchRejectedSaveFailureRetryAfterReopen(t *testing.T) {
	dir := tempDir(t)
	r := mustCreate(t, dir)
	setupBatchTransferWorld(t, r)
	req := tbtReq("rb-rej", tbtEntry("i1", "carol", 9, 10000))

	blockBatchSave(t, r)
	res, err := r.TransferBatch(req)
	assertRejectionSaveFailureResult(t, res, err)

	if err := r.Close(); err != nil {
		t.Fatal(err)
	}
	r2, err := Open(dir)
	if err != nil {
		t.Fatalf("失败后原登记册必须仍能正常读取: %v", err)
	}
	t.Cleanup(func() { _ = r2.Close() })
	assertBatchRejectionStillPending(t, r2, req)

	restoreBatchSave(t, r2)
	advanceI1ToBobVer9(t, r2)
	res, err = r2.TransferBatch(req)
	if err != nil || res.Replayed || len(res.Items) != 1 {
		t.Fatalf("重开后状态合法的原请求应完整执行一次: %+v %v", res, err)
	}
	if w := res.Items[0]; w.ToID != "carol" || w.Version != 10 {
		t.Fatalf("执行结果异常: %+v", w)
	}
}

// TestTransferBatchValidationErrorsIgnoreSaveFailure 覆盖保留规则：
// 参数错误与引用不存在不占用请求号、不要求保存拒绝结果，存储暂时不可写
// 时仍返回原参数/引用错误，不能改报保存错误。
func TestTransferBatchValidationErrorsIgnoreSaveFailure(t *testing.T) {
	r := mustCreate(t, tempDir(t))
	setupBatchTransferWorld(t, r)
	blockBatchSave(t, r)
	t.Cleanup(func() { restoreBatchSave(t, r) })

	// 参数错误：负价款，在进入临界区前即拒绝，根本不尝试落盘。
	badPrice := tbtReq("rv-arg", tbtEntry("i1", "carol", 1, -1))
	res, err := r.TransferBatch(badPrice)
	if !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("参数错误仍应返回 ErrInvalidArgument，存储不可写也不改报保存错误: %v", err)
	}

	// 引用不存在：操作者未登记（不附藏品编号）。
	noOperator := tbtReq("rv-op", tbtEntry("i1", "carol", 1, 0))
	noOperator.Operator = "nobody"
	res, err = r.TransferBatch(noOperator)
	if !errors.Is(err, ErrNotFound) || res.ItemID != "" {
		t.Fatalf("操作者不存在仍应返回 ErrNotFound 且不附藏品编号: %+v %v", res, err)
	}

	// 引用不存在：藏品未登记（指出该件）。
	res, err = r.TransferBatch(tbtReq("rv-item",
		tbtEntry("i1", "carol", 1, 0),
		TransferBatchEntry{ItemID: "ghost", ToID: "dave", ExpectedOwner: "bob", ExpectedVer: 1}))
	if !errors.Is(err, ErrNotFound) || res.ItemID != "ghost" {
		t.Fatalf("藏品不存在仍应返回 ErrNotFound 并指出该件: %+v %v", res, err)
	}

	// 这些请求都不占用请求号。
	for _, k := range [][2]string{{"bob", "rv-arg"}, {"nobody", "rv-op"}, {"bob", "rv-item"}} {
		if _, ok := r.state.Requests[requestKey(k[0], k[1])]; ok {
			t.Fatalf("校验类请求 %s/%s 不应占用请求号", k[0], k[1])
		}
	}
}
