package registry

import (
	"errors"
	"os"
	"testing"
)

// 本文件为单件发行补充"成功发行本身保存失败"场景的回归保障。既有的
// issue_save_failure_test.go 覆盖的是状态类业务拒绝的落盘失败；这里覆盖的
// 是账户、系列与编号检查全部通过、本次发行的藏品登记/初始持有/发行历史/
// 请求结果尚未原子替换原数据就发生写入失败的情形——这样的发行实际没有保存，
// 绝不能成为登记册认可的藏品，即使原数据在失败后暂时无法读取或解析。
//
// 失败返回必须保留实际写入错误并给出空结果（不携带藏品编号、初始持有人、
// 版本或历史序号，业务错误为空，不标回放）。同一个仍打开的登记册上：查询
// 该编号的藏品、持有和历史都返回 ErrNotFound，初始持有人的藏品列表不增加
// 这一件，也不能把它当成已发行藏品继续转让，藏品编号、请求号与历史序号都
// 不被消耗；失败前已有的账户、系列、其他藏品及其持有和历史原样保留。读写
// 恢复后，即使先做一次无关操作成功保存，也不能把这件未保存的藏品一并写入；
// 用原请求重提按当时的账户、系列和编号状态重新判断，条件仍满足时真正发行
// （版本 1、序号紧接已有历史、不标回放），只有此次成功保存后再提才回放；
// 系列此时已封存的按已有规则拒绝。
//
// 失败注入与其他场景共用：在 .registry.json.tmp 上预建目录，save 在
// OpenFile 阶段即以 EISDIR 失败（rename 之前）；部分用例同时把数据文件改写
// 为无法解析的内容，覆盖"原数据暂时无法读取、commit 无法按磁盘重建状态"。

// seedIssueSuccessWorld 建立一个已经存在成功发行的登记册：setupWorld 后
// alice 把 i0 发行给 bob（历史序号 1）。因此本文件各用例失败发生前最后一条
// 历史序号是 1；未保存的 i1 发行若执行将占用序号 2。
func seedIssueSuccessWorld(t *testing.T, r *Registry) {
	t.Helper()
	setupWorld(t, r)
	if _, err := r.Issue(issueReq("i0", "bob")); err != nil {
		t.Fatal(err)
	}
}

// assertIssueSuccessSaveFailureEmpty 核对成功发行保存失败的返回：error 是
// 保存错误；结果为空——没有藏品编号、持有人、版本、历史序号，不标回放，
// 业务错误为空。
func assertIssueSuccessSaveFailureEmpty(t *testing.T, res IssueResult, err error) {
	t.Helper()
	assertSaveFailureError(t, err)
	if res.ItemID != "" || res.OwnerID != "" || res.Version != 0 || res.TxSeq != 0 ||
		res.Replayed || res.Err != nil {
		t.Fatalf("成功发行保存失败必须整体失败，结果应为空: %+v", res)
	}
}

