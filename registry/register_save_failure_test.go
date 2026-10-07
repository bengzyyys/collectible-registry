package registry

import (
	"errors"
	"os"
	"testing"
)

// 本文件为账户登记补充"登记保存失败"场景的回归保障。对一个尚未使用的编号
// 调用 RegisterAccount 时，账户是否存在始终以一次保存完成为准：新账户尚未
// 原子替换原数据就发生写入失败（如数据位置暂时无法写入），这样的失败不是
// 一次有效登记——必须返回本次实际的保存错误，不能返回成功或编号冲突，也
// 不能用失败后重新读取原数据时的错误取代它。即使原数据仍在、却暂时无法
// 读取或解析、状态未能按磁盘重建，同一个仍打开的登记册也必须立即表现为该
// 编号从未登记：GetAccount 返回 ErrNotFound，藏品转让不把它当成已登记的
// 接收账户。失败不占用新账户编号：保存条件未恢复时用该编号再次登记仍实际
// 尝试保存并返回当次保存错误，而不是因上次失败遗留的账户返回
// ErrAlreadyExists；随后另一次合法操作成功保存不夹带这个未保存的账户，关闭
// 再打开登记册后它仍不存在。读写恢复后该编号可以重新登记，以本次提交的元
// 数据为准，保存成功才显示为可用账户。原数据仍可读取的普通保存失败得到同
// 样结果。此前成功登记的账户及其元数据、可用或停用状态，以及已有藏品的持有
// 人、版本与历史都原样保留。
//
// 失败注入与其他场景共用：在 .registry.json.tmp 上预建目录，save 在
// OpenFile 阶段即以 EISDIR 失败（rename 之前）；部分用例同时把数据文件
// 改写为无法解析的内容，覆盖"原数据暂时无法读取、commit 无法按磁盘重建
// 状态"。

// seedRegisterWorld 建立登记保存失败用例的初始世界：
//   - alice（创作者，元数据"元-alice"）、bob（"元-bob"）两个可用账户，
//     carol（"元-carol"）在失败注入前已成功停用，用于核对回滚既不恢复其他
//     账户的停用，也不允许覆盖已登记编号；
//   - alice 创建系列 s1，发行 i1 给自己后转让给 bob：i1 现为 bob 版本 2；
//   - alice 另发行 i2 给自己：alice 仍持有 i2 版本 1，用于核对失败的登记
//     不改变任何藏品的持有人、版本与历史。
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

// assertDaveNotRegistered 在（登记保存失败后的）同一个已打开登记册上核对：
// 新编号 dave 如同从未登记，且失败前已有的账户、藏品持有与历史完整保留。
func assertDaveNotRegistered(t *testing.T, r *Registry) {
	t.Helper()
	if _, err := r.GetAccount("dave"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("保存失败后 dave 应表现为从未登记，GetAccount 返回: %+v, err %v",
			err, err)
	}
	// 转让不能把失败的账户当成已登记的接收账户：接收人校验先于持有信息
	// 校验，应按接收账户不存在返回 ErrNotFound，而不是 ErrAccountInactive
	// 或持有冲突。
	if _, err := r.Transfer(TransferRequest{
		Operator: "bob", Reason: "转给幻影账户", RequestID: "rt-to-dave-pending",
		ItemID: "i1", ExpectedOwner: "bob", ExpectedVer: 2, ToID: "dave",
	}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("幻影账户作为接收人应被 ErrNotFound 拒绝: %v", err)
	}

	// 此前成功登记的账户及其元数据、可用/停用状态原样保留。
	if a, err := r.GetAccount("alice"); err != nil || !a.Active ||
		a.ID != "alice" || a.Metadata != "元-alice" {
		t.Fatalf("alice 记录被失败的登记波及: %+v, err %v", a, err)
	}
	if b, err := r.GetAccount("bob"); err != nil || !b.Active || b.Metadata != "元-bob" {
		t.Fatalf("bob 记录被失败的登记波及: %+v, err %v", b, err)
	}
	if c, err := r.GetAccount("carol"); err != nil || c.Active {
		t.Fatalf("carol 应保持已停用: %+v, err %v", c, err)
	}

	// 已有藏品的持有人、版本与历史不变。
	if h, err := r.GetHolding("i1"); err != nil || h.OwnerID != "bob" || h.Version != 2 {
		t.Fatalf("i1 持有被失败的登记波及: %+v, err %v", h, err)
	}
	if h, err := r.GetHolding("i2"); err != nil || h.OwnerID != "alice" || h.Version != 1 {
		t.Fatalf("i2 持有被失败的登记波及: %+v, err %v", h, err)
	}
	if hist, err := r.History("i1"); err != nil || len(hist) != 2 {
		t.Fatalf("i1 历史被失败的登记改变: %+v, err %v", hist, err)
	}
	if hist, err := r.History("i2"); err != nil || len(hist) != 1 {
		t.Fatalf("i2 历史被失败的登记改变: %+v, err %v", hist, err)
	}
}

