package registry

import (
	"errors"
	"os"
	"testing"
)

// 本文件为系列登记补充"登记保存失败"场景的回归保障。创建账户已登记且可用、
// 系列编号尚未使用时调用 CreateSeries，系列是否存在始终以一次保存完成为准：
// 新系列尚未原子替换原数据就发生写入失败（如数据位置暂时无法写入），这样的
// 失败不是一次有效登记——必须返回本次实际的保存错误，不能返回成功或编号
// 冲突，也不能用失败后重新读取原数据时的错误取代它。即使原数据仍在、却
// 暂时无法读取或解析、状态未能按磁盘重建，同一个仍打开的登记册也必须立即
// 表现为该编号从未登记：GetSeries 返回 ErrNotFound，不能把它当成已存在的
// 系列继续发行（单件与整批）或封存。失败不占用系列编号：保存条件未恢复时
// 用该编号再次登记仍实际尝试保存并返回当次保存错误，而不是因上次失败遗留
// 的系列返回 ErrAlreadyExists；随后另一次合法操作成功保存不夹带这个未保存
// 的系列，关闭再打开登记册后它仍不存在。读写恢复后该编号可以重新登记，以
// 本次提交的创建账户与文字元数据为准，保存成功才显示为未封存的新系列，并
// 可按已有规则发行藏品。原数据仍可读取的普通保存失败得到同样结果。此前
// 成功登记的账户、系列及其封存状态，以及已有藏品的持有人、版本与历史都
// 原样保留。
//
// 失败注入与其他场景共用：在 .registry.json.tmp 上预建目录，save 在
// OpenFile 阶段即以 EISDIR 失败（rename 之前）；部分用例同时把数据文件
// 改写为无法解析的内容，覆盖"原数据暂时无法读取、commit 无法按磁盘重建
// 状态"。

// seedCreateSeriesWorld 建立系列登记保存失败用例的初始世界：
//   - alice（创作者，元数据"元-alice"）、bob（"元-bob"）两个可用账户，
//     carol（"元-carol"）在失败注入前已成功停用，用于核对回滚不改变账户
//     状态，以及停用的创建账户在不可写时仍按 ErrAccountInactive 拒绝；
//   - alice 创建系列 s1（"系列一"），发行 i1 给自己后转让给 bob：i1 现为
//     bob 版本 2；alice 另发行 i2 给自己持有，用于核对失败的登记不改变
//     任何藏品的持有人、版本与历史；
//   - alice 另创建系列 s2（"系列二"）并在失败注入前成功封存，用于核对
//     回滚不会把其他系列的封存解封，也不覆盖已登记系列的任何字段。
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
	if _, err := r.Issue(IssueRequest{
		Operator: "alice", Reason: "首发 i2", RequestID: "ri-i2",
		ItemID: "i2", SeriesID: "s1", BatchNo: "b1", HolderID: "alice",
	}); err != nil {
		t.Fatal(err)
	}
	if err := r.CreateSeries("s2", "alice", "系列二"); err != nil {
		t.Fatal(err)
	}
	if err := r.SealSeries("s2", "alice"); err != nil {
		t.Fatal(err)
	}
	if err := r.DeactivateAccount("carol"); err != nil {
		t.Fatal(err)
	}
}

