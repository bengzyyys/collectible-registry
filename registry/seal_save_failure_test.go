package registry

import (
	"errors"
	"os"
	"testing"
)

// 本文件为系列封存补充"封存保存失败"场景的回归保障。账户、系列与藏品均
// 已存在、目标系列尚未封存时，SealSeries 的封存状态尚未原子替换原数据就
// 发生写入失败（如数据位置暂时无法写入），这样的失败不是一次有效封存：必须
// 返回本次实际的保存错误，不能返回成功，也不能用失败后重新读取原数据时的
// 错误取代它。即使原数据仍在、却暂时无法读取或解析、状态未能按磁盘重建，
// 同一个仍打开的登记册也必须保持封存前的样子——系列仍未封存，编号、创建
// 账户与文字元数据不变，满足原有发行条件时可继续发行，已有藏品的持有人、
// 版本与发行、转让历史原样保留；其他系列此前已经成功保存的封存状态不被
// 恢复为未封存。原数据仍可读取的普通保存失败同样撤销本次变化，不依赖重新
// 读取成功。随后另一次无关操作成功保存也不会夹带这次未保存的封存；保存
// 条件恢复后创建账户再次封存须重新保存，成功返回后才显示已封存，并按现有
// 规则拒绝单件与整批发行，此前发行的藏品仍可转让。
//
// 失败注入与其他场景共用：在 .registry.json.tmp 上预建目录，save 在
// OpenFile 阶段即以 EISDIR 失败（rename 之前）；部分用例同时把数据文件
// 改写为无法解析的内容，覆盖"原数据暂时无法读取、commit 无法按磁盘重建
// 状态"。

