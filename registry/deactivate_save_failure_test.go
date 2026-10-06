package registry

import (
	"errors"
	"os"
	"testing"
)

// 本文件为账户停用补充"停用本身保存失败"场景的回归保障：对已登记且可用的
// 账户调用 DeactivateAccount，新状态尚未替换原数据就发生写入失败时，这样
// 的失败不能算作一次有效停用——必须返回实际保存错误而非成功；即使原数据
// 在失败后暂时无法读取或解析、状态未能按磁盘重建，同一个仍打开的登记册上
// 该账户的可用状态、编号与元数据也必须与调用前一致，其已有藏品的持有人、
// 版本与历史保持原状，其他账户此前已停用的状态不被恢复为可用。读写恢复后
// 该账户仍按原有规则参与转让；先做无关操作成功保存也不能夹带这次未保存的
// 停用；再次提交停用重新完成保存后才生效，此后按现有规则拒绝该账户发起或
// 接收新的转让，但其藏品与历史仍可查询。
//
// 失败注入与其他场景共用：在 .registry.json.tmp 上预建目录，save 在
// OpenFile 阶段即以 EISDIR 失败（rename 之前）；部分用例同时把数据文件
// 改写为无法解析的内容，覆盖"原数据暂时无法读取、commit 无法按磁盘重建
// 状态"。

// seedDeactivateWorld 在标准世界上把 dave 先成功停用，作为"其他账户此前
// 已停用的状态不得被恢复"的见证；bob 持有 i1..i4 且可用，是本次停用失败的
// 目标账户。
func seedDeactivateWorld(t *testing.T, r *Registry) {
	t.Helper()
	setupBatchTransferWorld(t, r)
	if err := r.DeactivateAccount("dave"); err != nil {
		t.Fatal(err)
	}
}

// assertAccountStillActive 在同一个已打开登记册上核对：未保存的停用如同
// 从未发生——账户可用、编号与元数据不变，其藏品的持有人、版本与历史保持
// 原状，此前已停用的 dave 仍是停用状态。
func assertAccountStillActive(t *testing.T, r *Registry, id, metadata string) {
	t.Helper()
	a, err := r.GetAccount(id)
	if err != nil {
		t.Fatalf("GetAccount %s: %v", id, err)
	}
	if !a.Active || a.ID != id || a.Metadata != metadata {
		t.Fatalf("保存失败后账户 %s 状态被改变: %+v", id, a)
	}
	dave, err := r.GetAccount("dave")
	if err != nil {
		t.Fatalf("GetAccount dave: %v", err)
	}
	if dave.Active {
		t.Fatalf("此前已停用的 dave 不应被恢复为可用: %+v", dave)
	}
	// bob 的既有藏品：持有人、版本与历史（各一条发行记录）保持原状。
	holdings, err := r.HoldingsOf(id)
	if err != nil {
		t.Fatalf("HoldingsOf %s: %v", id, err)
	}
	if len(holdings) != 4 {
		t.Fatalf("保存失败后 %s 持有清单异常: %+v", id, holdings)
	}
	for _, h := range holdings {
		if h.OwnerID != id || h.Version != 1 {
			t.Fatalf("保存失败后持有被改变: %+v", h)
		}
		hist, err := r.History(h.ItemID)
		if err != nil {
			t.Fatalf("History %s: %v", h.ItemID, err)
		}
		if len(hist) != 1 || hist[0].Kind != "issue" {
			t.Fatalf("保存失败后 %s 历史异常: %+v", h.ItemID, hist)
		}
	}
}