// assertSeriesNotRegistered 在（登记保存失败后的）同一个已打开登记册上核对：
// 新系列 s3 如同从未登记，且失败前已有的账户、系列封存状态、藏品持有与
// 历史完整保留。
func assertSeriesNotRegistered(t *testing.T, r *Registry) {
	t.Helper()
	if _, err := r.GetSeries("s3"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("保存失败后 s3 应表现为从未登记，GetSeries 返回: %+v, err %v",
			err, err)
	}
	// 不能把幻影系列当成已存在的系列发行：单件发行在系列层按不存在返回
	// ErrNotFound，而不是按封存、无权或编号占用拒绝。
	if _, err := r.Issue(IssueRequest{
		Operator: "alice", Reason: "向幻影系列发行", RequestID: "ri-ghost-single",
		ItemID: "ig1", SeriesID: "s3", BatchNo: "b1", HolderID: "alice",
	}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("幻影系列的单件发行应被 ErrNotFound 拒绝: %v", err)
	}
	// 整批发行同样不能把幻影系列当成已存在的系列。
	if _, err := r.IssueBatch(IssueBatchRequest{
		Operator: "alice", Reason: "向幻影系列整批发行", RequestID: "rib-ghost",
		SeriesID: "s3", BatchNo: "b1",
		Entries: []IssueBatchEntry{{ItemID: "ig2", HolderID: "alice"}},
	}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("幻影系列的整批发行应被 ErrNotFound 拒绝: %v", err)
	}
	// 不能把幻影系列当成已存在的系列封存。
	if err := r.SealSeries("s3", "alice"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("幻影系列的封存应被 ErrNotFound 拒绝: %v", err)
	}
	// 被拒绝的发行不占用藏品编号：ig1/ig2 仍可在恢复后的合法系列中使用，
	// 但此处至少先确认它们没有被登记成藏品。
	if _, err := r.GetItem("ig1"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("被拒绝的单件发行不应留下藏品: %v", err)
	}
	if _, err := r.GetItem("ig2"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("被拒绝的整批发行不应留下藏品: %v", err)
	}

	// 此前成功登记的系列及其创建账户、元数据、封存状态原样保留。
	if s, err := r.GetSeries("s1"); err != nil || s.Sealed ||
		s.ID != "s1" || s.CreatorID != "alice" || s.Metadata != "系列一" {
		t.Fatalf("s1 记录被失败的登记波及: %+v, err %v", s, err)
	}
	if s2, err := r.GetSeries("s2"); err != nil || !s2.Sealed ||
		s2.CreatorID != "alice" || s2.Metadata != "系列二" {
		t.Fatalf("s2 应保持已封存且字段不变: %+v, err %v", s2, err)
	}

	// 账户的可用/停用状态不变。
	if a, err := r.GetAccount("alice"); err != nil || !a.Active || a.Metadata != "元-alice" {
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

// TestCreateSeriesSaveFailureUnreadableSameRegistry 覆盖核心场景：登记在
// 写入阶段失败，且原数据同时被改写为无法解析（commit 无法按磁盘重建状态）。
// 返回保存错误而非成功或编号冲突；同一登记册上该编号从未登记，旧记录完整。
// 保存仍失败时用同一编号再次登记依旧实际尝试保存并报保存错误（不能因遗留
// 系列返回 ErrAlreadyExists）。读写恢复后先让一次无关操作成功保存，不夹带
// 未保存的系列；关闭再打开它仍不存在。随后该编号以新的创建账户与文字元数据
// 重新登记，保存成功才显示为未封存的新系列，可按已有规则发行，重开后以
// 这次提交为准持久存在。
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
	err = r.CreateSeries("s3", "alice", "新系列")
	assertSaveFailureError(t, err)
	assertSeriesNotRegistered(t, r)

	// 保存条件未恢复时用同一编号再次登记：仍须真实保存并报当次保存错误，
	// 修复前内存中遗留的 s3 会让这里直接返回 ErrAlreadyExists。
	err = r.CreateSeries("s3", "alice", "新系列-重试")
	assertSaveFailureError(t, err)
	assertSeriesNotRegistered(t, r)

	// 恢复正常读写；先让一次无关操作成功保存，不能夹带这个未保存的系列。
	if err := os.WriteFile(dataFile(r.dir), orig, fileMode); err != nil {
		t.Fatal(err)
	}
	restoreBatchSave(t, r)
	if err := r.RegisterAccount("eve", "路人"); err != nil {
		t.Fatalf("无关操作应能成功保存: %v", err)
	}
	assertSeriesNotRegistered(t, r)
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
	assertSeriesNotRegistered(t, r2)
	if e, _ := r2.GetAccount("eve"); !e.Active {
		t.Fatalf("重开后无关新账户应存在且可用: %+v", e)
	}

	// 读写恢复后该编号可以重新登记，以本次提交的创建账户与文字元数据为准
	// （而非失败那次的"新系列"），保存成功才显示为未封存的新系列。
	if err := r2.CreateSeries("s3", "alice", "新系列-恢复后"); err != nil {
		t.Fatalf("恢复后重新登记应成功保存: %v", err)
	}
	if s, _ := r2.GetSeries("s3"); s.Sealed || s.CreatorID != "alice" ||
		s.Metadata != "新系列-恢复后" {
		t.Fatalf("重新登记应以本次提交为准且未封存: %+v", s)
	}

	// 保存成功后才能按已有规则在该系列下发行藏品。
	if _, err := r2.Issue(IssueRequest{
		Operator: "alice", Reason: "恢复后发行 i3", RequestID: "ri-i3",
		ItemID: "i3", SeriesID: "s3", BatchNo: "b1", HolderID: "alice",
	}); err != nil {
		t.Fatalf("保存成功的系列应能发行藏品: %v", err)
	}
	if h, _ := r2.GetHolding("i3"); h.OwnerID != "alice" || h.Version != 1 {
		t.Fatalf("发行结果异常: %+v", h)
	}
	if hist, _ := r2.History("i3"); len(hist) != 1 || hist[0].Kind != "issue" {
		t.Fatalf("新系列藏品的发行历史异常: %+v", hist)
	}

	// 再次落盘视角：重开后 s3 以新元数据存在且未封存，藏品与历史一致。
	if err := r2.Close(); err != nil {
		t.Fatal(err)
	}
	r3, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = r3.Close() })
	if s, _ := r3.GetSeries("s3"); s.Sealed || s.Metadata != "新系列-恢复后" ||
		s.CreatorID != "alice" {
		t.Fatalf("重开后 s3 应以新元数据存在且未封存: %+v", s)
	}
	if h, _ := r3.GetHolding("i3"); h.OwnerID != "alice" || h.Version != 1 {
		t.Fatalf("重开后 i3 持有异常: %+v", h)
	}
}