// seedSealWorld 建立封存保存失败用例的初始世界：
//   - alice（创作者，元数据"元-alice"）、bob、carol 三个已登记账户；
//   - alice 创建系列 s1（元数据"系列-s1"），发行 i1 给自己后转让给 bob：
//     i1 现为 bob 版本 2；alice 另发行 i2 给自己：alice 仍持有 i2 版本 1，
//     用于核对封存失败不改变已有藏品的持有人、版本与发行、转让历史；
//   - alice 另建系列 s2 并在失败注入前成功封存，用于核对回滚不会把其他
//     系列此前已保存的封存状态恢复为未封存。
func seedSealWorld(t *testing.T, r *Registry) {
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
	if err := r.CreateSeries("s1", "alice", "系列-s1"); err != nil {
		t.Fatal(err)
	}
	if err := r.CreateSeries("s2", "alice", "系列-s2"); err != nil {
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
	if err := r.SealSeries("s2", "alice"); err != nil {
		t.Fatal(err)
	}
}

// assertSealDidNotTakeEffect 在（封存保存失败后的）同一个已打开登记册上
// 核对：这次封存如同从未提交，且失败前已有的记录完整保留。该助手只做查询，
// 不触发任何写入——保存条件受限时新的发行本就无法落盘，"仍具发行资格"在
// 读写恢复后另行验证。
func assertSealDidNotTakeEffect(t *testing.T, r *Registry) {
	t.Helper()
	// 目标系列仍未封存，编号、创建账户与文字元数据与调用前一致。
	s, err := r.GetSeries("s1")
	if err != nil {
		t.Fatalf("GetSeries s1: %v", err)
	}
	if s.Sealed || s.ID != "s1" || s.CreatorID != "alice" || s.Metadata != "系列-s1" {
		t.Fatalf("保存失败后 s1 应保持封存前状态: %+v", s)
	}
	// 其他系列此前已经成功保存的封存状态不能被这次回滚恢复为未封存。
	if s2, err := r.GetSeries("s2"); err != nil || !s2.Sealed {
		t.Fatalf("s2 应保持已封存: %+v, err %v", s2, err)
	}

	// 已有藏品的持有人、版本与发行、转让历史保留。
	h1, err := r.GetHolding("i1")
	if err != nil || h1.OwnerID != "bob" || h1.Version != 2 {
		t.Fatalf("i1 持有被失败的封存波及: %+v, err %v", h1, err)
	}
	h2, err := r.GetHolding("i2")
	if err != nil || h2.OwnerID != "alice" || h2.Version != 1 {
		t.Fatalf("i2 持有被改变: %+v, err %v", h2, err)
	}
	if hist, err := r.History("i1"); err != nil || len(hist) != 2 ||
		hist[1].Kind != "transfer" || hist[1].ToID != "bob" {
		t.Fatalf("i1 历史被改变: %+v, err %v", hist, err)
	}
	if hist, err := r.History("i2"); err != nil || len(hist) != 1 ||
		hist[0].Kind != "issue" || hist[0].ToID != "alice" {
		t.Fatalf("i2 历史被改变: %+v, err %v", hist, err)
	}
}

// TestSealSaveFailureUnreadableSameRegistry 覆盖核心场景：封存在写入阶段
// 失败，且原数据同时被改写为无法解析（commit 无法按磁盘重建状态）。返回
// 保存错误而非成功；同一登记册上封存从未生效，旧记录完整，系列仍可发行。
// 保存仍失败时再次封存依旧报保存错误（不能因内存里的幻影封存直接返回
// 成功）。读写恢复后先做一次无关操作成功保存，不夹带未保存的封存；随后
// 由创建账户重新封存，成功保存后才显示已封存，并按原有规则拒绝单件与
// 整批发行；此前发行的藏品仍可转让；其他系列的封存状态全程保留。
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
	if s, _ := r.GetSeries("s1"); s.Sealed {
		t.Fatalf("无关操作保存后 s1 仍应未封存: %+v", s)
	}
	if s2, _ := r.GetSeries("s2"); !s2.Sealed {
		t.Fatalf("s2 应全程保持已封存: %+v", s2)
	}

	// 恢复后未封存系列仍满足原有发行条件：单件发行成功（probe 在上一轮
	// 断言中已发行，整批再发行两件不同编号同样成功）。
	if _, err := r.IssueBatch(IssueBatchRequest{
		Operator: "alice", Reason: "恢复后整批发行", RequestID: "rb-after",
		SeriesID: "s1", BatchNo: "b2",
		Entries: []IssueBatchEntry{
			{ItemID: "i3", Metadata: "元-i3", HolderID: "bob"},
			{ItemID: "i4", Metadata: "元-i4", HolderID: "carol"},
		},
	}); err != nil {
		t.Fatalf("未封存系列整批发行应成功: %v", err)
	}

	// 由创建账户重新提交封存：重新完成保存，成功返回后才显示已封存。
	if err := r.SealSeries("s1", "alice"); err != nil {
		t.Fatalf("恢复后封存应成功保存: %v", err)
	}
	if s, _ := r.GetSeries("s1"); !s.Sealed {
		t.Fatalf("封存保存成功后 s1 应已封存: %+v", s)
	}

	// 封存后按原有规则拒绝单件发行。
	if _, err := r.Issue(IssueRequest{
		Operator: "alice", Reason: "封存后发行", RequestID: "ri-blocked",
		ItemID: "i5", SeriesID: "s1", BatchNo: "b1", HolderID: "bob",
	}); !errors.Is(err, ErrSeriesSealed) {
		t.Fatalf("封存后单件发行应被 ErrSeriesSealed 拒绝: %v", err)
	}
	// 封存后按原有规则拒绝整批发行。
	if res, err := r.IssueBatch(IssueBatchRequest{
		Operator: "alice", Reason: "封存后整批", RequestID: "rb-blocked",
		SeriesID: "s1", BatchNo: "b1",
		Entries: []IssueBatchEntry{{ItemID: "i6", HolderID: "bob"}},
	}); !errors.Is(err, ErrSeriesSealed) || !errors.Is(res.Err, ErrSeriesSealed) {
		t.Fatalf("封存后整批发行应被 ErrSeriesSealed 拒绝: err=%v res=%+v", err, res)
	}
	// 被拒绝的发行没有产生任何藏品或历史。
	if _, err := r.GetItem("i5"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("被拒绝的单件发行不应留下藏品: %v", err)
	}
	if _, err := r.GetItem("i6"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("被拒绝的整批发行不应留下藏品: %v", err)
	}

	// 此前发行的藏品封存后仍可转让：bob 把 i1（版本 2）转给 carol。
	if _, err := r.Transfer(TransferRequest{
		Operator: "bob", Reason: "封存后转让", RequestID: "rt-i1-carol", ItemID: "i1",
		ExpectedOwner: "bob", ExpectedVer: 2, ToID: "carol",
	}); err != nil {
		t.Fatalf("封存后已发行藏品应仍可转让: %v", err)
	}
	if h, _ := r.GetHolding("i1"); h.OwnerID != "carol" || h.Version != 3 {
		t.Fatalf("封存后转让结果异常: %+v", h)
	}

	// 落盘视角：关闭重开后 s1 已封存、s2 仍封存，持有与历史一致。
	dir := r.dir
	if err := r.Close(); err != nil {
		t.Fatal(err)
	}
	r2, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = r2.Close() })
	if s, _ := r2.GetSeries("s1"); !s.Sealed || s.CreatorID != "alice" || s.Metadata != "系列-s1" {
		t.Fatalf("重开后 s1 应保持已封存且编号、创建账户与元数据不变: %+v", s)
	}
	if s2, _ := r2.GetSeries("s2"); !s2.Sealed {
		t.Fatalf("重开后 s2 应仍封存: %+v", s2)
	}
	if h, _ := r2.GetHolding("i1"); h.OwnerID != "carol" || h.Version != 3 {
		t.Fatalf("重开后 i1 持有异常: %+v", h)
	}
	if h, _ := r2.GetHolding("i2"); h.OwnerID != "alice" || h.Version != 1 {
		t.Fatalf("重开后 i2 持有异常: %+v", h)
	}
	if hist, err := r2.History("i1"); err != nil || len(hist) != 3 {
		t.Fatalf("重开后 i1 历史异常: %+v, err %v", hist, err)
	}
}