// assertIssueSuccessStillPending 在（成功发行保存失败后的）同一个已打开
// 登记册上核对：这次发行如同从未发生，而失败前已有的内容完整保留。
// item 为本次未保存发行的藏品编号，rid 为其请求号。
func assertIssueSuccessStillPending(t *testing.T, r *Registry, item, rid string) {
	t.Helper()
	// 藏品、持有与历史都不存在。
	if _, err := r.GetItem(item); !errors.Is(err, ErrNotFound) {
		t.Fatalf("失败后 %s 不应被登记: %v", item, err)
	}
	if _, err := r.GetHolding(item); !errors.Is(err, ErrNotFound) {
		t.Fatalf("失败后 %s 不应有持有: %v", item, err)
	}
	if _, err := r.History(item); !errors.Is(err, ErrNotFound) {
		t.Fatalf("失败后 %s 不应有历史: %v", item, err)
	}

	// 初始持有人的藏品列表不增加这一件：bob 仍只持有 i0。
	bobHoldings, err := r.HoldingsOf("bob")
	if err != nil {
		t.Fatal(err)
	}
	if len(bobHoldings) != 1 || bobHoldings[0].ItemID != "i0" ||
		bobHoldings[0].OwnerID != "bob" || bobHoldings[0].Version != 1 {
		t.Fatalf("失败后 bob 持有清单异常: %+v", bobHoldings)
	}

	// 不能把它当成已发行藏品继续转让：藏品不存在按 ErrNotFound 拒绝。
	if _, err := r.Transfer(xferReq("bob", item, "alice", 1, "rt-phantom-"+item)); !errors.Is(err, ErrNotFound) {
		t.Fatalf("未保存发行的 %s 不应可转让: %v", item, err)
	}

	// 历史序号不被消耗：下一条仍是 2（失败前最后一条为 1）；请求号空闲。
	if r.state.NextSeq != 1 {
		t.Fatalf("NextSeq = %d，未保存发行不应消耗历史序号，仍应为 1", r.state.NextSeq)
	}
	if _, ok := r.state.Requests[requestKey("alice", rid)]; ok {
		t.Fatalf("未保存发行不应占用请求号 %s", rid)
	}

	// 失败前已有的账户、系列、其他藏品及其持有和历史原样保留。
	if a, err := r.GetAccount("alice"); err != nil || !a.Active || a.Metadata != "创作者" {
		t.Fatalf("alice 账户受影响: %+v, err %v", a, err)
	}
	if s, err := r.GetSeries("s1"); err != nil || s.Sealed || s.CreatorID != "alice" {
		t.Fatalf("s1 系列受影响: %+v, err %v", s, err)
	}
	h0, err := r.GetHolding("i0")
	if err != nil || h0.OwnerID != "bob" || h0.Version != 1 {
		t.Fatalf("i0 持有受影响: %+v, err %v", h0, err)
	}
	if hist0, _ := r.History("i0"); len(hist0) != 1 || hist0[0].Seq != 1 ||
		hist0[0].Kind != "issue" || hist0[0].ToID != "bob" {
		t.Fatalf("i0 历史受影响: %+v", hist0)
	}
}

// TestIssueSuccessSaveFailureUnreadableSameRegistry 覆盖核心场景：检查全部
// 通过后的成功发行在写入阶段失败，且原数据同时被改写为无法解析（commit
// 无法按磁盘重建状态）。调用返回保存错误与空结果；同一登记册上发行如同
// 从未发生，旧记录完整。保存仍失败时重提依旧失败、不留痕。读写恢复后先
// 做一次无关操作（登记新账户）成功保存，也不把未保存的藏品带入；随后原
// 请求重提按当前状态真正发行一次（非回放、版本 1、序号 2），再提才回放，
// 改变业务参数按请求号冲突拒绝。
func TestIssueSuccessSaveFailureUnreadableSameRegistry(t *testing.T) {
	r := mustCreate(t, tempDir(t))
	seedIssueSuccessWorld(t, r)
	req := issueReq("i1", "bob")

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
	assertIssueSuccessSaveFailureEmpty(t, res, err)
	assertIssueSuccessStillPending(t, r, "i1", req.RequestID)

	// 保存条件未恢复、磁盘仍不可读时再次提交同一请求：仍失败在保存上，
	// 不能把上次未保存的成功当成已保存结果回放。
	res, err = r.Issue(req)
	assertIssueSuccessSaveFailureEmpty(t, res, err)
	assertIssueSuccessStillPending(t, r, "i1", req.RequestID)

	// 恢复正常读写；先让一次无关操作成功保存，不能夹带这件未保存的藏品。
	if err := os.WriteFile(dataFile(r.dir), orig, fileMode); err != nil {
		t.Fatal(err)
	}
	restoreBatchSave(t, r)
	if err := r.RegisterAccount("carol", "路人"); err != nil {
		t.Fatalf("无关操作应能成功保存: %v", err)
	}
	assertIssueSuccessStillPending(t, r, "i1", req.RequestID)

	// 用原操作者、请求号、原因和全部参数重提：条件仍满足，真正发行一次，
	// 不标回放，初始持有版本为 1，序号紧接已有历史（2）。
	res, err = r.Issue(req)
	if err != nil {
		t.Fatalf("恢复后重提应真正发行: %v", err)
	}
	if res.Replayed || res.Err != nil || res.ItemID != "i1" ||
		res.OwnerID != "bob" || res.Version != 1 || res.TxSeq != 2 {
		t.Fatalf("重提结果异常: %+v", res)
	}
	h, _ := r.GetHolding("i1")
	if h.OwnerID != "bob" || h.Version != 1 {
		t.Fatalf("重提成功后 i1 持有 = %+v", h)
	}
	hist, _ := r.History("i1")
	if len(hist) != 1 || hist[0].Seq != 2 || hist[0].Kind != "issue" ||
		hist[0].Operator != "alice" || hist[0].Reason != req.Reason ||
		hist[0].RequestID != req.RequestID || hist[0].FromID != "" ||
		hist[0].ToID != "bob" || hist[0].FromVersion != 0 || hist[0].ToVersion != 1 {
		t.Fatalf("重提成功后 i1 历史异常: %+v", hist)
	}
	if r.state.NextSeq != 2 {
		t.Fatalf("NextSeq = %d, want 2", r.state.NextSeq)
	}

	// 只有此次成功保存后再提相同内容，才回放首次成功结果，不第二次发行。
	replay, err := r.Issue(req)
	if err != nil || !replay.Replayed || replay.OwnerID != "bob" ||
		replay.Version != 1 || replay.TxSeq != 2 {
		t.Fatalf("成功保存后再提应回放已保存结果: %+v, err %v", replay, err)
	}
	if hist, _ := r.History("i1"); len(hist) != 1 {
		t.Fatalf("回放不应新增历史: %+v", hist)
	}

	// 同一请求号改变业务参数按请求号冲突拒绝。
	changed := req
	changed.Metadata = "别的元数据"
	if _, err := r.Issue(changed); !errors.Is(err, ErrRequestConflict) {
		t.Fatalf("改变业务参数应返回请求号冲突: %v", err)
	}
}

