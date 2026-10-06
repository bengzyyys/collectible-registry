package registry

import (
	"errors"
	"os"
	"testing"
)

// 本文件为系列封存补充"封存保存失败"场景的回归保障。系列已创建且尚未封存
// 时，SealSeries 的封存状态尚未原子替换原数据就发生写入失败（如数据位置
// 暂时无法写入），这样的失败不是一次有效封存：必须返回本次实际的保存错误，
// 不能返回成功，也不能用失败后重新读取原数据时的错误取代它。即使原数据仍
// 在、却暂时无法读取或解析、状态未能按磁盘重建，同一个仍打开的登记册也
// 必须保持封存前的样子——系列编号、创建账户、文字元数据不变且仍未封存，
// 满足原有发行条件时仍可发行；已发行藏品的持有人、版本与发行、转让历史
// 原样保留；其他系列此前已经成功封存的状态不被解封。原数据仍可读取的普通
// 保存失败同样撤销本次变化，不依赖重新读取成功。保存条件未恢复时再次封存
// 仍须报保存错误（不能因内存里的幻影封存直接返回成功）；随后另一次无关
// 操作成功保存也不会夹带这次未保存的封存；读写恢复后重新提交封存，成功
// 保存后才显示已封存，并按原有规则拒绝单件与整批发行，已发行藏品仍可转让。
//
// 失败注入与其他场景共用：在 .registry.json.tmp 上预建目录，save 在
// OpenFile 阶段即以 EISDIR 失败（rename 之前）；部分用例同时把数据文件
// 改写为无法解析的内容，覆盖"原数据暂时无法读取、commit 无法按磁盘重建
// 状态"。

