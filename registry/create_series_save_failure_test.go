package registry

import (
	"errors"
	"os"
	"testing"
)

// 本文件为系列登记补充"登记保存失败"场景的回归保障。对一个尚未使用的系列
// 编号调用 CreateSeries 时，系列是否存在始终以一次保存完成为准：新系列尚未
// 原子替换原数据就发生写入失败（如数据位置暂时无法写入），这样的失败不是
// 一次有效登记——必须返回本次实际的保存错误，不能返回成功或编号冲突，也
// 不能用失败后重新读取原数据时的错误取代它。即使原数据仍在、却暂时无法
// 读取或解析、状态未能按磁盘重建，同一个仍打开的登记册也必须立即表现为该
// 编号从未登记：GetSeries 返回 ErrNotFound，发行与封存都不能把它当成已存在
// 的系列。失败不占用系列编号：保存条件未恢复时用该编号再次登记（即使换了
// 创建账户或元数据）仍实际尝试保存并返回当次保存错误，而不是因上次失败
// 遗留的系列返回 ErrAlreadyExists；随后另一次合法操作成功保存不夹带这个未
// 保存的系列，关闭再打开登记册后它仍不存在。读写恢复后该编号可以重新登记，
// 以本次提交的创建账户与文字元数据为准，保存成功才显示为未封存的新系列并
// 可发行藏品。原数据仍可读取的普通保存失败得到相同结果。此前成功登记的
// 账户、系列及其封存状态，以及已有藏品的持有人、版本与历史都原样保留。
//
// 失败注入与其他场景共用：在 .registry.json.tmp 上预建目录，save 在
// OpenFile 阶段即以 EISDIR 失败（rename 之前）；部分用例同时把数据文件
// 改写为无法解析的内容，覆盖"原数据暂时无法读取、commit 无法按磁盘重建
// 状态"。

