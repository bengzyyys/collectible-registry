package registry

import (
	"errors"
	"os"
	"testing"
)

// 本文件为单件发行补充"成功发行本身保存失败"场景的回归保障。既有的
// issue_save_failure_test.go 覆盖的是状态类业务拒绝的落盘失败；这里覆盖
// 的是参数合法、操作者/系列/初始持有人/编号全部满足发行条件，本次发行的
// 藏品登记、初始持有、发行历史、历史序号与请求结果尚未原子替换原数据就
// 发生写入失败的情形——这样的发行实际没有保存，绝不能成为登记册认可的
// 发行，即使原数据在失败后暂时无法读取或解析。
//
// 失败返回必须保留实际写入错误并给出空结果（不携带藏品编号、初始持有人、
// 版本或历史序号，业务错误为空，不标回放）。同一个仍打开的登记册上：
// GetItem、GetHolding 与 History 对该编号都返回 ErrNotFound，初始持有人的
// 持有列表不增加这一件，也不能把它当成已发行藏品继续转让；藏品编号、操作
// 者请求号与发行/转让共用的历史序号都不被消耗；失败前已有的账户、系列、
// 其他藏品及其持有和历史原样保留；此前没有成功发行过藏品的系列仍可按原
// 条件设置版税规则。读写恢复后，即使用户先做一次无关操作成功保存，也不能
// 把这件未保存的藏品、发行历史或请求结果一并写入；用原请求重提按当时状态
// 重新判断，条件仍满足时才真正发行（版本 1、历史序号紧接此前已有历史、
// 不标回放），系列已封存则按既有规则拒绝；只有此次成功保存后再提才回放。
//
// 失败注入与其他场景共用：在 .registry.json.tmp 上预建目录，save 在
// OpenFile 阶段即以 EISDIR 失败（rename 之前）；部分用例同时把数据文件改写
// 为无法解析的内容，覆盖"原数据暂时无法读取、commit 无法按磁盘重建状态"。

// assertSingleIssueSuccessEmpty 核对单件发行"成功发行保存失败"的返回：
// error 是保存错误而非业务拒绝；结果整体为空。
func assertSingleIssueSuccessEmpty(t *testing.T, res IssueResult, err error) {
	t.Helper()
	assertSaveFailureError(t, err)
	if res.ItemID != "" || res.OwnerID != "" || res.Version != 0 || res.TxSeq != 0 ||
		res.Replayed || res.Err != nil {
		t.Fatalf("发行保存失败必须整体失败，结果应为空: %+v", res)
	}
}

// seedIssueSuccessWorld 建立失败发生前的世界：alice、bob 两个可用账户，
// alice 的系列 s1，且已有一件成功发行给 bob 的 i1（历史序号 1）。因此
// 本文件多数用例失败发生前最后一条历史序号是 1；未保存发行若执行将占用
// 序号 2，bob 的持有清单在失败前后都应只有 i1 一件。
func seedIssueSuccessWorld(t *testing.T, r *Registry) IssueRequest {
	t.Helper()
	setupWorld(t, r)
	if _, err := r.Issue(issueReq("i1", "bob")); err != nil {
		t.Fatal(err)
	}
	return issueReq("i2", "bob")
}