// seedSealWorld 建立封存保存失败用例的初始世界：
//   - alice（创作者）、bob 两个已登记账户；
//   - alice 创建系列 s1（元数据"系列一"），发行 i1 给自己后转让给 bob：
//     i1 现为 bob 版本 2，用于核对封存失败不改变已有藏品的持有人、版本
//     与历史，以及封存成功后已发行藏品仍可转让；
//   - alice 另创建系列 s2 并在失败注入前成功封存，用于核对回滚不会把
//     其他系列的封存解封。
func seedSealWorld(t *testing.T, r *Registry) {
	t.Helper()
	if err := r.RegisterAccount("alice", "元-alice"); err != nil {
		t.Fatal(err)
	}
	if err := r.RegisterAccount("bob", "元-bob"); err != nil {
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

// assertSealDidNotTakeEffect 在（封存保存失败后的）同一个已打开登记册上
// 核对：这次封存如同从未提交，且失败前已有的记录完整保留。
func assertSealDidNotTakeEffect(t *testing.T, r *Registry) {
	t.Helper()
	// 目标系列仍未封存，编号、创建账户与文字元数据与调用前一致。
	s, err := r.GetSeries("s1")
	if err != nil {
		t.Fatalf("GetSeries s1: %v", err)
	}
	if s.Sealed || s.ID != "s1" || s.CreatorID != "alice" || s.Metadata != "系列一" {
		t.Fatalf("保存失败后 s1 应保持封存前状态: %+v", s)
	}
	// 其他系列此前已经成功封存的状态不能被这次回滚解封。
	if s2, err := r.GetSeries("s2"); err != nil || !s2.Sealed {
		t.Fatalf("s2 应保持已封存: %+v, err %v", s2, err)
	}
	// 已发行藏品的持有人、版本与发行、转让历史原样保留。
	h, err := r.GetHolding("i1")
	if err != nil || h.OwnerID != "bob" || h.Version != 2 {
		t.Fatalf("i1 持有被失败的封存波及: %+v, err %v", h, err)
	}
	if hist, err := r.History("i1"); err != nil || len(hist) != 2 ||
		hist[0].Kind != "issue" || hist[1].Kind != "transfer" || hist[1].ToID != "bob" {
		t.Fatalf("i1 历史被改变: %+v, err %v", hist, err)
	}
}

// TestSealSaveFailureUnreadableSameRegistry 覆盖核心场景：封存在写入阶段
// 失败，且原数据同时被改写为无法解析（commit 无法按磁盘重建状态）。返回
// 保存错误而非成功；同一登记册上封存从未生效，旧记录完整。保存仍失败时
// 再次封存依旧报保存错误（不能因内存里的幻影封存直接返回成功）。读写恢复
// 后先做一次无关操作成功保存，不夹带未保存的封存；系列随后仍可正常发行；
// 再次封存重新保存成功后才显示已封存，并按原有规则拒绝单件与整批发行，
// 此前发行的藏品仍可转让。
func TestSealSaveFailureUnreadableSameRegistry(t *testing.T) {
	r := mustCreate(t, tempDir(t))
	seedSealWorld(t, r)

	orig, err := os.ReadFile(dataFile(r.dir))
	if err != nil {
		t.Fatal(err)
	}
	blockBatchSave(t, r)
	if err := os.WriteFile(dataFile(r.dir), []byte("{corrupt"), fileMode); err != nil {
		t.Fatal(err)
	}

	// 封存落盘失败：返回实际保存错误，不能是任何业务拒绝，也不能成功。
	err = r.SealSeries("s1", "alice")
	assertSaveFailureError(t, err)
	assertSealDidNotTakeEffect(t, r)

	// 保存条件未恢复时再次提交：仍须报保存错误。修复前内存中已是封存态，
	// 这里会被幂等分支直接返回成功——必须保持失败。
	err = r.SealSeries("s1", "alice")
	assertSaveFailureError(t, err)
	assertSealDidNotTakeEffect(t, r)

	// 恢复正常读写；先让一次无关操作成功保存，不能夹带这笔未保存的封存。
	if err := os.WriteFile(dataFile(r.dir), orig, fileMode); err != nil {
		t.Fatal(err)
	}
	restoreBatchSave(t, r)
	if err := r.RegisterAccount("eve", "路人"); err != nil {
		t.Fatalf("无关操作应能成功保存: %v", err)
	}
	assertSealDidNotTakeEffect(t, r)

	// 读写恢复后系列满足原有发行条件，仍可继续发行。
	if _, err := r.Issue(IssueRequest{
		Operator: "alice", Reason: "恢复后发行 i2", RequestID: "ri-i2",
		ItemID: "i2", SeriesID: "s1", BatchNo: "b1", HolderID: "alice",
	}); err != nil {
		t.Fatalf("封存保存失败后系列应仍可发行: %v", err)
	}

	// 之后再次提交封存：重新完成保存，成功返回后才显示已封存。
	if err := r.SealSeries("s1", "alice"); err != nil {
		t.Fatalf("恢复后封存应成功保存: %v", err)
	}
	if s, _ := r.GetSeries("s1"); !s.Sealed {
		t.Fatalf("封存保存成功后 s1 应已封存: %+v", s)
	}

	// 封存后按原有规则拒绝单件发行。
	if _, err := r.Issue(IssueRequest{
		Operator: "alice", Reason: "封存后发行", RequestID: "ri-blocked",
		ItemID: "i3", SeriesID: "s1", BatchNo: "b1", HolderID: "alice",
	}); !errors.Is(err, ErrSeriesSealed) {
		t.Fatalf("封存后单件发行应被 ErrSeriesSealed 拒绝: %v", err)
	}
	// 封存后按原有规则拒绝整批发行。
	if _, err := r.IssueBatch(IssueBatchRequest{
		Operator: "alice", Reason: "封存后整批发行", RequestID: "rib-blocked",
		SeriesID: "s1", BatchNo: "b2",
		Entries: []IssueBatchEntry{{ItemID: "i4", HolderID: "alice"}},
	}); !errors.Is(err, ErrSeriesSealed) {
		t.Fatalf("封存后整批发行应被 ErrSeriesSealed 拒绝: %v", err)
	}
	// 此前发行的藏品仍可转让（i1 现为 bob 版本 2）。
	if _, err := r.Transfer(TransferRequest{
		Operator: "bob", Reason: "封存后转让", RequestID: "rt-i1-alice",
		ItemID: "i1", ExpectedOwner: "bob", ExpectedVer: 2, ToID: "alice",
	}); err != nil {
		t.Fatalf("封存后已发行藏品应仍可转让: %v", err)
	}
	if h, _ := r.GetHolding("i1"); h.OwnerID != "alice" || h.Version != 3 {
		t.Fatalf("封存后转让结果异常: %+v", h)
	}

	// 落盘视角：关闭重开后封存已持久化，其他系列状态与持有、历史一致。
	dir := r.dir
	if err := r.Close(); err != nil {
		t.Fatal(err)
	}
	r2, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = r2.Close() })
	if s, _ := r2.GetSeries("s1"); !s.Sealed || s.CreatorID != "alice" || s.Metadata != "系列一" {
		t.Fatalf("重开后 s1 应保持封存且元数据不变: %+v", s)
	}
	if s2, _ := r2.GetSeries("s2"); !s2.Sealed {
		t.Fatalf("重开后 s2 应仍封存: %+v", s2)
	}
	if e, _ := r2.GetAccount("eve"); !e.Active {
		t.Fatalf("重开后无关新账户应存在且可用: %+v", e)
	}
	if h, _ := r2.GetHolding("i1"); h.OwnerID != "alice" || h.Version != 3 {
		t.Fatalf("重开后 i1 持有异常: %+v", h)
	}
	if hist, err := r2.History("i1"); err != nil || len(hist) != 3 {
		t.Fatalf("重开后 i1 历史异常: %+v, err %v", hist, err)
	}
}