// seedCreateSeriesWorld 建立系列登记保存失败用例的初始世界：
//   - alice（创作者，元数据"元-alice"）、bob（"元-bob"）两个可用账户，
//     carol（"元-carol"）在失败注入前已成功停用，用于核对停用创建账户仍按
//     ErrAccountInactive 拒绝且回滚不恢复其他账户的停用；
//   - alice 创建系列 s1（"系列一"，未封存），发行 i1 给自己后转让给 bob：
//     i1 现为 bob 版本 2，用于核对失败的登记不改变任何藏品的持有人、版本
//     与历史；
//   - alice 另创建系列 s2（"系列二"）并在失败注入前成功封存，用于核对
//     回滚不会把其他系列的封存解封，也不会覆盖已登记编号。
func seedCreateSeriesWorld(t *testing.T, r *Registry) {
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
	if err := r.DeactivateAccount("carol"); err != nil {
		t.Fatal(err)
	}
	if err := r.CreateSeries("s1", "alice", "系列一"); err != nil {
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
	if err := r.CreateSeries("s2", "alice", "系列二"); err != nil {
		t.Fatal(err)
	}
	if err := r.SealSeries("s2", "alice"); err != nil {
		t.Fatal(err)
	}
}

// assertS3NotRegistered 在（系列登记保存失败后的）同一个已打开登记册上核对：
// 新编号 s3 如同从未登记，发行与封存都不把它当成已存在的系列，且失败前
// 已有的账户、系列封存状态、藏品持有与历史完整保留。
func assertS3NotRegistered(t *testing.T, r *Registry) {
	t.Helper()
	if _, err := r.GetSeries("s3"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("保存失败后 s3 应表现为从未登记，GetSeries 返回: %+v, err %v",
			err, err)
	}
	// 发行不能把失败的系列当成已存在的系列：系列不存在属于校验类拒绝，
	// 返回 ErrNotFound 而非封存/无权等状态错误，也不产生藏品或历史。
	if _, err := r.Issue(IssueRequest{
		Operator: "alice", Reason: "向幻影系列发行", RequestID: "ri-s3-ghost",
		ItemID: "i9", SeriesID: "s3", BatchNo: "b9", HolderID: "alice",
	}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("幻影系列发行应被 ErrNotFound 拒绝: %v", err)
	}
	if _, err := r.GetItem("i9"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("被幻影系列拒绝的发行不应产生藏品: %v", err)
	}
	// 封存同样不能把它当成已存在的系列（即使数据位置仍不可写，引用检查
	// 先于保存发生，应返回 ErrNotFound 而不是保存错误）。
	if err := r.SealSeries("s3", "alice"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("幻影系列封存应被 ErrNotFound 拒绝: %v", err)
	}

	// 此前成功登记的系列、创建账户、文字元数据与封存状态原样保留。
	if s, err := r.GetSeries("s1"); err != nil || s.Sealed ||
		s.CreatorID != "alice" || s.Metadata != "系列一" {
		t.Fatalf("s1 记录被失败的登记波及: %+v, err %v", s, err)
	}
	if s, err := r.GetSeries("s2"); err != nil || !s.Sealed ||
		s.CreatorID != "alice" || s.Metadata != "系列二" {
		t.Fatalf("s2 应保持已封存且记录不变: %+v, err %v", s, err)
	}
	// 已有藏品的持有人、版本与历史不变。
	if h, err := r.GetHolding("i1"); err != nil || h.OwnerID != "bob" || h.Version != 2 {
		t.Fatalf("i1 持有被失败的登记波及: %+v, err %v", h, err)
	}
	if hist, err := r.History("i1"); err != nil || len(hist) != 2 {
		t.Fatalf("i1 历史被失败的登记改变: %+v, err %v", hist, err)
	}
	// 此前成功登记的账户及可用/停用状态原样保留。
	if a, err := r.GetAccount("alice"); err != nil || !a.Active || a.Metadata != "元-alice" {
		t.Fatalf("alice 记录被失败的登记波及: %+v, err %v", a, err)
	}
	if c, err := r.GetAccount("carol"); err != nil || c.Active {
		t.Fatalf("carol 应保持已停用: %+v, err %v", c, err)
	}
}

// TestCreateSeriesSaveFailureUnreadableSameRegistry 覆盖核心场景：登记在
// 写入阶段失败，且原数据同时被改写为无法解析（commit 无法按磁盘重建状态）。
// 返回保存错误而非成功或编号冲突；同一登记册上该编号从未登记，旧记录完整。
// 保存仍失败时用同一编号再次登记（即使换创建账户或元数据）依旧实际尝试
// 保存并报保存错误（不能因遗留系列返回 ErrAlreadyExists）。读写恢复后先让
// 一次无关操作成功保存，不夹带未保存的系列；关闭再打开它仍不存在。随后该
// 编号以新创建账户与元数据重新登记，保存成功才显示为未封存的新系列，可以
// 发行藏品，重开后以这次提交为准持久存在。
func TestCreateSeriesSaveFailureUnreadableSameRegistry(t *testing.T) {
	r := mustCreate(t, tempDir(t))
	seedCreateSeriesWorld(t, r)

	orig, err := os.ReadFile(dataFile(r.dir))
	if err != nil {
		t.Fatal(err)
	}
	blockBatchSave(t, r)
	if err := os.WriteFile(dataFile(r.dir), []byte("{corrupt"), fileMode); err != nil {
		t.Fatal(err)
	}

	// 登记落盘失败：返回实际保存错误，不能是成功、编号冲突或任何业务拒绝。
	err = r.CreateSeries("s3", "alice", "系列三")
	assertSaveFailureError(t, err)
	assertS3NotRegistered(t, r)

	// 保存条件未恢复时用同一编号、不同元数据再次登记：仍须真实保存并报
	// 当次保存错误，修复前内存中遗留的 s3 会让这里直接返回
	// ErrAlreadyExists。
	err = r.CreateSeries("s3", "alice", "系列三-重试")
	assertSaveFailureError(t, err)
	assertS3NotRegistered(t, r)

	// 换一个合法创建账户重提也必须实际尝试保存：上次失败不把创建账户
	// 钉死为 alice，也不占用编号。
	err = r.CreateSeries("s3", "bob", "系列三-bob 的尝试")
	assertSaveFailureError(t, err)
	assertS3NotRegistered(t, r)

	// 恢复正常读写；先让一次无关操作成功保存，不能夹带这个未保存的系列。
	if err := os.WriteFile(dataFile(r.dir), orig, fileMode); err != nil {
		t.Fatal(err)
	}
	restoreBatchSave(t, r)
	if err := r.RegisterAccount("eve", "路人"); err != nil {
		t.Fatalf("无关操作应能成功保存: %v", err)
	}
	assertS3NotRegistered(t, r)
	if e, err := r.GetAccount("eve"); err != nil || !e.Active || e.Metadata != "路人" {
		t.Fatalf("无关新账户应正常存在: %+v, err %v", e, err)
	}

	// 关闭再打开：未重新登记的失败系列仍不存在，其他记录与无关操作都在。
	dir := r.dir
	if err := r.Close(); err != nil {
		t.Fatal(err)
	}
	r2, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = r2.Close() })
	assertS3NotRegistered(t, r2)
	if e, _ := r2.GetAccount("eve"); !e.Active {
		t.Fatalf("重开后无关新账户应存在且可用: %+v", e)
	}

	// 读写恢复后该编号可以重新登记，以本次提交的创建账户与文字元数据为准
	// （而非失败那次的 alice/"系列三" 或 bob 的尝试），保存成功才显示为
	// 未封存的新系列。
	if err := r2.CreateSeries("s3", "alice", "系列三-恢复后"); err != nil {
		t.Fatalf("恢复后重新登记应成功保存: %v", err)
	}
	s, err := r2.GetSeries("s3")
	if err != nil || s.Sealed || s.CreatorID != "alice" || s.Metadata != "系列三-恢复后" {
		t.Fatalf("重新登记应以本次提交为准且未封存: %+v, err %v", s, err)
	}

	// 保存成功后新系列才能按已有规则发行藏品；此前被幻影系列拒绝、未占用
	// 的请求内容现在可以正常执行（藏品编号、请求号此前都未被消耗）。
	if _, err := r2.Issue(IssueRequest{
		Operator: "alice", Reason: "恢复后发行 i2", RequestID: "ri-s3-ok",
		ItemID: "i2", SeriesID: "s3", BatchNo: "b3", HolderID: "alice",
	}); err != nil {
		t.Fatalf("保存成功的系列应能发行藏品: %v", err)
	}
	if h, _ := r2.GetHolding("i2"); h.OwnerID != "alice" || h.Version != 1 {
		t.Fatalf("新系列发行结果异常: %+v", h)
	}

	// 再次落盘视角：重开后 s3 以新元数据存在且未封存，持有与历史一致。
	if err := r2.Close(); err != nil {
		t.Fatal(err)
	}
	r3, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = r3.Close() })
	if s, _ := r3.GetSeries("s3"); s.Sealed || s.CreatorID != "alice" || s.Metadata != "系列三-恢复后" {
		t.Fatalf("重开后 s3 应以本次提交为准且未封存: %+v", s)
	}
	if h, _ := r3.GetHolding("i2"); h.OwnerID != "alice" || h.Version != 1 {
		t.Fatalf("重开后 i2 持有异常: %+v", h)
	}
	if hist, _ := r3.History("i1"); len(hist) != 2 {
		t.Fatalf("重开后 i1 历史应保持 2 条")
	}
	if hist, _ := r3.History("i2"); len(hist) != 1 {
		t.Fatalf("重开后 i2 历史应有 1 条")
	}
}

