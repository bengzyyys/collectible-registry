package registry

import (
	"errors"
	"os"
	"testing"
)

// 本文件为账户停用补充"停用保存失败"场景的回归保障。账户、藏品与持有均
// 已存在且可用时，DeactivateAccount 的停用状态尚未原子替换原数据就发生
// 写入失败（如数据位置暂时无法写入），这样的失败不是一次有效停用：必须
// 返回本次实际的保存错误，不能返回成功，也不能用失败后重新读取原数据时的
// 错误取代它。即使原数据仍在、却暂时无法读取或解析、状态未能按磁盘重建，
// 同一个仍打开的登记册也必须保持停用前的样子——账户仍可用、编号与元数据
// 不变，其已有藏品的持有人、版本与历史原样保留；其他账户此前已经成功停用
// 的状态不被恢复为可用。原数据仍可读取的普通保存失败同样撤销本次变化，不
// 依赖重新读取成功。随后另一次无关操作成功保存也不会夹带这次未保存的停用；
// 读写恢复后该账户仍按原有规则参与转让；之后再次提交停用须重新保存，成功
// 返回后才显示不可用，并按现有规则拒绝其发起或接收新的转让。
//
// 失败注入与其他场景共用：在 .registry.json.tmp 上预建目录，save 在
// OpenFile 阶段即以 EISDIR 失败（rename 之前）；部分用例同时把数据文件
// 改写为无法解析的内容，覆盖"原数据暂时无法读取、commit 无法按磁盘重建
// 状态"。