// assertIssueSuccessStillPending 在（成功发行保存失败后的）同一个已打开
// 登记册上核对：这件藏品如同从未发行，而失败前已有的内容完整保留。
func assertIssueSuccessStillPending(t *testing.T, r *Registry, itemID, rid string) {
	t.Helper()
	if _, err := r.GetItem(itemID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("保存失败后 %s 不应被登记: %v", itemID, err)
	}
	if _, err := r.GetHolding(itemID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("保存失败后 %s 不应有持有: %v", itemID, err)
	}
	if _, err := r.History(itemID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("保存失败后 %s 不应有历史: %v", itemID, err)
	}

	// 初始持有人 bob 的持有清单不增加这一件：仍只有失败前已发行的 i1。
	holdings, err := r.HoldingsOf("bob")
	if err != nil {
		t.Fatal(err)
	}
	if len(holdings) != 1 || holdings[0].ItemID != "i1" ||
		holdings[0].OwnerID != "bob" || holdings[0].Version != 1 {
		t.Fatalf("保存失败后 bob 的持有清单被改变: %+v", holdings)
	}

	// 藏品编号、操作者请求号与历史序号都不被消耗。
	if r.state.NextSeq != 1 {
		t.Fatalf("NextSeq = %d，未保存发行不应消耗历史序号，仍应为 1", r.state.NextSeq)
	}
	if _, ok := r.state.Requests[requestKey("alice", rid)]; ok {
		t.Fatalf("未保存发行不应占用请求号 %s", rid)
	}

	// 不能把它当成已发行藏品继续转让：藏品不存在按引用不存在拒绝，且该
	// 转让请求同样不占用请求号。
	if _, err := r.Transfer(TransferRequest{
		Operator: "bob", Reason: "转出未保存藏品", RequestID: "rt-" + rid, ItemID: itemID,
		ExpectedOwner: "bob", ExpectedVer: 1, ToID: "alice",
	}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("未保存藏品不能转让，应返回 ErrNotFound: %v", err)
	}
	if _, ok := r.state.Requests[requestKey("bob", "rt-"+rid)]; ok {
		t.Fatal("未保存藏品的转让拒绝不应占用请求号")
	}

	// 失败前已有的藏品、持有与历史原样保留。
	h, err := r.GetHolding("i1")
	if err != nil || h.OwnerID != "bob" || h.Version != 1 {
		t.Fatalf("失败前已有的 i1 持有被破坏: %+v, err %v", h, err)
	}
	hist, err := r.History("i1")
	if err != nil || len(hist) != 1 || hist[0].Seq != 1 || hist[0].Kind != "issue" {
		t.Fatalf("失败前已有的 i1 历史被破坏: %+v, err %v", hist, err)
	}
}

