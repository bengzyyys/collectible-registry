package registry

import (
	"errors"
	"os"
	"testing"
)

// 本文件为整批转让补充"成功整批本身保存失败且原数据暂时无法读取"场景的
// 回归保障。既有的 batch_transfer_save_failure_test.go 覆盖原登记册仍可正常
// 读取（commit 能按磁盘重建状态）的保存失败，以及状态类业务拒绝的落盘失败；
// 这里覆盖业务检查全部通过后，本次整批的持有换人/历史/版税应付/请求结果尚未
// 原子替换原数据就发生写入错误，且原数据同时被改写为无法解析、commit 无法按
// 磁盘重建状态的情形——这批转让实际没有保存，绝不能在同一个仍打开的登记册中
// 留下"已换人、历史与应付已增加、原请求可被回放"的幻影整批。
//
// 失败返回必须保留实际写入错误并给出空结果（无转让条目、无失败藏品编号、
// 业务错误为空、不标回放）。同一个仍打开的登记册上：清单内每件仍归提交前
// 持有人、版本不增加、持有列表与藏品历史保持原样、按本次序号查版税返回对象
// 不存在、收款账户不多出应付，请求号与历史序号都不被消耗；失败前已保存的
// 历史、应付、系列规则与清单外藏品的持有和版本完整保留。保存条件未恢复时用
// 完全相同的请求再次提交仍实际尝试保存并返回当次保存错误，不能回放未保存的
// 成功；随后另一项无关操作成功保存也不把这批未保存的变化写入。读写恢复后用
// 原请求号、原因和完整清单重提，条件仍满足时完整执行一次（各件版本只加一次、
// 历史序号紧接已有记录、不标回放），此后相同提交才回放；恢复期间其中一件被
// 另一笔合法转让转出的，原整批按现有持有版本规则拒绝，其余条目不发生转让。
//
// 失败注入与其他场景共用：在 .registry.json.tmp 上预建目录使 save 在 rename
// 之前以 EISDIR 失败，同时把 registry.json 改写为无法解析的内容，使 commit 的
// 重新加载也失败；删除该目录并还原数据文件即恢复读写条件。

// corruptDataFile 把登记册数据文件改写为无法解析的内容，模拟旧数据仍在却
// 暂时无法读取或解析；返回改写前的原始内容供恢复。
func corruptDataFile(t *testing.T, r *Registry) []byte {
	t.Helper()
	orig, err := os.ReadFile(dataFile(r.dir))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dataFile(r.dir), []byte("{corrupt"), fileMode); err != nil {
		t.Fatal(err)
	}
	return orig
}

// restoreDataFile 用此前保存的原始内容恢复数据文件。
func restoreDataFile(t *testing.T, r *Registry, orig []byte) {
	t.Helper()
	if err := os.WriteFile(dataFile(r.dir), orig, fileMode); err != nil {
		t.Fatal(err)
	}
}

// assertBatchNoNewRoyalties 核对本次整批计算出的序号（8/9/10）都没有留下
// 版税应付记录：按转让序号查询返回 ErrNotFound。
func assertBatchNoNewRoyalties(t *testing.T, r *Registry) {
	t.Helper()
	for _, seq := range []int64{8, 9, 10} {
		if _, err := r.TransferRoyalty(seq); !errors.Is(err, ErrNotFound) {
			t.Fatalf("未保存整批不应留下序号 %d 的版税记录: %v", seq, err)
		}
	}
}

// TestTransferBatchSaveFailureUnreadableSameRegistry 覆盖核心场景：业务检查
// 全部通过后的整批转让在写入阶段失败，且原数据同时无法解析（commit 无法按
// 磁盘重建状态）。返回保存错误与空结果；同一登记册上整批如同从未发生，旧
// 记录完整。保存仍失败时重提依旧实际尝试保存并返回当次保存错误，不回放。
// 读写恢复后先让一次无关操作成功保存，也不把未保存整批带入；随后原请求重提
// 按当前状态完整执行一次（非回放、序号 8/9/10），再提才回放。
func TestTransferBatchSaveFailureUnreadableSameRegistry(t *testing.T) {
	r := mustCreate(t, tempDir(t))
	seedBatchSaveFailureWorld(t, r)
	req := batchSaveFailureReq()

	orig := corruptDataFile(t, r)
	blockBatchSave(t, r)

	// 整批落盘失败：返回保存错误与空结果，而不是成功、持有冲突或重新读取
	// 时的错误。
	res, err := r.TransferBatch(req)
	assertSaveFailureError(t, err)
	if len(res.Items) != 0 || res.ItemID != "" || res.Replayed || res.Err != nil {
		t.Fatalf("保存失败必须整体失败，不能返回任何一件已转让成功的结果: %+v", res)
	}

	// 同一个已打开的登记册上立即查询（磁盘仍不可读）：整批从未发生，旧记录
	// 完整保留；本次序号没有版税记录。
	assertBatchStillPending(t, r)
	assertBatchNoNewRoyalties(t, r)

	// 保存条件尚未恢复、磁盘仍不可读时用完全相同的请求再次提交：仍失败在
	// 保存上并返回当次保存错误，不能把上次未保存的成功当成已保存结果回放。
	res, err = r.TransferBatch(req)
	assertSaveFailureError(t, err)
	if len(res.Items) != 0 || res.ItemID != "" || res.Replayed || res.Err != nil {
		t.Fatalf("保存未恢复时重提必须再次整体失败: %+v", res)
	}
	assertBatchStillPending(t, r)
	assertBatchNoNewRoyalties(t, r)

	// 恢复正常读写；先让一次无关操作成功保存，不能夹带这批未保存的转让。
	restoreDataFile(t, r, orig)
	restoreBatchSave(t, r)
	if err := r.RegisterAccount("eve", "路人"); err != nil {
		t.Fatalf("无关操作应能成功保存: %v", err)
	}
	assertBatchStillPending(t, r)
	assertBatchNoNewRoyalties(t, r)

	// 用原请求号、原因和完整清单重提：账户与持有条件仍满足，整批完整执行
	// 一次，不标回放，各件版本只加一次，历史序号紧接已有记录（8/9/10）。
	assertBatchRetrySuccess(t, r, req)
}