// TestDeactivateSaveFailureUnreadableSameRegistry 覆盖核心场景：停用写入
// 失败且原数据同时被改写为无法解析（commit 无法按磁盘重建状态）。调用返回
// 保存错误；同一登记册上账户仍可用、藏品与历史原状、dave 保持停用。保存
// 仍失败时再次停用依旧返回保存错误。读写恢复后账户可正常发起与接收转让；
// 先做无关操作成功保存不夹带未保存的停用；再次停用保存成功后才生效，此后
// 该账户发起或接收转让按 ErrAccountInactive 拒绝，藏品与历史仍可查询。
func TestDeactivateSaveFailureUnreadableSameRegistry(t *testing.T) {
	r := mustCreate(t, tempDir(t))
	seedDeactivateWorld(t, r)

	orig, err := os.ReadFile(dataFile(r.dir))
	if err != nil {
		t.Fatal(err)
	}
	blockBatchSave(t, r)
	if err := os.WriteFile(dataFile(r.dir), []byte("{corrupt"), fileMode); err != nil {
		t.Fatal(err)
	}

	// 停用落盘失败：返回保存错误而非成功。
	assertSaveFailureError(t, r.DeactivateAccount("bob"))
	assertAccountStillActive(t, r, "bob", "")

	// 保存条件未恢复、磁盘仍不可读时再次停用：仍失败在保存上，不留痕。
	assertSaveFailureError(t, r.DeactivateAccount("bob"))
	assertAccountStillActive(t, r, "bob", "")

	// 恢复正常读写；先让一次无关操作成功保存，不能夹带这次未保存的停用。
	if err := os.WriteFile(dataFile(r.dir), orig, fileMode); err != nil {
		t.Fatal(err)
	}
	restoreBatchSave(t, r)
	if err := r.RegisterAccount("eve", "路人"); err != nil {
		t.Fatalf("无关操作应能成功保存: %v", err)
	}
	assertAccountStillActive(t, r, "bob", "")

	// 读写恢复后该账户仍按原有规则参与转让：bob 转出 i1 给 carol 成功，
	// carol 转 i5 给 bob（作为接收人）也成功，不能收到 ErrAccountInactive。
	if _, err := r.Transfer(TransferRequest{
		Operator: "bob", Reason: "失败后仍可转出", RequestID: "rt-out", ItemID: "i1",
		ExpectedOwner: "bob", ExpectedVer: 1, ToID: "carol", Price: 100,
	}); err != nil {
		t.Fatalf("失败的停用不应影响发起转让: %v", err)
	}
	if _, err := r.Transfer(TransferRequest{
		Operator: "carol", Reason: "失败后仍可接收", RequestID: "rt-in", ItemID: "i5",
		ExpectedOwner: "carol", ExpectedVer: 1, ToID: "bob", Price: 100,
	}); err != nil {
		t.Fatalf("失败的停用不应影响接收转让: %v", err)
	}

	// 再次提交停用：重新完成保存，成功返回后才显示不可用。
	if err := r.DeactivateAccount("bob"); err != nil {
		t.Fatalf("恢复后停用应成功保存: %v", err)
	}
	a, err := r.GetAccount("bob")
	if err != nil || a.Active {
		t.Fatalf("停用保存成功后应显示不可用: %+v, err %v", a, err)
	}

	// 已停用账户发起或接收新的转让按现有规则拒绝。
	if _, err := r.Transfer(TransferRequest{
		Operator: "bob", Reason: "停用后转出", RequestID: "rt-out2", ItemID: "i2",
		ExpectedOwner: "bob", ExpectedVer: 1, ToID: "carol", Price: 100,
	}); !errors.Is(err, ErrAccountInactive) {
		t.Fatalf("停用后发起转让应报 ErrAccountInactive: %v", err)
	}
	if _, err := r.Transfer(TransferRequest{
		Operator: "carol", Reason: "停用后接收", RequestID: "rt-in2", ItemID: "i1",
		ExpectedOwner: "carol", ExpectedVer: 2, ToID: "bob", Price: 100,
	}); !errors.Is(err, ErrAccountInactive) {
		t.Fatalf("停用后接收转让应报 ErrAccountInactive: %v", err)
	}

	// 停用不改变藏品归属与持有版本，也不删除藏品与历史：查询仍可使用。
	holdings, err := r.HoldingsOf("bob")
	if err != nil {
		t.Fatalf("停用账户的持有列表仍可查询: %v", err)
	}
	if len(holdings) != 4 {
		t.Fatalf("停用不应改变持有清单: %+v", holdings)
	}
	h, err := r.GetHolding("i5")
	if err != nil || h.OwnerID != "bob" || h.Version != 2 {
		t.Fatalf("停用不应改变持有版本: %+v, err %v", h, err)
	}
	if hist, err := r.History("i5"); err != nil || len(hist) != 2 {
		t.Fatalf("停用不应删除历史: %+v, err %v", hist, err)
	}
}

