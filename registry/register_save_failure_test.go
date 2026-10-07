package registry

import (
	"errors"
	"os"
	"testing"
)

// 本文件为账户登记补充"登记保存失败"场景的回归保障。RegisterAccount 登记
// 一个尚未使用的编号时，新账户尚未原子替换原数据就发生写入失败（如数据
// 位置暂时无法写入），这样的失败不是一次有效登记：必须返回本次实际的保存
// 错误，不能返回成功，也不能用失败后重新读取原数据时的错误取代它。即使原
// 数据仍在、却暂时无法读取或解析、状态未能按磁盘重建，同一个仍打开的登记册
// 也必须表现为该编号从未登记——GetAccount 返回 ErrNotFound，藏品转让不能把
// 它当成已登记的接收账户；原数据仍可读取的普通保存失败同样撤销本次登记。
// 失败不占用编号：保存条件未恢复时用同一编号再次登记仍实际尝试保存并返回
// 当次保存错误，不能因上次失败遗留的账户报 ErrAlreadyExists；随后另一次
// 无关操作成功保存也不会夹带这次未保存的登记，关闭再打开后该编号仍不存在。
// 读写恢复后该编号可重新登记，以这次提交的元数据为准，保存成功后才显示为
// 可用账户。失败前已成功登记的账户及其元数据、可用或停用状态，以及已有藏品
// 的持有人、版本与历史都原样保留。
//
// 失败注入与其他场景共用：在 .registry.json.tmp 上预建目录，save 在
// OpenFile 阶段即以 EISDIR 失败（rename 之前）；部分用例同时把数据文件
// 改写为无法解析的内容，覆盖"原数据暂时无法读取、commit 无法按磁盘重建
// 状态"。

// seedRegisterWorld 建立登记保存失败用例的初始世界：
//   - alice（创作者，元数据"元-alice"）、bob 两个已登记账户；
//   - carol 已登记并成功停用，用于核对失败回滚不影响其他账户的停用状态，
//     以及"已登记编号无论是否停用都报 ErrAlreadyExists"；
//   - alice 创建系列 s1 并发行 i1 给自己：i1 为 alice 版本 1，用于核对
//     失败的登记不影响已有藏品的持有人、版本与历史，以及恢复后向新账户
//     转让的验证。
func seedRegisterWorld(t *testing.T, r *Registry) {
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
	if err := r.DeactivateAccount("carol"); err != nil {
		t.Fatal(err)
	}
}

// assertRegisterDidNotTakeEffect 在（登记保存失败后的）同一个已打开登记册
// 上核对：编号 dave 如同从未登记，且失败前已有的记录完整保留。
func assertRegisterDidNotTakeEffect(t *testing.T, r *Registry) {
	t.Helper()
	// 失败的登记不留下账户：GetAccount 返回 ErrNotFound。
	if _, err := r.GetAccount("dave"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("保存失败后 dave 应不存在: %v", err)
	}
	// 藏品转让不能把 dave 当成已登记的接收账户。
	if _, err := r.Transfer(TransferRequest{
		Operator: "alice", Reason: "转给未登记的 dave", RequestID: "rt-to-dave",
		ItemID: "i1", ExpectedOwner: "alice", ExpectedVer: 1, ToID: "dave",
	}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("向未保存成功的账户转让应被 ErrNotFound 拒绝: %v", err)
	}

	// 失败前已登记的账户及其元数据、可用或停用状态保留。
	if a, err := r.GetAccount("alice"); err != nil || !a.Active || a.Metadata != "元-alice" {
		t.Fatalf("alice 应保持原状: %+v, err %v", a, err)
	}
	if b, err := r.GetAccount("bob"); err != nil || !b.Active {
		t.Fatalf("bob 应保持可用: %+v, err %v", b, err)
	}
	if c, err := r.GetAccount("carol"); err != nil || c.Active {
		t.Fatalf("carol 应保持已停用: %+v, err %v", c, err)
	}

	// 已有藏品的持有人、版本与历史原样保留。
	if h, err := r.GetHolding("i1"); err != nil || h.OwnerID != "alice" || h.Version != 1 {
		t.Fatalf("i1 持有被失败的登记波及: %+v, err %v", h, err)
	}
	if hist, err := r.History("i1"); err != nil || len(hist) != 1 ||
		hist[0].Kind != "issue" || hist[0].ToID != "alice" {
		t.Fatalf("i1 历史被改变: %+v, err %v", hist, err)
	}
}

// assertRegisterSaveError 确认返回的是保存错误：既不是成功，也不是编号
// 重复等业务拒绝。
func assertRegisterSaveError(t *testing.T, err error) {
	t.Helper()
	assertSaveFailureError(t, err)
	if errors.Is(err, ErrAlreadyExists) {
		t.Fatalf("返回的应是保存错误，不能是编号重复: %v", err)
	}
}