// TestCreateSeriesSaveFailureReadableSameRegistry 覆盖：写入失败但原数据仍
// 可正常读取（commit 据磁盘内容重建状态）时得到相同保障——同一登记册上该
// 编号从未登记；恢复后重新登记须完整保存一次才生效。
func TestCreateSeriesSaveFailureReadableSameRegistry(t *testing.T) {
	r := mustCreate(t, tempDir(t))
	seedCreateSeriesWorld(t, r)

	// 只阻断写入，不破坏数据文件：rename 之前失败，原快照完好可读。
	blockBatchSave(t, r)
	err := r.CreateSeries("s3", "alice", "系列三")
	assertSaveFailureError(t, err)
	assertS3NotRegistered(t, r)

	// 保存条件未恢复，换一个元数据再次登记同样必须实际尝试保存。
	err = r.CreateSeries("s3", "alice", "系列三-重试")
	assertSaveFailureError(t, err)
	assertS3NotRegistered(t, r)

	restoreBatchSave(t, r)
	if err := r.CreateSeries("s3", "alice", "系列三-恢复后"); err != nil {
		t.Fatalf("恢复后重新登记应成功保存: %v", err)
	}
	if s, _ := r.GetSeries("s3"); s.Sealed || s.CreatorID != "alice" || s.Metadata != "系列三-恢复后" {
		t.Fatalf("重新登记保存成功后 s3 应未封存且以本次元数据为准: %+v", s)
	}
}