// TestTransferBatchSaveFailureUnreadableRetryConflict 覆盖：恢复期间清单内
// 一件被另一笔合法转让转出后，原整批必须按现有持有版本规则拒绝（报告清单
// 顺序最前的失败藏品），其余条目不能发生转让；不能回放那次未保存的成功。
// 拒绝保存成功后，原样重提才回放该拒绝。
func TestTransferBatchSaveFailureUnreadableRetryConflict(t *testing.T) {
	r := mustCreate(t, tempDir(t))
	seedBatchSaveFailureWorld(t, r)
	req := batchSaveFailureReq()

	orig := corruptDataFile(t, r)
	blockBatchSave(t, r)
	res, err := r.TransferBatch(req)
	assertSaveFailureError(t, err)
	if len(res.Items) != 0 || res.Replayed {
		t.Fatalf("保存失败必须整体失败: %+v", res)
	}
	assertBatchStillPending(t, r)
	assertBatchNoNewRoyalties(t, r)

	// 恢复读写后，bob 先把清单第一件 i1 合法转让给 alice：i1 变为 alice
	// 版本 2（序号 8）。
	restoreDataFile(t, r, orig)
	restoreBatchSave(t, r)
	if _, err := r.Transfer(TransferRequest{
		Operator: "bob", Reason: "恢复期间的合法转让", RequestID: "rt-intervene", ItemID: "i1",
		ExpectedOwner: "bob", ExpectedVer: 1, ToID: "alice", Price: 6000,
	}); err != nil {
		t.Fatal(err)
	}

	// 原整批（i1 仍期望 bob 版本 1）按清单顺序在 i1 上即被持有版本规则拒绝，
	// 不是回放未保存的成功；其余条目不能发生转让。
	res, err = r.TransferBatch(req)
	if !errors.Is(err, ErrConflict) {
		t.Fatalf("清单内藏品已转出后原整批应按持有版本规则拒绝: %v", err)
	}
	if res.Replayed || res.ItemID != "i1" || !errors.Is(res.Err, ErrConflict) ||
		len(res.Items) != 0 {
		t.Fatalf("冲突拒绝的结果异常: %+v", res)
	}
	// i1 保持合法转让后的状态；i2/i3 仍是原持有人 bob、版本 1，没有被整批
	// 顺带转出。
	if h, _ := r.GetHolding("i1"); h.OwnerID != "alice" || h.Version != 2 {
		t.Fatalf("i1 持有异常: %+v", h)
	}
	for _, id := range []string{"i2", "i3"} {
		if h, _ := r.GetHolding(id); h.OwnerID != "bob" || h.Version != 1 {
			t.Fatalf("整批拒绝后 %s 不应发生转让: %+v", id, h)
		}
		if hist, _ := r.History(id); len(hist) != 1 || hist[0].Kind != "issue" {
			t.Fatalf("整批拒绝后 %s 历史不应增加: %+v", id, hist)
		}
	}
	// 整批拒绝不消耗历史序号：最后一条仍是合法转让的序号 8。
	if r.state.NextSeq != 8 {
		t.Fatalf("整批拒绝不应消耗历史序号: NextSeq=%d", r.state.NextSeq)
	}

	// 该状态类拒绝保存成功后，原样重提回放这条拒绝，仍不执行任何转让。
	res, err = r.TransferBatch(req)
	if !errors.Is(err, ErrConflict) || !res.Replayed || res.ItemID != "i1" ||
		!errors.Is(res.Err, ErrConflict) {
		t.Fatalf("拒绝保存后再提应回放该拒绝: %+v, err %v", res, err)
	}
	if h, _ := r.GetHolding("i2"); h.OwnerID != "bob" || h.Version != 1 {
		t.Fatalf("回放拒绝不应改变持有: %+v", h)
	}
}