// TestSealSaveFailureReadableSameRegistry 覆盖：写入失败但原数据仍可正常
// 读取（commit 据磁盘内容重建状态）时，得到相同的保障——同一登记册上封存
// 从未发生；恢复后重新封存须完整保存一次才生效；已成功封存的系列重复封存
// 直接成功、不要求再次写入。
func TestSealSaveFailureReadableSameRegistry(t *testing.T) {
	r := mustCreate(t, tempDir(t))
	seedSealWorld(t, r)

	// 只阻断写入，不破坏数据文件：rename 之前失败，原快照完好可读。
	blockBatchSave(t, r)
	err := r.SealSeries("s1", "alice")
	assertSaveFailureError(t, err)
	assertSealDidNotTakeEffect(t, r)

	restoreBatchSave(t, r)
	if err := r.SealSeries("s1", "alice"); err != nil {
		t.Fatalf("恢复后重新封存应成功保存: %v", err)
	}
	if s, _ := r.GetSeries("s1"); !s.Sealed {
		t.Fatalf("重新封存保存成功后 s1 应已封存: %+v", s)
	}
	// 再次封存已封存系列仍直接成功，且不再写入（否则会被失败注入拦住）。
	blockBatchSave(t, r)
	if err := r.SealSeries("s1", "alice"); err != nil {
		t.Fatalf("已封存系列重复封存应直接成功、不要求写入: %v", err)
	}
}

// TestSealSaveFailureRetryAfterReopen 覆盖磁盘视角：封存保存失败（原数据
// 可读）后关闭重开，看到的仍是封存前状态；恢复保存后重新封存正常落盘，
// 而不是把一次从未保存的封存当成既成事实。
func TestSealSaveFailureRetryAfterReopen(t *testing.T) {
	dir := tempDir(t)
	r := mustCreate(t, dir)
	seedSealWorld(t, r)

	blockBatchSave(t, r)
	err := r.SealSeries("s1", "alice")
	assertSaveFailureError(t, err)
	if err := r.Close(); err != nil {
		t.Fatal(err)
	}
	r2, err := Open(dir)
	if err != nil {
		t.Fatalf("失败后原登记册必须仍能正常读取: %v", err)
	}
	t.Cleanup(func() { _ = r2.Close() })
	assertSealDidNotTakeEffect(t, r2)

	restoreBatchSave(t, r2)
	if err := r2.SealSeries("s1", "alice"); err != nil {
		t.Fatalf("重开后重新封存应成功: %v", err)
	}
	if s, _ := r2.GetSeries("s1"); !s.Sealed {
		t.Fatalf("重开后封存保存成功才应已封存: %+v", s)
	}
	// 封存后发行按原有规则被拒绝。
	if _, err := r2.Issue(IssueRequest{
		Operator: "alice", Reason: "封存后发行", RequestID: "ri-after-seal",
		ItemID: "i2", SeriesID: "s1", BatchNo: "b1", HolderID: "alice",
	}); !errors.Is(err, ErrSeriesSealed) {
		t.Fatalf("封存后发行应被 ErrSeriesSealed 拒绝: %v", err)
	}
	if h, _ := r2.GetHolding("i1"); h.OwnerID != "bob" || h.Version != 2 {
		t.Fatalf("被拒绝的发行不应改变持有: %+v", h)
	}
}

// TestSealUnknownAndForbidden 保留现有封存入口与错误判定：系列不存在返回
// 可由 errors.Is 判断的 ErrNotFound，非创建账户返回 ErrForbidden（即使当前
// 正无法写入，也不变成保存错误）；已成功封存的系列再次封存直接成功、保持
// 封存，不要求再次写入；写入受限时未封存系列的封存仍须真实保存并返回保存
// 错误，失败不改动系列状态。
func TestSealUnknownAndForbidden(t *testing.T) {
	r := mustCreate(t, tempDir(t))
	seedSealWorld(t, r)

	if err := r.SealSeries("ghost", "alice"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("不存在的系列封存应返回 ErrNotFound: %v", err)
	}
	if err := r.SealSeries("s1", "bob"); !errors.Is(err, ErrForbidden) {
		t.Fatalf("非创建账户封存应返回 ErrForbidden: %v", err)
	}
	// 写入被阻断时引用与权限检查仍先于保存发生，判定方式不变。
	blockBatchSave(t, r)
	if err := r.SealSeries("ghost", "alice"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("写入受限时不存在的系列仍应返回 ErrNotFound: %v", err)
	}
	if err := r.SealSeries("s1", "bob"); !errors.Is(err, ErrForbidden) {
		t.Fatalf("写入受限时非创建账户仍应返回 ErrForbidden: %v", err)
	}
	// 已封存系列重复封存直接成功，不触发写入（否则会被失败注入拦住）。
	if err := r.SealSeries("s2", "alice"); err != nil {
		t.Fatalf("已封存系列再次封存应直接成功且不写入: %v", err)
	}
	if s2, _ := r.GetSeries("s2"); !s2.Sealed {
		t.Fatalf("s2 应保持封存: %+v", s2)
	}
	// 写入受限时未封存系列的封存仍须真实保存并返回保存错误。
	err := r.SealSeries("s1", "alice")
	assertSaveFailureError(t, err)
	if s, _ := r.GetSeries("s1"); s.Sealed {
		t.Fatalf("保存失败后 s1 应仍未封存: %+v", s)
	}
}