// TestIssueSuccessSaveFailureReadableSameRegistry 覆盖：写入失败但原数据
// 仍可正常读取（commit 据磁盘内容重建状态）时，得到相同的失败结果与状态
// 保障——同一登记册上发行从未发生；恢复后原请求重提真正发行一次。
func TestIssueSuccessSaveFailureReadableSameRegistry(t *testing.T) {
	r := mustCreate(t, tempDir(t))
	seedIssueSuccessWorld(t, r)
	req := issueReq("i1", "bob")

	// 只阻断写入，不破坏数据文件：rename 之前失败，原快照完好可读。
	blockBatchSave(t, r)
	res, err := r.Issue(req)
	assertIssueSuccessSaveFailureEmpty(t, res, err)
	assertIssueSuccessStillPending(t, r, "i1", req.RequestID)

	restoreBatchSave(t, r)
	res, err = r.Issue(req)
	if err != nil || res.Replayed || res.ItemID != "i1" ||
		res.OwnerID != "bob" || res.Version != 1 || res.TxSeq != 2 {
		t.Fatalf("可读磁盘失败恢复后重提应真正发行一次: %+v, err %v", res, err)
	}
	// 成功保存后再提才回放。
	replay, err := r.Issue(req)
	if err != nil || !replay.Replayed || replay.TxSeq != 2 {
		t.Fatalf("成功后再提应回放: %+v, err %v", replay, err)
	}
}