// TestIssueSuccessSaveFailureUnreadableSameRegistry 覆盖核心场景：发行条件
// 全部满足后的成功发行在写入阶段失败，且原数据同时被改写为无法解析（commit
// 无法按磁盘重建状态）。调用返回保存错误与空结果；同一登记册上发行如同从未
// 发生，旧记录完整。保存仍失败时重提依旧失败、不留痕。读写恢复后先做一次
// 无关操作（登记新账户）成功保存，也不把未保存发行带入；随后原请求重提按
// 当前状态完整发行一次（非回放、版本 1、序号 2、历史含本次操作者、原因与
// 请求号），再提才回放。
func TestIssueSuccessSaveFailureUnreadableSameRegistry(t *testing.T) {
	r := mustCreate(t, tempDir(t))
	req := seedIssueSuccessWorld(t, r)

	orig, err := os.ReadFile(dataFile(r.dir))
	if err != nil {
		t.Fatal(err)
	}
	blockBatchSave(t, r)
	if err := os.WriteFile(dataFile(r.dir), []byte("{corrupt"), fileMode); err != nil {
		t.Fatal(err)
	}

	// 成功发行落盘失败：返回保存错误与空结果。
	res, err := r.Issue(req)
	assertSingleIssueSuccessEmpty(t, res, err)
	assertIssueSuccessStillPending(t, r, req.ItemID, req.RequestID)

	// 保存条件未恢复、磁盘仍不可读时再次提交同一请求：仍失败在保存上，
	// 不能把上次未保存的成功当成已保存结果回放。
	res, err = r.Issue(req)
	assertSingleIssueSuccessEmpty(t, res, err)
	assertIssueSuccessStillPending(t, r, req.ItemID, req.RequestID)

	// 恢复正常读写；先让一次无关操作成功保存，不能夹带这件未保存的藏品。
	if err := os.WriteFile(dataFile(r.dir), orig, fileMode); err != nil {
		t.Fatal(err)
	}
	restoreBatchSave(t, r)
	if err := r.RegisterAccount("carol", "路人"); err != nil {
		t.Fatalf("无关操作应能成功保存: %v", err)
	}
	assertIssueSuccessStillPending(t, r, req.ItemID, req.RequestID)

	// 用原操作者、请求号、原因和全部参数重提：条件仍满足，真正发行一次，
	// 不标回放，初始持有版本为 1，历史序号紧接此前已有历史（2）。
	res, err = r.Issue(req)
	if err != nil {
		t.Fatalf("恢复后重提应真正发行一次: %v", err)
	}
	if res.Replayed || res.Err != nil || res.ItemID != "i2" || res.OwnerID != "bob" ||
		res.Version != 1 || res.TxSeq != 2 {
		t.Fatalf("重提结果异常: %+v", res)
	}
	it, err := r.GetItem("i2")
	if err != nil || it.SeriesID != "s1" || it.BatchNo != "b1" || it.IssuedTxID != 2 {
		t.Fatalf("重提后藏品登记异常: %+v, err %v", it, err)
	}
	h, _ := r.GetHolding("i2")
	if h.OwnerID != "bob" || h.Version != 1 {
		t.Fatalf("重提后 i2 持有 = %+v", h)
	}
	hist, _ := r.History("i2")
	if len(hist) != 1 || hist[0].Seq != 2 || hist[0].Kind != "issue" ||
		hist[0].Operator != req.Operator || hist[0].Reason != req.Reason ||
		hist[0].RequestID != req.RequestID || hist[0].ToID != "bob" ||
		hist[0].ToVersion != 1 || hist[0].FromID != "" || hist[0].FromVersion != 0 {
		t.Fatalf("重提后 i2 发行历史异常: %+v", hist)
	}
	if r.state.NextSeq != 2 {
		t.Fatalf("NextSeq = %d, want 2", r.state.NextSeq)
	}
	if holdings, _ := r.HoldingsOf("bob"); len(holdings) != 2 {
		t.Fatalf("发行成功后 bob 应持有两件: %+v", holdings)
	}

	// 只有此次成功保存后再提相同内容，才回放这一笔已保存结果，不第二次
	// 发行、不新增历史、不重复消耗序号。
	replay, err := r.Issue(req)
	if err != nil || !replay.Replayed || replay.OwnerID != "bob" ||
		replay.Version != 1 || replay.TxSeq != 2 {
		t.Fatalf("成功保存后再提应回放已保存结果: %+v, err %v", replay, err)
	}
	if hist2, _ := r.History("i2"); len(hist2) != 1 {
		t.Fatalf("回放不应新增历史: %+v", hist2)
	}
	if r.state.NextSeq != 2 {
		t.Fatalf("回放不应消耗历史序号: NextSeq=%d", r.state.NextSeq)
	}
}

// TestIssueSuccessSaveFailureReadableSameRegistry 覆盖：写入失败但原数据仍
// 可正常读取（commit 据磁盘内容重建状态）时，得到相同的失败结果与状态
// 保障——同一登记册上发行从未发生；恢复后原请求重提真正发行一次。
func TestIssueSuccessSaveFailureReadableSameRegistry(t *testing.T) {
	r := mustCreate(t, tempDir(t))
	req := seedIssueSuccessWorld(t, r)

	// 只阻断写入，不破坏数据文件：rename 之前失败，原快照完好可读。
	blockBatchSave(t, r)
	res, err := r.Issue(req)
	assertSingleIssueSuccessEmpty(t, res, err)
	assertIssueSuccessStillPending(t, r, req.ItemID, req.RequestID)

	restoreBatchSave(t, r)
	res, err = r.Issue(req)
	if err != nil || res.Replayed || res.ItemID != "i2" || res.OwnerID != "bob" ||
		res.Version != 1 || res.TxSeq != 2 {
		t.Fatalf("可读磁盘失败恢复后重提应真正发行一次: %+v, err %v", res, err)
	}
	replay, err := r.Issue(req)
	if err != nil || !replay.Replayed || replay.TxSeq != 2 {
		t.Fatalf("成功后再提应回放: %+v, err %v", replay, err)
	}
}