// TestCreateSeriesSaveFailureReadableSameRegistry 覆盖：写入失败但原数据
// 仍可正常读取（commit 据磁盘内容重建状态）时得到相同保障——同一登记册上
// 该编号从未登记；恢复后重新登记须完整保存一次才生效，新系列未封存并可
// 发行。
func TestCreateSeriesSaveFailureReadableSameRegistry(t *testing.T) {
	r := mustCreate(t, tempDir(t))
	seedCreateSeriesWorld(t, r)

	// 只阻断写入，不破坏数据文件：rename 之前失败，原快照完好可读。
	blockBatchSave(t, r)
	err := r.CreateSeries("s3", "alice", "新系列")
	assertSaveFailureError(t, err)
	assertSeriesNotRegistered(t, r)

	// 保存条件未恢复，换一个元数据再次登记同样必须实际尝试保存。
	err = r.CreateSeries("s3", "alice", "新系列-重试")
	assertSaveFailureError(t, err)
	assertSeriesNotRegistered(t, r)

	restoreBatchSave(t, r)
	if err := r.CreateSeries("s3", "alice", "新系列-恢复后"); err != nil {
		t.Fatalf("恢复后重新登记应成功保存: %v", err)
	}
	if s, _ := r.GetSeries("s3"); s.Sealed || s.Metadata != "新系列-恢复后" {
		t.Fatalf("重新登记保存成功后 s3 应未封存且以本次元数据为准: %+v", s)
	}
	if _, err := r.Issue(IssueRequest{
		Operator: "alice", Reason: "恢复后发行 i3", RequestID: "ri-i3",
		ItemID: "i3", SeriesID: "s3", BatchNo: "b1", HolderID: "alice",
	}); err != nil {
		t.Fatalf("重新登记的系列保存成功后应能发行: %v", err)
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
	err := r.CreateSeries("s3", "alice", "新系列")
	assertSaveFailureError(t, err)
	if err := r.Close(); err != nil {
		t.Fatal(err)
	}
	r2, err := Open(dir)
	if err != nil {
		t.Fatalf("失败后原登记册必须仍能正常读取: %v", err)
	}
	t.Cleanup(func() { _ = r2.Close() })
	assertSeriesNotRegistered(t, r2)

	restoreBatchSave(t, r2)
	if err := r2.CreateSeries("s3", "alice", "新系列-恢复后"); err != nil {
		t.Fatalf("重开后重新登记应成功: %v", err)
	}
	if s, _ := r2.GetSeries("s3"); s.Sealed || s.Metadata != "新系列-恢复后" {
		t.Fatalf("重开后登记保存成功才应存在、未封存且以本次元数据为准: %+v", s)
	}
	if _, err := r2.Issue(IssueRequest{
		Operator: "alice", Reason: "恢复后发行 i3", RequestID: "ri-i3",
		ItemID: "i3", SeriesID: "s3", BatchNo: "b1", HolderID: "alice",
	}); err != nil {
		t.Fatalf("重开后新系列应能正常发行: %v", err)
	}
	if h, _ := r2.GetHolding("i3"); h.OwnerID != "alice" || h.Version != 1 {
		t.Fatalf("发行结果异常: %+v", h)
	}
}

// TestCreateSeriesBusinessRejectionsWhileUnwritable 保留登记入口原有约定：
// 空白系列编号返回 ErrInvalidArgument，创建账户不存在返回 ErrNotFound、
// 已停用返回 ErrAccountInactive，已成功登记的系列编号（无论是否封存）
// 返回 ErrAlreadyExists 且不覆盖创建账户、元数据或封存状态——这些业务拒绝
// 不触发保存，数据位置恰好不可写时也不能变成保存错误。文字元数据允许为空：
// 新编号配空元数据在不可写时仍实际尝试保存并返回保存错误、不留下系列，
// 恢复后成功登记为空元数据的未封存系列。
func TestCreateSeriesBusinessRejectionsWhileUnwritable(t *testing.T) {
	r := mustCreate(t, tempDir(t))
	seedCreateSeriesWorld(t, r)

	blockBatchSave(t, r)

	if err := r.CreateSeries("   ", "alice", "x"); !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("空白系列编号应返回 ErrInvalidArgument，且不变成保存错误: %v", err)
	}
	// 创建账户不存在：ErrNotFound，不触发保存。
	if err := r.CreateSeries("s3", "dave", "x"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("创建账户不存在应返回 ErrNotFound，且不变成保存错误: %v", err)
	}
	// 创建账户已停用：ErrAccountInactive，不触发保存。
	if err := r.CreateSeries("s3", "carol", "x"); !errors.Is(err, ErrAccountInactive) {
		t.Fatalf("创建账户已停用应返回 ErrAccountInactive，且不变成保存错误: %v", err)
	}
	// 未封存系列的编号已占用：ErrAlreadyExists，记录不被覆盖。
	if err := r.CreateSeries("s1", "bob", "被覆盖的元数据"); !errors.Is(err, ErrAlreadyExists) {
		t.Fatalf("已登记编号应返回 ErrAlreadyExists，且不变成保存错误: %v", err)
	}
	// 已封存系列的编号同样永久占用：ErrAlreadyExists，封存状态不被解除。
	if err := r.CreateSeries("s2", "bob", "被覆盖的元数据"); !errors.Is(err, ErrAlreadyExists) {
		t.Fatalf("已封存系列的编号仍应返回 ErrAlreadyExists: %v", err)
	}
	if s, _ := r.GetSeries("s1"); s.Sealed || s.CreatorID != "alice" ||
		s.Metadata != "系列一" {
		t.Fatalf("业务拒绝不应改动 s1 记录: %+v", s)
	}
	if s2, _ := r.GetSeries("s2"); !s2.Sealed || s2.CreatorID != "alice" ||
		s2.Metadata != "系列二" {
		t.Fatalf("业务拒绝不应改动 s2 记录或解除其封存: %+v", s2)
	}

	// 文字元数据允许为空：新编号仍须真实保存，不可写时返回当次保存错误且
	// 不留下系列。
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
		t.Fatalf("空元数据系列保存成功后应未封存、创建账户正确且元数据为空: %+v", s)
	}
}
