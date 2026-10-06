package registry

import (
	"errors"
	"os"
	"reflect"
	"testing"
)

// 本文件为单件直接转让补充"成功结果保存失败"场景的回归保障，与创建授权
// （authorization_save_failure_test.go）行为对齐：业务检查全部通过后，
// 本次内容尚未替换原有数据就发生写入失败时，必须返回实际保存错误与空
// 结果，不能报告转让成功；即使失败后原数据暂时无法读取、状态未能按磁盘
// 重建，同一个仍打开的登记册中藏品仍属提交前的持有人，版本不增加，持有
// 列表与藏品历史保持原状，版税查询没有本次转让的计算记录，收款账户不
// 多出应付，请求号与历史序号都不被消耗；失败前已存在的成功转让、应付
// 与其他藏品记录原样保留。读写条件恢复后，其他操作成功保存不会把这笔
// 未保存的转让一并写入；用完全相同的请求重提按当时状态重新判断，条件
// 仍满足则正常完成一次转让（不标回放），此后相同请求才回放已保存的
// 结果；恢复期间藏品已被另一笔合法转让转出时，原请求按版本冲突拒绝。
//
// 失败注入方式与其他场景共用：在临时文件路径 .registry.json.tmp 上预先
// 建一个目录，save 在 OpenFile 阶段即以 EISDIR 失败，原快照完好可读；
// 另有用例同时破坏数据文件，覆盖"原数据暂时无法读取"的情形。

// xferSuccessSaveReq 是各用例共用的成功转让请求：bob 把 i1（s1，版税
// carol 10% + dave 5%）以 10000 分转给 dave，期望持有人 bob、版本 1。
func xferSuccessSaveReq() TransferRequest {
	return xferSaveReq("rt-success", "i1", "bob", 1, "dave", 10000)
}

// assertTransferStillPending 在（失败返回后的）同一个已打开登记册上核对：
// 本次转让如同从未发生——藏品仍属 bob 版本 1、历史只有发行一条、请求号
// 与历史序号未被消耗、版税查询没有本次转让的计算记录、收款账户没有多出
// 应付；失败前已有的记录（i5 属 carol）原样保留。
func assertTransferStillPending(t *testing.T, r *Registry, req TransferRequest) {
	t.Helper()
	assertXferRequestFree(t, r, req, 5)
	assertHoldingUnchanged(t, r, "i1", "bob", 1, 1)
	assertHoldingUnchanged(t, r, "i5", "carol", 1, 1)

	// 版税查询不能出现本次转让的计算记录：发行占序号 1..5，本次转让若
	// 留下记录会占用序号 6。
	if _, err := r.TransferRoyalty(6); !errors.Is(err, ErrNotFound) {
		t.Fatalf("未保存的转让不应留下版税计算记录: %v", err)
	}
	// 收款账户不能多出应付。
	for _, payee := range []string{"carol", "dave"} {
		pay, err := r.PayablesOf(payee)
		if err != nil {
			t.Fatal(err)
		}
		if len(pay) != 0 {
			t.Fatalf("未保存的转让不应给 %s 留下应付: %+v", payee, pay)
		}
	}
	// 接收账户不能因未保存的转让多持藏品。
	holdings, err := r.HoldingsOf("dave")
	if err != nil {
		t.Fatal(err)
	}
	if len(holdings) != 0 {
		t.Fatalf("未保存的转让不应改变 dave 的持有列表: %+v", holdings)
	}
}