// TestDeactivateSaveFailureReadableSameRegistry 覆盖：写入失败但原数据仍可
// 正常读取（commit 据磁盘内容重建状态）时，得到相同的保障——同一登记册上
// 账户仍可用、持有与历史原状；恢复后再次停用保存成功才生效。
func TestDeactivateSaveFailureReadableSameRegistry(t *testing.T) {
	r := mustCreate(t, tempDir(t))
	seedDeactivateWorld(t, r)

	// 只阻断写入，不破坏数据文件：rename 之前失败，原快照完好可读。
	blockBatchSave(t, r)
	assertSaveFailureError(t, r.DeactivateAccount("bob"))
	assertAccountStillActive(t, r, "bob", "")

	restoreBatchSave(t, r)
	if _, err := r.Transfer(TransferRequest{
		Operator: "bob", Reason: "失败后仍可转出", RequestID: "rt-out-r", ItemID: "i1",
		ExpectedOwner: "bob", ExpectedVer: 1, ToID: "carol", Price: 100,
	}); err != nil {
		t.Fatalf("失败的停用不应影响发起转让: %v", err)
	}
	if err := r.DeactivateAccount("bob"); err != nil {
		t.Fatalf("恢复后停用应成功保存: %v", err)
	}
	if a, _ := r.GetAccount("bob"); a.Active {
		t.Fatalf("停用保存成功后应显示不可用: %+v", a)
	}
	if _, err := r.Transfer(TransferRequest{
		Operator: "bob", Reason: "停用后转出", RequestID: "rt-out2-r", ItemID: "i2",
		ExpectedOwner: "bob", ExpectedVer: 1, ToID: "carol", Price: 100,
	}); !errors.Is(err, ErrAccountInactive) {
		t.Fatalf("停用后发起转让应报 ErrAccountInactive: %v", err)
	}
}

// TestDeactivateSaveFailureRetryAfterReopen 覆盖磁盘视角：停用保存失败
// （原数据可读）后关闭重开，看到的仍是停用前状态；恢复保存后再次停用
// 成功生效。
func TestDeactivateSaveFailureRetryAfterReopen(t *testing.T) {
	dir := tempDir(t)
	r := mustCreate(t, dir)
	t.Cleanup(func() { _ = r.Close() })
	seedDeactivateWorld(t, r)

	blockBatchSave(t, r)
	assertSaveFailureError(t, r.DeactivateAccount("bob"))

	// 数据文件从未被替换：正常关闭并重新打开仍读到停用前状态。
	if err := r.Close(); err != nil {
		t.Fatal(err)
	}
	r2, err := Open(dir)
	if err != nil {
		t.Fatalf("失败后原登记册必须仍能正常读取: %v", err)
	}
	t.Cleanup(func() { _ = r2.Close() })
	assertAccountStillActive(t, r2, "bob", "")

	restoreBatchSave(t, r2)
	if err := r2.DeactivateAccount("bob"); err != nil {
		t.Fatalf("重开后停用应成功保存: %v", err)
	}
	if a, _ := r2.GetAccount("bob"); a.Active {
		t.Fatalf("停用保存成功后应显示不可用: %+v", a)
	}
}

// TestDeactivateUnchangedSemantics 覆盖保留的既有入口语义：未登记账户返回
// 可由 errors.Is 判断的 ErrNotFound；已成功停用的账户再次停用直接成功，
// 即使此时无法写入也不要求再次落盘。
func TestDeactivateUnchangedSemantics(t *testing.T) {
	r := mustCreate(t, tempDir(t))
	seedDeactivateWorld(t, r)

	if err := r.DeactivateAccount("nobody"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("未登记账户停用应报 ErrNotFound: %v", err)
	}

	// dave 此前已成功停用：再次停用直接成功；即使保存条件被阻断也不要求
	// 再次写入，且 dave 保持停用。
	blockBatchSave(t, r)
	if err := r.DeactivateAccount("dave"); err != nil {
		t.Fatalf("重复停用应直接成功: %v", err)
	}
	restoreBatchSave(t, r)
	if a, _ := r.GetAccount("dave"); a.Active {
		t.Fatalf("dave 应保持停用: %+v", a)
	}
}