// TestRegisterAccountSaveFailureUnreadableSameRegistry 覆盖核心场景：登记在
// 写入阶段失败，且原数据同时被改写为无法解析（commit 无法按磁盘重建状态）。
// 返回保存错误而非成功或编号冲突；同一登记册上该编号从未登记，旧记录完整。
// 保存仍失败时用同一编号再次登记依旧实际尝试保存并报保存错误（不能因遗留
// 账户返回 ErrAlreadyExists）。读写恢复后先让一次无关操作成功保存，不夹带
// 未保存的账户；关闭再打开它仍不存在。随后该编号以新元数据重新登记，保存
// 成功才显示为可用账户并能接收转让，重开后以这次提交为准持久存在。
func TestRegisterAccountSaveFailureUnreadableSameRegistry(t *testing.T) {
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

	// 登记落盘失败：返回实际保存错误，不能是成功、编号冲突或任何业务拒绝。
	err = r.RegisterAccount("dave", "元-dave")
	assertSaveFailureError(t, err)
	assertDaveNotRegistered(t, r)

	// 保存条件未恢复时用同一编号再次登记：仍须真实保存并报当次保存错误，
	// 修复前内存中遗留的 dave 会让这里直接返回 ErrAlreadyExists。
	err = r.RegisterAccount("dave", "元-dave-重试")
	assertSaveFailureError(t, err)
	assertDaveNotRegistered(t, r)

	// 恢复正常读写；先让一次无关操作成功保存，不能夹带这个未保存的账户。
	if err := os.WriteFile(dataFile(r.dir), orig, fileMode); err != nil {
		t.Fatal(err)
	}
	restoreBatchSave(t, r)
	if err := r.RegisterAccount("eve", "路人"); err != nil {
		t.Fatalf("无关操作应能成功保存: %v", err)
	}
	assertDaveNotRegistered(t, r)
	if e, err := r.GetAccount("eve"); err != nil || !e.Active || e.Metadata != "路人" {
		t.Fatalf("无关新账户应正常存在: %+v, err %v", e, err)
	}

	// 关闭再打开：未重新登记的失败账户仍不存在，其他记录与无关操作都在。
	dir := r.dir
	if err := r.Close(); err != nil {
		t.Fatal(err)
	}
	r2, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = r2.Close() })
	assertDaveNotRegistered(t, r2)
	if e, _ := r2.GetAccount("eve"); !e.Active {
		t.Fatalf("重开后无关新账户应存在且可用: %+v", e)
	}

	// 读写恢复后该编号可以重新登记，以本次提交的元数据为准（而非失败那次
	// 的"元-dave"），保存成功才显示为可用账户。
	if err := r2.RegisterAccount("dave", "元-dave-恢复后"); err != nil {
		t.Fatalf("恢复后重新登记应成功保存: %v", err)
	}
	if a, _ := r2.GetAccount("dave"); !a.Active || a.Metadata != "元-dave-恢复后" {
		t.Fatalf("重新登记应以本次提交的元数据为准且可用: %+v", a)
	}

	// 保存成功后 dave 才能作为接收账户接收转让（bob 持 i1 版本 2）。
	if _, err := r2.Transfer(TransferRequest{
		Operator: "bob", Reason: "恢复后转给 dave", RequestID: "rt-i1-dave",
		ItemID: "i1", ExpectedOwner: "bob", ExpectedVer: 2, ToID: "dave",
	}); err != nil {
		t.Fatalf("保存成功的账户应能接收转让: %v", err)
	}
	if h, _ := r2.GetHolding("i1"); h.OwnerID != "dave" || h.Version != 3 {
		t.Fatalf("转让结果异常: %+v", h)
	}

	// 再次落盘视角：重开后 dave 以新元数据存在，持有与历史一致。
	if err := r2.Close(); err != nil {
		t.Fatal(err)
	}
	r3, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = r3.Close() })
	if a, _ := r3.GetAccount("dave"); !a.Active || a.Metadata != "元-dave-恢复后" {
		t.Fatalf("重开后 dave 应以新元数据可用: %+v", a)
	}
	if h, _ := r3.GetHolding("i1"); h.OwnerID != "dave" || h.Version != 3 {
		t.Fatalf("重开后 i1 持有异常: %+v", h)
	}
	if hist, _ := r3.History("i1"); len(hist) != 3 {
		t.Fatalf("重开后 i1 历史应有 3 条")
	}
}