// TestIssueSuccessSaveFailureAfterReopen 覆盖磁盘视角：成功发行保存失败
// （原数据不可读）后关闭重开，看到的仍是发行前状态；恢复保存后用原请求
// 重提真正发行一次，而不是回放一个从未保存的成功；此前先完成的一次无关
// 操作同样不能把未保存内容带入。
func TestIssueSuccessSaveFailureAfterReopen(t *testing.T) {
	dir := tempDir(t)
	r := mustCreate(t, dir)
	req := seedIssueSuccessWorld(t, r)

	orig, err := os.ReadFile(dataFile(r.dir))
	if err != nil {
		t.Fatal(err)
	}
	blockBatchSave(t, r)
	if err := os.WriteFile(dataFile(r.dir), []byte("{corrupt"), fileMode); err != nil {
		t.Fatal(err)
	}
	res, err := r.Issue(req)
	assertSingleIssueSuccessEmpty(t, res, err)

	// 恢复数据文件为失败前内容后关闭重开：未保存的发行不在磁盘上。
	if err := os.WriteFile(dataFile(r.dir), orig, fileMode); err != nil {
		t.Fatal(err)
	}
	if err := r.Close(); err != nil {
		t.Fatal(err)
	}
	r2, err := Open(dir)
	if err != nil {
		t.Fatalf("失败后原登记册必须仍能正常读取: %v", err)
	}
	t.Cleanup(func() { _ = r2.Close() })
	if _, err := r2.GetItem("i2"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("重开后 i2 不应存在: %v", err)
	}
	if _, ok := r2.state.Requests[requestKey(req.Operator, req.RequestID)]; ok {
		t.Fatal("重开后未保存发行不应占用请求号")
	}
	if r2.state.NextSeq != 1 {
		t.Fatalf("重开后 NextSeq = %d，仍应为 1", r2.state.NextSeq)
	}

	// 先完成一次无关操作并成功保存，再重提原请求：未保存内容不被夹带，
	// 本次按重开后的状态真正发行一次。
	restoreBatchSave(t, r2)
	if err := r2.RegisterAccount("carol", "路人"); err != nil {
		t.Fatalf("无关操作应能成功保存: %v", err)
	}
	if _, err := r2.GetItem("i2"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("无关操作保存后 i2 仍不应存在: %v", err)
	}
	res2, err := r2.Issue(req)
	if err != nil || res2.Replayed || res2.TxSeq != 2 || res2.Version != 1 ||
		res2.OwnerID != "bob" {
		t.Fatalf("重开后重提应真正发行一次: %+v, err %v", res2, err)
	}
	replay, err := r2.Issue(req)
	if err != nil || !replay.Replayed || replay.TxSeq != 2 {
		t.Fatalf("成功后再提应回放: %+v, err %v", replay, err)
	}
}