// TestSealSaveFailureReadableSameRegistry 覆盖：写入失败但原数据仍可正常
// 读取（commit 据磁盘内容重建状态）时，得到相同的保障——同一登记册上封存
// 从未发生；恢复后重新封存须完整保存一次才生效。
func TestSealSaveFailureReadableSameRegistry(t *testing.T) {
	r := mustCreate(t, tempDir(t))
	seedSealWorld(t, r)

	// 只阻断写入，不破坏数据文件：rename 之前失败，原快照完好可读。
	blockBatchSave(t, r)
	err := r.SealSeries("s1", "alice")
	assertSaveFailureError(t, err)
	if s, _ := r.GetSeries("s1"); s.Sealed {
		t.Fatalf("可读原数据的保存失败后 s1 应仍未封存: %+v", s)
	}
	if s2, _ := r.GetSeries("s2"); !s2.Sealed {
		t.Fatalf("s2 应保持已封存: %+v", s2)
	}

	restoreBatchSave(t, r)
	if err := r.SealSeries("s1", "alice"); err != nil {
		t.Fatalf("恢复后重新封存应成功保存: %v", err)
	}
	if s, _ := r.GetSeries("s1"); !s.Sealed {
		t.Fatalf("重新封存保存成功后 s1 应已封存: %+v", s)
	}
	// 已封存系列再次封存仍直接成功，且不再写入（否则会被失败注入拦住）。
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
	if s, _ := r2.GetSeries("s1"); s.Sealed {
		t.Fatalf("重开后 s1 应仍未封存: %+v", s)
	}
	if s2, _ := r2.GetSeries("s2"); !s2.Sealed {
		t.Fatalf("重开后 s2 应仍封存: %+v", s2)
	}

	restoreBatchSave(t, r2)
	if err := r2.SealSeries("s1", "alice"); err != nil {
		t.Fatalf("重开后重新封存应成功: %v", err)
	}
	if s, _ := r2.GetSeries("s1"); !s.Sealed {
		t.Fatalf("重开后封存保存成功才应已封存: %+v", s)
	}
	// 封存后发行按现有规则被拒绝。
	if _, err := r2.Issue(IssueRequest{
		Operator: "alice", Reason: "封存后发行", RequestID: "ri-blocked",
		ItemID: "i3", SeriesID: "s1", BatchNo: "b1", HolderID: "bob",
	}); !errors.Is(err, ErrSeriesSealed) {
		t.Fatalf("封存后发行应被 ErrSeriesSealed 拒绝: %v", err)
	}
	// 已发行藏品仍可转让。
	if _, err := r2.Transfer(TransferRequest{
		Operator: "bob", Reason: "封存后转让", RequestID: "rt-i1-carol", ItemID: "i1",
		ExpectedOwner: "bob", ExpectedVer: 2, ToID: "carol",
	}); err != nil {
		t.Fatalf("封存后已发行藏品应仍可转让: %v", err)
	}
}

// TestSealUnknownForbiddenAndIdempotent 保留现有封存入口与错误判定：系列
// 不存在返回可由 errors.Is 判断的 ErrNotFound（即使当前正无法写入，也不
// 变成保存错误）；非创建账户返回 ErrForbidden，失败不改动系列状态；已成功
// 封存的系列创建账户再次封存直接成功、保持封存，不因当前数据位置暂时无法
// 写入而报保存错误。
func TestSealUnknownForbiddenAndIdempotent(t *testing.T) {
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