// seedDeactivateWorld 建立停用保存失败用例的初始世界：
//   - alice（创作者，元数据"元-alice"）、bob、carol 三个已登记账户；
//   - alice 创建系列 s1，发行 i1 给自己后转让给 bob：i1 现为 bob 版本 2；
//   - alice 另发行 i2 给自己：alice 仍持有 i2 版本 1，用于核对停用失败
//     不改变其名下藏品的持有人、版本与历史；
//   - carol 在失败注入前已成功停用，用于核对回滚不会把其他账户的停用
//     恢复为可用。
func seedDeactivateWorld(t *testing.T, r *Registry) {
	t.Helper()
	if err := r.RegisterAccount("alice", "元-alice"); err != nil {
		t.Fatal(err)
	}
	if err := r.RegisterAccount("bob", "元-bob"); err != nil {
		t.Fatal(err)
	}
	if err := r.RegisterAccount("carol", "元-carol"); err != nil {
		t.Fatal(err)
	}
	if err := r.CreateSeries("s1", "alice", "系列"); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Issue(IssueRequest{
		Operator: "alice", Reason: "首发 i1", RequestID: "ri-i1",
		ItemID: "i1", SeriesID: "s1", BatchNo: "b1", HolderID: "alice",
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Transfer(TransferRequest{
		Operator: "alice", Reason: "转让给 bob", RequestID: "rt-i1-bob", ItemID: "i1",
		ExpectedOwner: "alice", ExpectedVer: 1, ToID: "bob",
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Issue(IssueRequest{
		Operator: "alice", Reason: "首发 i2", RequestID: "ri-i2",
		ItemID: "i2", SeriesID: "s1", BatchNo: "b1", HolderID: "alice",
	}); err != nil {
		t.Fatal(err)
	}
	if err := r.DeactivateAccount("carol"); err != nil {
		t.Fatal(err)
	}
}

// assertDeactivateDidNotTakeEffect 在（停用保存失败后的）同一个已打开登记册
// 上核对：这次停用如同从未提交，且失败前已有的记录完整保留。
func assertDeactivateDidNotTakeEffect(t *testing.T, r *Registry) {
	t.Helper()
	// 目标账户仍可用，编号与元数据与调用前一致。
	a, err := r.GetAccount("alice")
	if err != nil {
		t.Fatalf("GetAccount alice: %v", err)
	}
	if !a.Active || a.ID != "alice" || a.Metadata != "元-alice" {
		t.Fatalf("保存失败后 alice 应保持停用前状态: %+v", a)
	}
	// 无关的可用账户不受影响。
	if b, err := r.GetAccount("bob"); err != nil || !b.Active {
		t.Fatalf("bob 应保持可用: %+v, err %v", b, err)
	}
	// 其他账户此前已经成功停用的状态不能被这次回滚恢复为可用。
	if c, err := r.GetAccount("carol"); err != nil || c.Active {
		t.Fatalf("carol 应保持已停用: %+v, err %v", c, err)
	}

	// 目标账户名下藏品的持有人、版本与历史保留；其曾转出的藏品同样未变。
	h2, err := r.GetHolding("i2")
	if err != nil || h2.OwnerID != "alice" || h2.Version != 1 {
		t.Fatalf("alice 名下 i2 持有被改变: %+v, err %v", h2, err)
	}
	h1, err := r.GetHolding("i1")
	if err != nil || h1.OwnerID != "bob" || h1.Version != 2 {
		t.Fatalf("i1 持有被失败的停用波及: %+v, err %v", h1, err)
	}
	if hs, err := r.HoldingsOf("alice"); err != nil || len(hs) != 1 ||
		hs[0].ItemID != "i2" || hs[0].Version != 1 {
		t.Fatalf("alice 持有清单异常: %+v, err %v", hs, err)
	}
	if hist, err := r.History("i2"); err != nil || len(hist) != 1 ||
		hist[0].Kind != "issue" || hist[0].ToID != "alice" {
		t.Fatalf("i2 历史被改变: %+v, err %v", hist, err)
	}
	if hist, err := r.History("i1"); err != nil || len(hist) != 2 ||
		hist[1].Kind != "transfer" || hist[1].ToID != "bob" {
		t.Fatalf("i1 历史被改变: %+v, err %v", hist, err)
	}
}

// TestDeactivateSaveFailureUnreadableSameRegistry 覆盖核心场景：停用在写入
// 阶段失败，且原数据同时被改写为无法解析（commit 无法按磁盘重建状态）。
// 返回保存错误而非成功；同一登记册上停用从未生效，旧记录完整。保存仍失败
// 时再次停用依旧报保存错误（不能因内存里的幻影停用直接返回成功）。读写
// 恢复后先做一次无关操作成功保存，不夹带未保存的停用；账户随后按原规则
// 正常发起与接收转让；再次停用重新保存成功后才显示不可用，并拒绝其参与
// 新转让，名下藏品与历史仍可查询。
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

	// 停用落盘失败：返回实际保存错误，不能是任何业务拒绝，也不能成功。
	err = r.DeactivateAccount("alice")
	assertSaveFailureError(t, err)
	assertDeactivateDidNotTakeEffect(t, r)

	// 保存条件未恢复时再次提交：仍须报保存错误。修复前内存中已是停用态，
	// 这里会被幂等分支直接返回成功——必须保持失败。
	err = r.DeactivateAccount("alice")
	assertSaveFailureError(t, err)
	assertDeactivateDidNotTakeEffect(t, r)

	// 恢复正常读写；先让一次无关操作成功保存，不能夹带这笔未保存的停用。
	if err := os.WriteFile(dataFile(r.dir), orig, fileMode); err != nil {
		t.Fatal(err)
	}
	restoreBatchSave(t, r)
	if err := r.RegisterAccount("eve", "路人"); err != nil {
		t.Fatalf("无关操作应能成功保存: %v", err)
	}
	assertDeactivateDidNotTakeEffect(t, r)

	// 读写恢复后账户仍按原有规则参与转让：先由 bob 把 i1 转给 alice
	// （接收人 alice 条件齐备），再由 alice 转回 bob——两笔都正常完成，
	// 不能因刚才失败的停用收到 ErrAccountInactive。转回后 i1 为 bob
	// 版本 4，供停用后验证"向停用账户转让被拒绝"。
	if _, err := r.Transfer(TransferRequest{
		Operator: "bob", Reason: "恢复后转入 alice", RequestID: "rt-i1-alice",
		ItemID: "i1", ExpectedOwner: "bob", ExpectedVer: 2, ToID: "alice",
	}); err != nil {
		t.Fatalf("账户应仍可接收转让: %v", err)
	}
	if _, err := r.Transfer(TransferRequest{
		Operator: "alice", Reason: "alice 再转回 bob", RequestID: "rt-i1-back",
		ItemID: "i1", ExpectedOwner: "alice", ExpectedVer: 3, ToID: "bob",
	}); err != nil {
		t.Fatalf("账户应仍可发起转让: %v", err)
	}
	if h, _ := r.GetHolding("i1"); h.OwnerID != "bob" || h.Version != 4 {
		t.Fatalf("恢复后转让结果异常: %+v", h)
	}

	// 之后再次提交停用：重新完成保存，成功返回后才显示不可用。
	if err := r.DeactivateAccount("alice"); err != nil {
		t.Fatalf("恢复后停用应成功保存: %v", err)
	}
	if a, _ := r.GetAccount("alice"); a.Active {
		t.Fatalf("停用保存成功后 alice 应不可用: %+v", a)
	}

	// 停用账户按现有规则被拒绝发起新的转让（i2 仍在 alice 手中，版本 1）。
	if _, err := r.Transfer(TransferRequest{
		Operator: "alice", Reason: "停用后转出", RequestID: "rt-blocked-out",
		ItemID: "i2", ExpectedOwner: "alice", ExpectedVer: 1, ToID: "bob",
	}); !errors.Is(err, ErrAccountInactive) {
		t.Fatalf("停用账户发起转让应被 ErrAccountInactive 拒绝: %v", err)
	}
	// 停用账户按现有规则被拒绝接收新的转让（bob 持 i1 版本 4）。
	if _, err := r.Transfer(TransferRequest{
		Operator: "bob", Reason: "停用后转入", RequestID: "rt-blocked-in",
		ItemID: "i1", ExpectedOwner: "bob", ExpectedVer: 4, ToID: "alice",
	}); !errors.Is(err, ErrAccountInactive) {
		t.Fatalf("停用账户接收转让应被 ErrAccountInactive 拒绝: %v", err)
	}
	// 停用的创建账户不能再发行。
	if _, err := r.Issue(IssueRequest{
		Operator: "alice", Reason: "停用后发行", RequestID: "ri-blocked",
		ItemID: "i3", SeriesID: "s1", BatchNo: "b1", HolderID: "bob",
	}); !errors.Is(err, ErrAccountInactive) {
		t.Fatalf("停用账户发行应被 ErrAccountInactive 拒绝: %v", err)
	}

	// 停用本身不改变藏品归属与持有版本，不删除其原有藏品与历史，查询仍可用。
	if h, _ := r.GetHolding("i2"); h.OwnerID != "alice" || h.Version != 1 {
		t.Fatalf("停用不应改变 i2 持有: %+v", h)
	}
	if h, _ := r.GetHolding("i1"); h.OwnerID != "bob" || h.Version != 4 {
		t.Fatalf("被拒绝的转让不应改变 i1 持有: %+v", h)
	}
	if hs, err := r.HoldingsOf("alice"); err != nil || len(hs) != 1 ||
		hs[0].ItemID != "i2" {
		t.Fatalf("停用后名下藏品仍应可查询: %+v, err %v", hs, err)
	}
	if hist, err := r.History("i1"); err != nil || len(hist) != 4 {
		t.Fatalf("停用后历史仍应可查询且不增减: %+v, err %v", hist, err)
	}

	// 落盘视角：关闭重开后停用已持久化，其他账户状态与持有一致。
	dir := r.dir
	if err := r.Close(); err != nil {
		t.Fatal(err)
	}
	r2, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = r2.Close() })
	if a, _ := r2.GetAccount("alice"); a.Active || a.Metadata != "元-alice" {
		t.Fatalf("重开后 alice 应保持停用且元数据不变: %+v", a)
	}
	if c, _ := r2.GetAccount("carol"); c.Active {
		t.Fatalf("重开后 carol 应仍停用: %+v", c)
	}
	if e, _ := r2.GetAccount("eve"); !e.Active {
		t.Fatalf("重开后无关新账户应存在且可用: %+v", e)
	}
	if h, _ := r2.GetHolding("i1"); h.OwnerID != "bob" || h.Version != 4 {
		t.Fatalf("重开后 i1 持有异常: %+v", h)
	}
	if h, _ := r2.GetHolding("i2"); h.OwnerID != "alice" || h.Version != 1 {
		t.Fatalf("重开后 i2 持有异常: %+v", h)
	}
}

// TestDeactivateSaveFailureReadableSameRegistry 覆盖：写入失败但原数据仍可
// 正常读取（commit 据磁盘内容重建状态）时，得到相同的保障——同一登记册上
// 停用从未发生；恢复后重新停用须完整保存一次才生效。
func TestDeactivateSaveFailureReadableSameRegistry(t *testing.T) {
	r := mustCreate(t, tempDir(t))
	seedDeactivateWorld(t, r)

	// 只阻断写入，不破坏数据文件：rename 之前失败，原快照完好可读。
	blockBatchSave(t, r)
	err := r.DeactivateAccount("alice")
	assertSaveFailureError(t, err)
	assertDeactivateDidNotTakeEffect(t, r)

	restoreBatchSave(t, r)
	if err := r.DeactivateAccount("alice"); err != nil {
		t.Fatalf("恢复后重新停用应成功保存: %v", err)
	}
	if a, _ := r.GetAccount("alice"); a.Active {
		t.Fatalf("重新停用保存成功后 alice 应不可用: %+v", a)
	}
	// 再次停用已停用账户仍直接成功，且不再写入（否则会被失败注入拦住）。
	blockBatchSave(t, r)
	if err := r.DeactivateAccount("alice"); err != nil {
		t.Fatalf("已停用账户重复停用应直接成功、不要求写入: %v", err)
	}
}

// TestDeactivateSaveFailureRetryAfterReopen 覆盖磁盘视角：停用保存失败
// （原数据可读）后关闭重开，看到的仍是停用前状态；恢复保存后重新停用
// 正常落盘，而不是把一次从未保存的停用当成既成事实。
func TestDeactivateSaveFailureRetryAfterReopen(t *testing.T) {
	dir := tempDir(t)
	r := mustCreate(t, dir)
	seedDeactivateWorld(t, r)

	blockBatchSave(t, r)
	err := r.DeactivateAccount("alice")
	assertSaveFailureError(t, err)
	if err := r.Close(); err != nil {
		t.Fatal(err)
	}
	r2, err := Open(dir)
	if err != nil {
		t.Fatalf("失败后原登记册必须仍能正常读取: %v", err)
	}
	t.Cleanup(func() { _ = r2.Close() })
	assertDeactivateDidNotTakeEffect(t, r2)

	restoreBatchSave(t, r2)
	if err := r2.DeactivateAccount("alice"); err != nil {
		t.Fatalf("重开后重新停用应成功: %v", err)
	}
	if a, _ := r2.GetAccount("alice"); a.Active {
		t.Fatalf("重开后停用保存成功才应不可用: %+v", a)
	}
	// 停用账户接收转让按现有规则被拒绝。
	if _, err := r2.Transfer(TransferRequest{
		Operator: "bob", Reason: "向停用账户转让", RequestID: "rt-to-inactive",
		ItemID: "i1", ExpectedOwner: "bob", ExpectedVer: 2, ToID: "alice",
	}); !errors.Is(err, ErrAccountInactive) {
		t.Fatalf("向停用账户转让应被 ErrAccountInactive 拒绝: %v", err)
	}
	if h, _ := r2.GetHolding("i1"); h.OwnerID != "bob" || h.Version != 2 {
		t.Fatalf("被拒绝的转让不应改变持有: %+v", h)
	}
}

// TestDeactivateUnknownAndIdempotent 保留现有停用入口与错误判定：未登记
// 账户返回可由 errors.Is 判断的 ErrNotFound（即使当前正无法写入，也不变成
// 保存错误）；已成功停用的账户再次提交直接成功、保持停用，不要求再次写入。
func TestDeactivateUnknownAndIdempotent(t *testing.T) {
	r := mustCreate(t, tempDir(t))
	seedDeactivateWorld(t, r)

	if err := r.DeactivateAccount("ghost"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("未登记账户停用应返回 ErrNotFound: %v", err)
	}
	// 写入被阻断时引用检查仍先于保存发生，判定方式不变。
	blockBatchSave(t, r)
	if err := r.DeactivateAccount("ghost"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("写入受限时未登记账户仍应返回 ErrNotFound: %v", err)
	}
	// 已停用账户重复停用直接成功，不触发写入（否则会被失败注入拦住）。
	if err := r.DeactivateAccount("carol"); err != nil {
		t.Fatalf("已停用账户再次停用应直接成功且不写入: %v", err)
	}
	if c, _ := r.GetAccount("carol"); c.Active {
		t.Fatalf("carol 应保持停用: %+v", c)
	}
	// 写入受限时可用账户的停用仍须真实保存并返回保存错误。
	err := r.DeactivateAccount("bob")
	assertSaveFailureError(t, err)
	if b, _ := r.GetAccount("bob"); !b.Active {
		t.Fatalf("保存失败后 bob 应仍可用: %+v", b)
	}
}