// TestRegisterSaveFailureUnreadableSameRegistry 覆盖核心场景：登记在写入
// 阶段失败，且原数据同时被改写为无法解析（commit 无法按磁盘重建状态）。
// 返回保存错误而非成功或编号重复；同一登记册上该编号从未登记。保存仍失败
// 时用同一编号再次登记依旧实际尝试保存并报当次保存错误（不能因上次失败
// 遗留的账户报 ErrAlreadyExists）。读写恢复后先做一次无关操作成功保存，
// 不夹带未保存的登记，关闭再打开后该编号仍不存在；随后用同一编号重新登记，
// 以这次提交的元数据为准，保存成功后才显示为可用账户并可接收转让。
func TestRegisterSaveFailureUnreadableSameRegistry(t *testing.T) {
	r := mustCreate(t, tempDir(t))
	seedRegisterWorld(t, r)

	orig, err := os.ReadFile(dataFile(r.dir))
	if err != nil {
		t.Fatal(err)
	}
	blockBatchSave(t, r)
	if err := os.WriteFile(dataFile(r.dir), []byte("{corrupt"), fileMode); err != nil {
		t.Fatal(err)
	}

	// 登记落盘失败：返回实际保存错误，不能成功，也不能是编号重复。
	err = r.RegisterAccount("dave", "元-dave-第一次")
	assertRegisterSaveError(t, err)
	assertRegisterDidNotTakeEffect(t, r)

	// 保存条件未恢复时用同一编号再次登记：仍须实际尝试保存并报当次保存
	// 错误。修复前内存中已留有该账户，这里会报 ErrAlreadyExists——必须
	// 保持失败。
	err = r.RegisterAccount("dave", "元-dave-第一次")
	assertRegisterSaveError(t, err)
	assertRegisterDidNotTakeEffect(t, r)

	// 恢复正常读写；先让一次无关操作成功保存，不能夹带这次未保存的登记。
	if err := os.WriteFile(dataFile(r.dir), orig, fileMode); err != nil {
		t.Fatal(err)
	}
	restoreBatchSave(t, r)
	if err := r.RegisterAccount("eve", "路人"); err != nil {
		t.Fatalf("无关操作应能成功保存: %v", err)
	}
	assertRegisterDidNotTakeEffect(t, r)

	// 落盘视角：关闭再打开后，未重新登记的失败账户仍不存在。
	dir := r.dir
	if err := r.Close(); err != nil {
		t.Fatal(err)
	}
	r2, err := Open(dir)
	if err != nil {
		t.Fatalf("失败后原登记册必须仍能正常打开: %v", err)
	}
	t.Cleanup(func() { _ = r2.Close() })
	if _, err := r2.GetAccount("dave"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("重开后 dave 应仍不存在: %v", err)
	}
	if e, err := r2.GetAccount("eve"); err != nil || !e.Active {
		t.Fatalf("重开后无关新账户应存在且可用: %+v, err %v", e, err)
	}
	if c, err := r2.GetAccount("carol"); err != nil || c.Active {
		t.Fatalf("重开后 carol 应仍停用: %+v, err %v", c, err)
	}

	// 读写恢复后同一编号可重新登记，以这次提交的元数据为准；保存成功后
	// 才显示为可用账户。
	if err := r2.RegisterAccount("dave", "元-dave-第二次"); err != nil {
		t.Fatalf("恢复后重新登记应成功保存: %v", err)
	}
	d, err := r2.GetAccount("dave")
	if err != nil || !d.Active || d.Metadata != "元-dave-第二次" {
		t.Fatalf("重新登记保存成功后 dave 应可用且以本次元数据为准: %+v, err %v", d, err)
	}
	// 已成功登记的编号再次登记报 ErrAlreadyExists，不能覆盖其记录。
	if err := r2.RegisterAccount("dave", "元-dave-第三次"); !errors.Is(err, ErrAlreadyExists) {
		t.Fatalf("已登记的编号再次登记应报 ErrAlreadyExists: %v", err)
	}
	if d, _ := r2.GetAccount("dave"); d.Metadata != "元-dave-第二次" {
		t.Fatalf("编号冲突不能覆盖已登记记录: %+v", d)
	}

	// 新账户可正常接收转让：alice 把 i1 转给 dave。
	if _, err := r2.Transfer(TransferRequest{
		Operator: "alice", Reason: "恢复后转给 dave", RequestID: "rt-i1-dave",
		ItemID: "i1", ExpectedOwner: "alice", ExpectedVer: 1, ToID: "dave",
	}); err != nil {
		t.Fatalf("新账户应可接收转让: %v", err)
	}
	if h, _ := r2.GetHolding("i1"); h.OwnerID != "dave" || h.Version != 2 {
		t.Fatalf("恢复后转让结果异常: %+v", h)
	}
}