// TestCreateSeriesSaveFailureRetryAfterReopen 覆盖磁盘视角：登记保存失败
// （原数据可读）后关闭重开，看到的仍是登记前状态；恢复保存后重新登记正常
// 落盘，而不是把一次从未保存的登记当成既成事实。
func TestCreateSeriesSaveFailureRetryAfterReopen(t *testing.T) {
	dir := tempDir(t)
	r := mustCreate(t, dir)
	seedCreateSeriesWorld(t, r)

	blockBatchSave(t, r)
	err := r.CreateSeries("s3", "alice", "系列三")
	assertSaveFailureError(t, err)
	if err := r.Close(); err != nil {
		t.Fatal(err)
	}
	r2, err := Open(dir)
	if err != nil {
		t.Fatalf("失败后原登记册必须仍能正常读取: %v", err)
	}
	t.Cleanup(func() { _ = r2.Close() })
	assertS3NotRegistered(t, r2)

	restoreBatchSave(t, r2)
	if err := r2.CreateSeries("s3", "alice", "系列三-恢复后"); err != nil {
		t.Fatalf("重开后重新登记应成功: %v", err)
	}
	if s, _ := r2.GetSeries("s3"); s.Sealed || s.Metadata != "系列三-恢复后" {
		t.Fatalf("重开后登记保存成功才应存在且未封存、以本次元数据为准: %+v", s)
	}
}

// TestCreateSeriesBusinessRejectionsWhileUnwritable 保留登记入口原有约定：
// 空白编号返回 ErrInvalidArgument，创建账户不存在返回 ErrNotFound、已停用
// 返回 ErrAccountInactive，已成功登记的系列编号（无论是否封存）返回
// ErrAlreadyExists 且不覆盖原创建账户、元数据或封存状态——这些业务拒绝不
// 触发保存，数据位置恰好不可写时也不能变成保存错误。文字元数据允许为空：
// 新编号配空元数据在不可写时仍实际尝试保存并返回保存错误、不留下系列，
// 恢复后成功登记为空元数据的未封存系列。
func TestCreateSeriesBusinessRejectionsWhileUnwritable(t *testing.T) {
	r := mustCreate(t, tempDir(t))
	seedCreateSeriesWorld(t, r)

	blockBatchSave(t, r)

	if err := r.CreateSeries("   ", "alice", "x"); !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("空白编号应返回 ErrInvalidArgument，且不变成保存错误: %v", err)
	}
	// 创建账户不存在：引用类拒绝，不触发保存。
	if err := r.CreateSeries("s4", "dave", "x"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("不存在的创建账户应返回 ErrNotFound，且不变成保存错误: %v", err)
	}
	// 创建账户已停用。
	if err := r.CreateSeries("s4", "carol", "x"); !errors.Is(err, ErrAccountInactive) {
		t.Fatalf("已停用创建账户应返回 ErrAccountInactive，且不变成保存错误: %v", err)
	}
	// 未封存的已登记编号：ErrAlreadyExists，记录不被覆盖。
	if err := r.CreateSeries("s1", "bob", "被覆盖的系列"); !errors.Is(err, ErrAlreadyExists) {
		t.Fatalf("已登记编号应返回 ErrAlreadyExists，且不变成保存错误: %v", err)
	}
	// 已封存的已登记编号同样永久占用：ErrAlreadyExists，封存不被解除。
	if err := r.CreateSeries("s2", "bob", "被覆盖的系列"); !errors.Is(err, ErrAlreadyExists) {
		t.Fatalf("已封存系列的编号仍应返回 ErrAlreadyExists: %v", err)
	}
	if s, _ := r.GetSeries("s1"); s.Sealed || s.CreatorID != "alice" || s.Metadata != "系列一" {
		t.Fatalf("业务拒绝不应改动 s1 记录: %+v", s)
	}
	if s, _ := r.GetSeries("s2"); !s.Sealed || s.CreatorID != "alice" || s.Metadata != "系列二" {
		t.Fatalf("业务拒绝不应改动 s2 记录或解除封存: %+v", s)
	}
	if c, _ := r.GetAccount("carol"); c.Active {
		t.Fatalf("业务拒绝不应恢复 carol 的停用: %+v", c)
	}

	// 文字元数据允许为空：新编号仍须真实保存，不可写时返回当次保存错误
	// 且不留系列。
	err := r.CreateSeries("s3", "alice", "")
	assertSaveFailureError(t, err)
	if _, err := r.GetSeries("s3"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("保存失败后 s3 不应存在: %v", err)
	}

	restoreBatchSave(t, r)
	if err := r.CreateSeries("s3", "alice", ""); err != nil {
		t.Fatalf("恢复后空元数据登记应成功保存: %v", err)
	}
	if s, _ := r.GetSeries("s3"); s.Sealed || s.CreatorID != "alice" || s.Metadata != "" {
		t.Fatalf("空元数据系列保存成功后应未封存且元数据为空: %+v", s)
	}
}