// assertTransferRetrySuccess 核对保存恢复后用原请求重提正常完成一次转让：
// 不标回放，版本只增加一次，历史序号为紧接已有历史的 6，金额按 s1 既有
// 规则计算；此后相同请求回放这一笔已保存的结果。
func assertTransferRetrySuccess(t *testing.T, r *Registry, req TransferRequest) {
	t.Helper()
	res, err := r.Transfer(req)
	if err != nil {
		t.Fatalf("保存恢复后用原请求重提应正常完成一次转让: %v", err)
	}
	if res.Replayed || res.Err != nil || res.ItemID != "i1" ||
		res.FromID != "bob" || res.ToID != "dave" || res.Version != 2 || res.TxSeq != 6 {
		t.Fatalf("重提结果异常: %+v", res)
	}
	wantPayables := []RoyaltyPayable{
		{AccountID: "carol", Rate: 1000, Amount: 1000},
		{AccountID: "dave", Rate: 500, Amount: 500},
	}
	if res.Price != 10000 || res.Remainder != 8500 || !reflect.DeepEqual(res.Payables, wantPayables) {
		t.Fatalf("重提金额异常: %+v", res)
	}

	// 版本只增加一次，历史只新增本条转让，序号紧接已有历史。
	assertHoldingUnchanged(t, r, "i1", "dave", 2, 2)
	if r.state.NextSeq != 6 {
		t.Fatalf("NextSeq = %d, want 6", r.state.NextSeq)
	}
	tr, err := r.TransferRoyalty(6)
	if err != nil {
		t.Fatalf("TransferRoyalty 6: %v", err)
	}
	if tr.ItemID != "i1" || tr.Price != 10000 || tr.Remainder != 8500 || tr.OwnerID != "bob" ||
		!reflect.DeepEqual(tr.Payables, wantPayables) {
		t.Fatalf("版税记录异常: %+v", tr)
	}

	// 此次成功保存后再次提交相同内容：回放这一笔已保存的结果，不再执行。
	replay, err := r.Transfer(req)
	if err != nil {
		t.Fatalf("成功后重提应回放: %v", err)
	}
	if !replay.Replayed || replay.ItemID != "i1" || replay.FromID != "bob" ||
		replay.ToID != "dave" || replay.Version != 2 || replay.TxSeq != 6 ||
		replay.Price != 10000 || replay.Remainder != 8500 ||
		!reflect.DeepEqual(replay.Payables, wantPayables) {
		t.Fatalf("回放内容与已保存结果不一致: %+v", replay)
	}
	assertHoldingUnchanged(t, r, "i1", "dave", 2, 2)
}

// TestTransferSuccessSaveFailureSameRegistry 覆盖核心场景：成功转让的落盘
// 失败（原数据仍可读取）时返回保存错误与空结果；同一个仍打开的登记册中
// 持有、历史、版税与请求号都停留在提交前；保存条件恢复后，无关操作成功
// 保存不会带入这笔未保存的转让，用原请求重提正常完成一次转让。
func TestTransferSuccessSaveFailureSameRegistry(t *testing.T) {
	r := mustCreate(t, tempDir(t))
	setupBatchTransferWorld(t, r)
	req := xferSuccessSaveReq()

	// 业务参数完全合规（账户登记可用、收发不同人、期望持有人与版本相符、
	// 价款非负），失败只可能来自写入阶段。
	blockBatchSave(t, r)
	res, err := r.Transfer(req)
	assertTransferSaveFailureEmpty(t, res, err)
	assertTransferStillPending(t, r, req)

	// 保存条件未恢复时再次提交同一请求：仍重新尝试保存并失败，不能把
	// 上次未保存的转让当成已保存的成功回放。
	res, err = r.Transfer(req)
	assertTransferSaveFailureEmpty(t, res, err)
	assertTransferStillPending(t, r, req)

	// 保存条件恢复后，先登记另一个账户让无关操作成功保存：不能把这笔
	// 未保存的转让一并写入。
	restoreBatchSave(t, r)
	if err := r.RegisterAccount("erin", "路人"); err != nil {
		t.Fatal(err)
	}
	assertTransferStillPending(t, r, req)

	// 用原操作者、请求号、原因和全部转让参数重新提交：条件仍满足，正常
	// 完成一次转让；此后相同请求回放已保存的结果。
	assertTransferRetrySuccess(t, r, req)
}