// TestRegisterSaveFailureReadableSameRegistry 覆盖：写入失败但原数据仍可
// 正常读取（commit 据磁盘内容重建状态）时，得到相同的保障——同一登记册上
// 该编号从未登记；恢复后重新登记须完整保存一次才生效。
func TestRegisterSaveFailureReadableSameRegistry(t *testing.T) {
	r := mustCreate(t, tempDir(t))
	seedRegisterWorld(t, r)

	// 只阻断写入，不破坏数据文件：rename 之前失败，原快照完好可读。
	blockBatchSave(t, r)
	err := r.RegisterAccount("dave", "元-dave")
	assertRegisterSaveError(t, err)
	assertRegisterDidNotTakeEffect(t, r)

	// 保存条件未恢复时再次登记仍报保存错误，而非编号重复。
	err = r.RegisterAccount("dave", "元-dave")
	assertRegisterSaveError(t, err)
	assertRegisterDidNotTakeEffect(t, r)

	restoreBatchSave(t, r)
	if err := r.RegisterAccount("dave", "元-dave"); err != nil {
		t.Fatalf("恢复后重新登记应成功保存: %v", err)
	}
	if d, _ := r.GetAccount("dave"); !d.Active || d.Metadata != "元-dave" {
		t.Fatalf("重新登记保存成功后 dave 应可用: %+v", d)
	}
}

// TestRegisterSaveFailureRetryAfterReopen 覆盖磁盘视角：登记保存失败
// （原数据可读）后关闭重开，看到的仍是没有该编号的状态；恢复保存后重新
// 登记正常落盘，而不是把一次从未保存的登记当成既成事实。
func TestRegisterSaveFailureRetryAfterReopen(t *testing.T) {
	dir := tempDir(t)
	r := mustCreate(t, dir)
	seedRegisterWorld(t, r)

	blockBatchSave(t, r)
	err := r.RegisterAccount("dave", "元-dave")
	assertRegisterSaveError(t, err)
	if err := r.Close(); err != nil {
		t.Fatal(err)
	}
	r2, err := Open(dir)
	if err != nil {
		t.Fatalf("失败后原登记册必须仍能正常读取: %v", err)
	}
	t.Cleanup(func() { _ = r2.Close() })
	assertRegisterDidNotTakeEffect(t, r2)

	restoreBatchSave(t, r2)
	if err := r2.RegisterAccount("dave", "元-dave"); err != nil {
		t.Fatalf("重开后重新登记应成功: %v", err)
	}
	if d, _ := r2.GetAccount("dave"); !d.Active {
		t.Fatalf("重开后登记保存成功才应可用: %+v", d)
	}
}

// TestRegisterBusinessRulesUnderSaveFailure 保留现有登记入口与错误判定：
// 空白编号返回 ErrInvalidArgument，元数据允许为空，已成功登记的编号无论
// 账户是否停用都返回 ErrAlreadyExists 且不覆盖其记录；这些业务拒绝不因
// 数据位置恰好不可写而变成保存错误。
func TestRegisterBusinessRulesUnderSaveFailure(t *testing.T) {
	r := mustCreate(t, tempDir(t))
	seedRegisterWorld(t, r)

	// 元数据允许为空。
	if err := r.RegisterAccount("frank", ""); err != nil {
		t.Fatalf("空元数据应允许登记: %v", err)
	}
	if f, _ := r.GetAccount("frank"); !f.Active || f.Metadata != "" {
		t.Fatalf("空元数据账户应正常登记: %+v", f)
	}

	// 写入被阻断时业务判定仍先于保存发生，判定方式不变。
	blockBatchSave(t, r)
	if err := r.RegisterAccount("", "元"); !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("空白编号应返回 ErrInvalidArgument: %v", err)
	}
	if err := r.RegisterAccount("   ", "元"); !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("纯空白编号应返回 ErrInvalidArgument: %v", err)
	}
	if err := r.RegisterAccount("alice", "别的元数据"); !errors.Is(err, ErrAlreadyExists) {
		t.Fatalf("已登记的可用账户编号应返回 ErrAlreadyExists: %v", err)
	}
	if err := r.RegisterAccount("carol", "别的元数据"); !errors.Is(err, ErrAlreadyExists) {
		t.Fatalf("已登记的停用账户编号同样应返回 ErrAlreadyExists: %v", err)
	}
	// 编号冲突不能覆盖已有记录。
	if a, _ := r.GetAccount("alice"); a.Metadata != "元-alice" {
		t.Fatalf("编号冲突不能覆盖 alice 的记录: %+v", a)
	}
	if c, _ := r.GetAccount("carol"); c.Active || c.Metadata != "元-carol" {
		t.Fatalf("编号冲突不能覆盖 carol 的记录: %+v", c)
	}
	// 写入受限时新编号的登记仍须真实保存并返回保存错误。
	err := r.RegisterAccount("dave", "元-dave")
	assertRegisterSaveError(t, err)
	if _, err := r.GetAccount("dave"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("保存失败后 dave 应不存在: %v", err)
	}
}