// TestIssueSuccessSaveFailureRetrySealed 覆盖：恢复期间系列被封存后，原
// 请求必须按重提时的状态以 ErrSeriesSealed 拒绝（这次是新保存的拒绝，不
// 标回放），不能回放那次未保存的成功；拒绝保存后再提才回放该拒绝。
func TestIssueSuccessSaveFailureRetrySealed(t *testing.T) {
	r := mustCreate(t, tempDir(t))
	setupWorld(t, r)
	// 该系列此前没有成功发行过藏品：未保存发行将占用序号 1。
	req := issueReq("i1", "bob")

	orig, err := os.ReadFile(dataFile(r.dir))
	if err != nil {
		t.Fatal(err)
	}
	blockBatchSave(t, r)
	if err := os.WriteFile(dataFile(r.dir), []byte("{corrupt"), fileMode); err != nil {
		t.Fatal(err)
	}
	res, err := r.Issue(req)
	assertSingleIssueSuccessEmpty(t, res, err)
	if _, err := r.GetItem("i1"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("保存失败后 i1 不应存在: %v", err)
	}

	// 恢复读写后先封存系列。
	if err := os.WriteFile(dataFile(r.dir), orig, fileMode); err != nil {
		t.Fatal(err)
	}
	restoreBatchSave(t, r)
	if err := r.SealSeries("s1", "alice"); err != nil {
		t.Fatal(err)
	}

	// 原请求按重提时状态被封存拒绝：新保存的拒绝，不标回放。
	res, err = r.Issue(req)
	if !errors.Is(err, ErrSeriesSealed) {
		t.Fatalf("系列封存后原请求应按 ErrSeriesSealed 拒绝: %v", err)
	}
	if res.Replayed || res.ItemID != "i1" || !errors.Is(res.Err, ErrSeriesSealed) ||
		res.OwnerID != "" || res.Version != 0 || res.TxSeq != 0 {
		t.Fatalf("封存拒绝结果异常: %+v", res)
	}
	if _, err := r.GetItem("i1"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("拒绝后 i1 仍不应存在: %v", err)
	}
	if r.state.NextSeq != 0 {
		t.Fatalf("拒绝不应消耗历史序号: NextSeq=%d", r.state.NextSeq)
	}

	// 拒绝已保存：此后相同内容再提回放该拒绝。
	res, err = r.Issue(req)
	if !errors.Is(err, ErrSeriesSealed) || !res.Replayed ||
		!errors.Is(res.Err, ErrSeriesSealed) {
		t.Fatalf("拒绝保存后再提应回放: %+v, err %v", res, err)
	}
}

// TestIssueSuccessSaveFailureRoyaltyNotFrozen 覆盖：系列此前没有成功发行
// 过藏品时，未保存的发行不固定版税规则——保存失败后（即使写入仍未恢复）
// 设置版税规则不会得到 ErrRoyaltyFrozen，读写恢复后规则可按原条件设置；
// 原请求重提真正发行成功后，规则才固定。
func TestIssueSuccessSaveFailureRoyaltyNotFrozen(t *testing.T) {
	r := mustCreate(t, tempDir(t))
	setupWorld(t, r)
	req := issueReq("i1", "bob")

	blockBatchSave(t, r)
	res, err := r.Issue(req)
	assertSingleIssueSuccessEmpty(t, res, err)

	// 写入仍未恢复时设置版税规则：资格检查通过（未固定），只失败在保存上，
	// 不能返回 ErrRoyaltyFrozen。
	set := SetRoyaltyRequest{
		Operator: "alice", Reason: "失败后设置版税", RequestID: "rr-pending",
		SeriesID: "s1", Shares: []RoyaltyShare{{AccountID: "alice", Rate: 500}},
	}
	if _, err := r.SetRoyalty(set); errors.Is(err, ErrRoyaltyFrozen) {
		t.Fatalf("未成功发行的系列不应固定版税规则: %v", err)
	}

	// 读写恢复后规则仍可按原有条件设置成功。
	restoreBatchSave(t, r)
	set.RequestID = "rr-ok"
	if _, err := r.SetRoyalty(set); err != nil {
		t.Fatalf("未成功发行的系列应仍可设置版税规则: %v", err)
	}

	// 原请求重提真正发行成功（版税规则按既定快照，此处只核对发行）。
	res, err = r.Issue(req)
	if err != nil || res.Replayed || res.TxSeq != 1 || res.Version != 1 {
		t.Fatalf("重提应真正发行一次: %+v, err %v", res, err)
	}
	// 首次成功发行后规则固定：再次设置被拒绝。
	set.RequestID = "rr-frozen"
	if _, err := r.SetRoyalty(set); !errors.Is(err, ErrRoyaltyFrozen) {
		t.Fatalf("首次成功发行后规则应固定: %v", err)
	}
}