// TestTransferSuccessSaveFailureUnreadableDisk 覆盖：保存失败且原数据
// 暂时无法读取（状态未能按磁盘重建）时，未保存的持有变化、转让历史、
// 版税应付与请求号占用也不能留在当前登记册中；恢复正常读写后，其他
// 操作成功保存不带入这次未保存的内容，原请求重提正常完成一次转让。
func TestTransferSuccessSaveFailureUnreadableDisk(t *testing.T) {
	r := mustCreate(t, tempDir(t))
	setupBatchTransferWorld(t, r)
	req := xferSuccessSaveReq()

	// 让保存失败，同时让原数据暂时无法读取：commit 无法按磁盘重建状态。
	orig, err := os.ReadFile(dataFile(r.dir))
	if err != nil {
		t.Fatal(err)
	}
	blockBatchSave(t, r)
	if err := os.WriteFile(dataFile(r.dir), []byte("{corrupt"), fileMode); err != nil {
		t.Fatal(err)
	}

	res, err := r.Transfer(req)
	assertTransferSaveFailureEmpty(t, res, err)
	// 状态未能重建时也必须显式撤销：同一个仍打开的登记册中藏品仍属
	// 提交前的持有人，历史、版税、请求号与历史序号都保持提交前状态。
	assertTransferStillPending(t, r, req)

	// 恢复正常读写。
	if err := os.WriteFile(dataFile(r.dir), orig, fileMode); err != nil {
		t.Fatal(err)
	}
	restoreBatchSave(t, r)

	// 其他操作成功保存不能把这次未保存的转让或请求号占用带入登记册。
	if err := r.RegisterAccount("erin", "路人"); err != nil {
		t.Fatal(err)
	}
	assertTransferStillPending(t, r, req)

	// 用完全相同的请求重提：正常完成一次转让，不标回放。
	assertTransferRetrySuccess(t, r, req)
}

// TestTransferSuccessSaveFailureRetryAfterItemMoved 覆盖：未保存的成功
// 不阻碍后续重提——保存条件恢复后，藏品被另一笔合法转让转出，用完全
// 相同的原请求重提应按现有版本冲突规则拒绝（保存此次拒绝后返回
// ErrConflict），不能回放先前未保存的成功。
func TestTransferSuccessSaveFailureRetryAfterItemMoved(t *testing.T) {
	r := mustCreate(t, tempDir(t))
	setupBatchTransferWorld(t, r)
	req := xferSuccessSaveReq()

	blockBatchSave(t, r)
	res, err := r.Transfer(req)
	assertTransferSaveFailureEmpty(t, res, err)
	assertTransferStillPending(t, r, req)

	// 保存条件恢复后，bob 把 i1 合法转让给 carol：i1 变为 carol 版本 2，
	// 原请求期望的持有人与版本不再满足。
	restoreBatchSave(t, r)
	if _, err := r.Transfer(TransferRequest{
		Operator: "bob", Reason: "合法转让", RequestID: "rt-other", ItemID: "i1",
		ExpectedOwner: "bob", ExpectedVer: 1, ToID: "carol",
	}); err != nil {
		t.Fatal(err)
	}

	// 用完全相同的原请求重提：按当前业务状态判断，保存此次拒绝后返回
	// ErrConflict，不能回放先前未保存的成功。
	res, err = r.Transfer(req)
	if !errors.Is(err, ErrConflict) {
		t.Fatalf("藏品已易手后重提应按版本冲突拒绝: %v", err)
	}
	if res.Replayed || res.ItemID != "i1" || !errors.Is(res.Err, ErrConflict) ||
		res.Version != 0 || res.TxSeq != 0 {
		t.Fatalf("冲突拒绝的重提结果异常: %+v", res)
	}
	assertHoldingUnchanged(t, r, "i1", "carol", 2, 2)

	// 拒绝已保存：相同内容重提回放原拒绝。
	res, err = r.Transfer(req)
	if !errors.Is(err, ErrConflict) || !res.Replayed || res.ItemID != "i1" ||
		!errors.Is(res.Err, ErrConflict) {
		t.Fatalf("保存后的重提应回放原拒绝: %+v, err %v", res, err)
	}
}