// TestIssueSuccessSaveFailureRetryAfterReopen 覆盖磁盘视角：成功发行保存
// 失败（原数据可读）后关闭重开，看到的仍是发行前状态；恢复保存后用原请求
// 重提真正发行一次，而不是回放一个从未保存的成功。
func TestIssueSuccessSaveFailureRetryAfterReopen(t *testing.T) {
	dir := tempDir(t)
	r := mustCreate(t, dir)
	t.Cleanup(func() { _ = r.Close() })
	seedIssueSuccessWorld(t, r)
	req := issueReq("i1", "bob")

	blockBatchSave(t, r)
	res, err := r.Issue(req)
	assertIssueSuccessSaveFailureEmpty(t, res, err)

	// 数据文件从未被替换：正常关闭并重新打开仍读到发行前状态。
	if err := r.Close(); err != nil {
		t.Fatal(err)
	}
	r2, err := Open(dir)
	if err != nil {
		t.Fatalf("失败后原登记册必须仍能正常读取: %v", err)
	}
	t.Cleanup(func() { _ = r2.Close() })
	assertIssueSuccessStillPending(t, r2, "i1", req.RequestID)

	restoreBatchSave(t, r2)
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
// 请求必须按已有封存规则拒绝，不能回放先前未保存的成功；该拒绝保存成功
// 后，再次提交才回放它。
func TestIssueSuccessSaveFailureRetrySealed(t *testing.T) {
	r := mustCreate(t, tempDir(t))
	seedIssueSuccessWorld(t, r)
	req := issueReq("i1", "bob")

	blockBatchSave(t, r)
	res, err := r.Issue(req)
	assertIssueSuccessSaveFailureEmpty(t, res, err)
	assertIssueSuccessStillPending(t, r, "i1", req.RequestID)

	// 恢复保存条件，创建账户封存系列。
	restoreBatchSave(t, r)
	if err := r.SealSeries("s1", "alice"); err != nil {
		t.Fatal(err)
	}

	// 原请求按当前状态应被封存拒绝，不是回放那次未保存的成功。
	res, err = r.Issue(req)
	if !errors.Is(err, ErrSeriesSealed) {
		t.Fatalf("系列封存后原请求应按封存拒绝: %v", err)
	}
	if res.Replayed || res.ItemID != "i1" || !errors.Is(res.Err, ErrSeriesSealed) ||
		res.OwnerID != "" || res.Version != 0 || res.TxSeq != 0 {
		t.Fatalf("封存拒绝的结果异常: %+v", res)
	}
	if _, err := r.GetItem("i1"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("封存拒绝不应登记藏品: %v", err)
	}
	if r.state.NextSeq != 1 {
		t.Fatalf("封存拒绝不应消耗历史序号: NextSeq=%d", r.state.NextSeq)
	}

	// 此后原样重提回放这条已保存的封存拒绝。
	res, err = r.Issue(req)
	if !errors.Is(err, ErrSeriesSealed) || !res.Replayed || !errors.Is(res.Err, ErrSeriesSealed) {
		t.Fatalf("封存拒绝保存后再提应回放该拒绝: %+v, err %v", res, err)
	}
}

// TestIssueSuccessSaveFailureRoyaltyNotFrozen 覆盖：未保存的成功发行不算
// 成功发行——该系列此前没有成功发行过藏品时，版税规则仍可按原有条件设置；
// 设置后原请求重提正常发行一次。
func TestIssueSuccessSaveFailureRoyaltyNotFrozen(t *testing.T) {
	r := mustCreate(t, tempDir(t))
	setupWorld(t, r)
	if err := r.RegisterAccount("carol", ""); err != nil {
		t.Fatal(err)
	}
	if err := r.CreateSeries("s2", "alice", "第二系列"); err != nil {
		t.Fatal(err)
	}
	req := issueReq("i1", "bob")
	req.SeriesID = "s2"

	blockBatchSave(t, r)
	res, err := r.Issue(req)
	assertIssueSuccessSaveFailureEmpty(t, res, err)
	if _, err := r.GetItem("i1"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("失败后 i1 不应被登记: %v", err)
	}

	// s2 从未成功发行：那次未保存的发行不固定版税规则，仍可设置。
	restoreBatchSave(t, r)
	set := SetRoyaltyRequest{
		Operator: "alice", Reason: "发行前设置版税", RequestID: "rr-s2",
		SeriesID: "s2", Shares: []RoyaltyShare{{AccountID: "carol", Rate: 500}},
	}
	if _, err := r.SetRoyalty(set); err != nil {
		t.Fatalf("未成功发行的系列不应固定版税规则: %v", err)
	}

	// 原请求重提正常发行一次（序号 1），再提回放。
	res, err = r.Issue(req)
	if err != nil || res.Replayed || res.ItemID != "i1" ||
		res.OwnerID != "bob" || res.Version != 1 || res.TxSeq != 1 {
		t.Fatalf("重提应正常发行: %+v, err %v", res, err)
	}
	replay, err := r.Issue(req)
	if err != nil || !replay.Replayed || replay.TxSeq != 1 {
		t.Fatalf("成功后再提应回放: %+v, err %v", replay, err)
	}
}