// TestRegisterAccountSaveFailureReadableSameRegistry 覆盖：写入失败但原数据
// 仍可正常读取（commit 据磁盘内容重建状态）时得到相同保障——同一登记册上
// 该编号从未登记；恢复后重新登记须完整保存一次才生效。
func TestRegisterAccountSaveFailureReadableSameRegistry(t *testing.T) {
	r := mustCreate(t, tempDir(t))
	seedRegisterWorld(t, r)

	// 只阻断写入，不破坏数据文件：rename 之前失败，原快照完好可读。
	blockBatchSave(t, r)
	err := r.RegisterAccount("dave", "元-dave")
	assertSaveFailureError(t, err)
	assertDaveNotRegistered(t, r)

	// 保存条件未恢复，换一个元数据再次登记同样必须实际尝试保存。
	err = r.RegisterAccount("dave", "元-dave-重试")
	assertSaveFailureError(t, err)
	assertDaveNotRegistered(t, r)

	restoreBatchSave(t, r)
	if err := r.RegisterAccount("dave", "元-dave-恢复后"); err != nil {
		t.Fatalf("恢复后重新登记应成功保存: %v", err)
	}
	if a, _ := r.GetAccount("dave"); !a.Active || a.Metadata != "元-dave-恢复后" {
		t.Fatalf("重新登记保存成功后 dave 应可用且以本次元数据为准: %+v", a)
	}
}

// TestRegisterAccountSaveFailureRetryAfterReopen 覆盖磁盘视角：登记保存失败
// （原数据可读）后关闭重开，看到的仍是登记前状态；恢复保存后重新登记正常
// 落盘，而不是把一次从未保存的登记当成既成事实。
func TestRegisterAccountSaveFailureRetryAfterReopen(t *testing.T) {
	dir := tempDir(t)
	r := mustCreate(t, dir)
	seedRegisterWorld(t, r)

	blockBatchSave(t, r)
	err := r.RegisterAccount("dave", "元-dave")
	assertSaveFailureError(t, err)
	if err := r.Close(); err != nil {
		t.Fatal(err)
	}
	r2, err := Open(dir)
	if err != nil {
		t.Fatalf("失败后原登记册必须仍能正常读取: %v", err)
	}
	t.Cleanup(func() { _ = r2.Close() })
	assertDaveNotRegistered(t, r2)

	restoreBatchSave(t, r2)
	if err := r2.RegisterAccount("dave", "元-dave-恢复后"); err != nil {
		t.Fatalf("重开后重新登记应成功: %v", err)
	}
	if a, _ := r2.GetAccount("dave"); !a.Active || a.Metadata != "元-dave-恢复后" {
		t.Fatalf("重开后登记保存成功才应存在且以本次元数据为准: %+v", a)
	}
}

// TestRegisterAccountBusinessRejectionsWhileUnwritable 保留登记入口原有约定：
// 空白编号返回 ErrInvalidArgument，已成功登记的编号（无论账户是否停用）返回
// ErrAlreadyExists 且不覆盖记录——这些业务拒绝不触发保存，数据位置恰好不可
// 写时也不能变成保存错误。元数据允许为空：新编号配空元数据在不可写时仍实际
// 尝试保存并返回保存错误、不留下账户，恢复后成功登记为空元数据的可用账户。
func TestRegisterAccountBusinessRejectionsWhileUnwritable(t *testing.T) {
	r := mustCreate(t, tempDir(t))
	seedRegisterWorld(t, r)

	blockBatchSave(t, r)

	if err := r.RegisterAccount("   ", "x"); !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("空白编号应返回 ErrInvalidArgument，且不变成保存错误: %v", err)
	}
	// 可用账户的编号已占用：ErrAlreadyExists，记录不被覆盖。
	if err := r.RegisterAccount("alice", "被覆盖的元数据"); !errors.Is(err, ErrAlreadyExists) {
		t.Fatalf("已登记编号应返回 ErrAlreadyExists，且不变成保存错误: %v", err)
	}
	// 已停用账户的编号同样永久占用：ErrAlreadyExists，且停用状态不被恢复。
	if err := r.RegisterAccount("carol", "被覆盖的元数据"); !errors.Is(err, ErrAlreadyExists) {
		t.Fatalf("已停用账户的编号仍应返回 ErrAlreadyExists: %v", err)
	}
	if a, _ := r.GetAccount("alice"); a.Metadata != "元-alice" || !a.Active {
		t.Fatalf("业务拒绝不应改动 alice 记录: %+v", a)
	}
	if c, _ := r.GetAccount("carol"); c.Metadata != "元-carol" || c.Active {
		t.Fatalf("业务拒绝不应改动 carol 记录或恢复其停用: %+v", c)
	}

	// 元数据允许为空：新编号仍须真实保存，不可写时返回当次保存错误且不留账户。
	err := r.RegisterAccount("dave", "")
	assertSaveFailureError(t, err)
	if _, err := r.GetAccount("dave"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("保存失败后 dave 不应存在: %v", err)
	}

	restoreBatchSave(t, r)
	if err := r.RegisterAccount("dave", ""); err != nil {
		t.Fatalf("恢复后空元数据登记应成功保存: %v", err)
	}
	if a, _ := r.GetAccount("dave"); !a.Active || a.Metadata != "" {
		t.Fatalf("空元数据账户保存成功后应可用且元数据为空: %+v", a)
	}
}
