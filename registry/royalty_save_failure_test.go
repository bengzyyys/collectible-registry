package registry

import (
	"errors"
	"os"
	"testing"
)

// 本文件为版税规则设置（SetRoyalty）补充"保存失败"场景的自动化回归保障，
// 重点是保存失败后调用者直接使用原来打开的登记册就能看到原规则与原历史，
// 不必重开。系列尚未发行、尚未封存，创建账户与收款账户均已登记且可用时，
// 创建账户通过 SetRoyalty 提交新的收款账户和比例，或用空规则清除版税。
// 本次业务条件允许设置，但新内容尚未替换原登记册数据就遇到写入失败：
//
//   - 返回的应是本次实际保存错误，结果中的系列编号、份额和业务错误均为空，
//     不能标为回放，也不能显示已设置成功；失败后重新读取原数据时的错误
//     （如 ErrCorrupt）不能替代保存错误；
//   - 失败后在同一个仍打开的登记册上查询当前规则与变更历史，应与提交前
//     完全相同：已有规则不被换成新方案，清空失败不抹掉旧规则，此前未设置
//     规则的系列仍保持无版税；历史不多出本次操作，已有记录的操作者、原因、
//     请求号及前后份额保持原样，下一次真正成功的变更紧接此前的变更序号；
//   - 这些结果既要在原数据仍可读取的普通写入失败下成立，也要在原数据暂时
//     无法读取或解析、无法重新载入时成立；
//   - 失败请求不占用操作者的请求号：恢复保存条件后完全相同的请求按当时的
//     系列状态重新处理，符合设置条件时正常成功而非旧结果回放，只留下这次
//     真正保存的变更；随后再次提交相同内容才回放成功结果且不增加记录；
//   - 此前已成功设置的其他系列规则和历史应保留；随后另一次正常操作成功
//     保存时也不能夹带失败请求的规则或历史。首次发行后规则固定、封存后
//     不能设置、空规则表示无版税的现有约定在重提时同样沿用。
//
// 失败注入与其他场景共用：在 .registry.json.tmp 上预建目录，save 在
// OpenFile 阶段即以 EISDIR 失败（rename 之前）；部分用例同时把数据文件
// 改写为无法解析的内容，覆盖"原数据暂时无法读取或解析、commit 无法按
// 磁盘重建状态"。

// assertRoyaltySaveFailureEmpty 核对规则设置"保存失败"的返回：error 是
// 本次实际保存错误——不是成功，不是任何业务哨兵拒绝，也不是失败后重新
// 读取原数据得到的 ErrCorrupt；结果为空：系列编号与份额为空、业务错误
// 为空，且不标记为回放。
func assertRoyaltySaveFailureEmpty(t *testing.T, res SetRoyaltyResult, err error) {
	t.Helper()
	assertSaveFailureError(t, err)
	if errors.Is(err, ErrCorrupt) {
		t.Fatalf("返回的应是本次写入错误，不能用重新读取原数据的 ErrCorrupt 替代: %v", err)
	}
	if res.SeriesID != "" || len(res.Shares) != 0 || res.Replayed || res.Err != nil {
		t.Fatalf("保存失败必须返回空结果（系列编号、份额、业务错误均为空，不标回放）: %+v", res)
	}
}

// assertRoyaltyRequestFree 核对失败请求没有占用请求号、没有消耗全局规则
// 变更序号（在同一个已打开的登记册上检查）。
func assertRoyaltyRequestFree(t *testing.T, r *Registry, req SetRoyaltyRequest, wantNextSeq int64) {
	t.Helper()
	if _, ok := r.state.Requests[requestKey(req.Operator, req.RequestID)]; ok {
		t.Fatalf("未保存的规则设置不应占用请求号 %s", req.RequestID)
	}
	if r.state.NextRoyaltySeq != wantNextSeq {
		t.Fatalf("NextRoyaltySeq = %d, want %d（失败请求不应消耗变更序号）",
			r.state.NextRoyaltySeq, wantNextSeq)
	}
}

// assertSeriesRule 核对某系列当前规则（规范化排序后的精确份额）；不传
// want 时表示该系列当前应无版税。
func assertSeriesRule(t *testing.T, r *Registry, seriesID string, want ...RoyaltyShare) {
	t.Helper()
	rule, err := r.GetSeriesRoyalty(seriesID)
	if err != nil {
		t.Fatalf("GetSeriesRoyalty %s: %v", seriesID, err)
	}
	wantSharesEqual(t, rule, want...)
}

// royaltyHistoryWant 描述一条期望的规则变更记录。
type royaltyHistoryWant struct {
	seq           int64
	operator      string
	reason        string
	rid           string
	before, after []RoyaltyShare
}

// assertRoyaltyHistoryEvents 核对某系列版税规则变更历史与期望逐条相同：
// 条数、序号、操作者、原因、请求号、发生时间与前后份额。
func assertRoyaltyHistoryEvents(t *testing.T, r *Registry, seriesID string, want []royaltyHistoryWant) {
	t.Helper()
	evs, err := r.RoyaltyHistory(seriesID)
	if err != nil {
		t.Fatalf("RoyaltyHistory %s: %v", seriesID, err)
	}
	if len(evs) != len(want) {
		t.Fatalf("系列 %s 变更历史条数 = %d，期望 %d（实际 %+v）",
			seriesID, len(evs), len(want), evs)
	}
	for i, w := range want {
		e := evs[i]
		if e.Seq != w.seq || e.SeriesID != seriesID || e.Operator != w.operator ||
			e.Reason != w.reason || e.RequestID != w.rid {
			t.Fatalf("系列 %s 第 %d 条记录元数据错误: got %+v, want seq=%d op=%s reason=%s rid=%s",
				seriesID, i+1, e, w.seq, w.operator, w.reason, w.rid)
		}
		if e.OccurredAt.IsZero() {
			t.Fatalf("系列 %s 第 %d 条记录缺少发生时间", seriesID, i+1)
		}
		wantSharesEqual(t, e.Before, w.before...)
		wantSharesEqual(t, e.After, w.after...)
	}
}

// corruptDisk 让下一次 commit 的写入失败，同时把数据文件改写为无法解析的
// 内容，使 commit 无法按磁盘重建状态；返回原数据文件内容供修复使用。
func corruptDisk(t *testing.T, r *Registry) []byte {
	t.Helper()
	orig, err := os.ReadFile(dataFile(r.dir))
	if err != nil {
		t.Fatal(err)
	}
	blockBatchSave(t, r)
	if err := os.WriteFile(dataFile(r.dir), []byte("{corrupt"), fileMode); err != nil {
		t.Fatal(err)
	}
	return orig
}

// repairDisk 恢复数据文件内容与保存条件（corruptDisk 的逆操作）。
func repairDisk(t *testing.T, r *Registry, orig []byte) {
	t.Helper()
	if err := os.WriteFile(dataFile(r.dir), orig, fileMode); err != nil {
		t.Fatal(err)
	}
	restoreBatchSave(t, r)
}

func assertSealed(t *testing.T, r *Registry, seriesID string, want bool) {
	t.Helper()
	s, err := r.GetSeries(seriesID)
	if err != nil {
		t.Fatalf("GetSeries %s: %v", seriesID, err)
	}
	if s.Sealed != want {
		t.Fatalf("系列 %s Sealed = %v, want %v", seriesID, s.Sealed, want)
	}
}

// 各用例共用的规则方案与请求原因。
var (
	ruleS1Old1 = []RoyaltyShare{{AccountID: "bob", Rate: 1000}, {AccountID: "carol", Rate: 3000}}
	ruleS1Old2 = []RoyaltyShare{{AccountID: "dave", Rate: 2000}}
	ruleS1New  = []RoyaltyShare{
		{AccountID: "carol", Rate: 4000},
		{AccountID: "erin", Rate: 500},
	}
	ruleS3 = []RoyaltyShare{{AccountID: "carol", Rate: 5000}}
)

// seedRoyaltySaveFailureWorld 建立版税保存失败用例的初始世界：
//
//   - alice（各系列创建账户）、bob、carol、dave、erin 五个已登记可用账户；
//   - s3 先成功设置 carol 50%（变更序号 1）后封存，用于核对失败回滚与
//     随后成功保存都不改变其他系列已保存的规则、历史与封存状态；
//   - s1 尚未发行、尚未封存，已有两条已保存变更（序号 2：nil→carol 30%
//     +bob 10%；序号 3：旧规则→dave 20%），当前规则为 dave 20%；
//   - s2 尚未发行、尚未封存且从未设置规则，当前无版税、无历史；
//   - 因此全局下一条规则变更序号为 4。
func seedRoyaltySaveFailureWorld(t *testing.T, r *Registry) {
	t.Helper()
	royaltyWorld(t, r) // alice/bob + s1，再补 carol、dave
	if err := r.RegisterAccount("erin", "路人"); err != nil {
		t.Fatal(err)
	}
	// s3：先设置规则（占用全局变更序号 1）后封存。
	if err := r.CreateSeries("s3", "alice", "系列三"); err != nil {
		t.Fatal(err)
	}
	if _, err := r.SetRoyalty(SetRoyaltyRequest{
		Operator: "alice", Reason: "设定版税", RequestID: "sr-s3",
		SeriesID: "s3", Shares: ruleS3,
	}); err != nil {
		t.Fatal(err)
	}
	if err := r.SealSeries("s3", "alice"); err != nil {
		t.Fatal(err)
	}
	// s1：两条已保存变更（全局序号 2、3），当前 dave 20%。
	mustSetRoyalty(t, r, "sr-s1-1", ruleS1Old1...)
	mustSetRoyalty(t, r, "sr-s1-2", ruleS1Old2...)
	// s2：从未设置规则的未发行系列。
	if err := r.CreateSeries("s2", "alice", "系列二"); err != nil {
		t.Fatal(err)
	}
}

// s1HistoryBaseline 是失败发生前 s1 已保存的两条变更。
func s1HistoryBaseline() []royaltyHistoryWant {
	return []royaltyHistoryWant{
		{seq: 2, operator: "alice", reason: "设定版税", rid: "sr-s1-1", before: nil, after: ruleS1Old1},
		{seq: 3, operator: "alice", reason: "设定版税", rid: "sr-s1-2", before: ruleS1Old1, after: ruleS1Old2},
	}
}

func s3HistoryBaseline() []royaltyHistoryWant {
	return []royaltyHistoryWant{
		{seq: 1, operator: "alice", reason: "设定版税", rid: "sr-s3", before: nil, after: ruleS3},
	}
}

// TestSetRoyaltySaveFailureReadable 覆盖"原数据仍可读取的普通写入失败"：
// 失败发生在 rename 之前，原快照完好可读。提交新收款账户与比例、用空规则
// 清空、以及为从未设置规则的系列设规则，三类请求都必须返回保存错误与空
// 结果，不标回放，不占用请求号与变更序号；同一个仍打开的登记册上当前
// 规则与历史与提交前完全相同。保存条件未恢复时再次提交依旧报保存错误，
// 不能因内存里曾出现幻影规则而成功或回放。恢复后一次无关操作成功保存也
// 不夹带任何未保存的内容。
func TestSetRoyaltySaveFailureReadable(t *testing.T) {
	r := mustCreate(t, tempDir(t))
	seedRoyaltySaveFailureWorld(t, r)

	// 场景一：已有规则的系列提交"新收款账户+比例"。
	req := SetRoyaltyRequest{
		Operator: "alice", Reason: "改给 carol 和 erin", RequestID: "sr-fail-1",
		SeriesID: "s1", Shares: ruleS1New,
	}
	blockBatchSave(t, r)
	res, err := r.SetRoyalty(req)
	assertRoyaltySaveFailureEmpty(t, res, err)
	assertRoyaltyRequestFree(t, r, req, 3)
	assertSeriesRule(t, r, "s1", ruleS1Old2...)
	assertRoyaltyHistoryEvents(t, r, "s1", s1HistoryBaseline())

	// 保存条件未恢复时再次提交：仍须真实尝试保存并失败，不能回放幻影。
	res, err = r.SetRoyalty(req)
	assertRoyaltySaveFailureEmpty(t, res, err)
	assertRoyaltyRequestFree(t, r, req, 3)
	assertSeriesRule(t, r, "s1", ruleS1Old2...)
	assertRoyaltyHistoryEvents(t, r, "s1", s1HistoryBaseline())
	restoreBatchSave(t, r)

	// 场景二：用空规则清空版税，保存失败时旧规则不能被抹掉。
	clearReq := SetRoyaltyRequest{
		Operator: "alice", Reason: "清空版税", RequestID: "sr-fail-clear",
		SeriesID: "s1",
	}
	blockBatchSave(t, r)
	cres, cerr := r.SetRoyalty(clearReq)
	assertRoyaltySaveFailureEmpty(t, cres, cerr)
	assertRoyaltyRequestFree(t, r, clearReq, 3)
	assertSeriesRule(t, r, "s1", ruleS1Old2...)
	assertRoyaltyHistoryEvents(t, r, "s1", s1HistoryBaseline())
	restoreBatchSave(t, r)

	// 场景三：此前从未设置规则的系列设置失败，仍保持无版税、无历史。
	s2Req := SetRoyaltyRequest{
		Operator: "alice", Reason: "给 s2 设规则", RequestID: "sr-fail-s2",
		SeriesID: "s2", Shares: []RoyaltyShare{{AccountID: "bob", Rate: 2500}},
	}
	blockBatchSave(t, r)
	s2res, s2err := r.SetRoyalty(s2Req)
	assertRoyaltySaveFailureEmpty(t, s2res, s2err)
	assertRoyaltyRequestFree(t, r, s2Req, 3)
	assertSeriesRule(t, r, "s2")
	assertRoyaltyHistoryEvents(t, r, "s2", nil)
	restoreBatchSave(t, r)

	// 恢复后一次无关操作成功保存，不夹带三笔失败请求的任何规则或历史。
	if err := r.RegisterAccount("frank", "路人二"); err != nil {
		t.Fatalf("无关操作应能成功保存: %v", err)
	}
	assertSeriesRule(t, r, "s1", ruleS1Old2...)
	assertRoyaltyHistoryEvents(t, r, "s1", s1HistoryBaseline())
	assertSeriesRule(t, r, "s2")
	assertRoyaltyHistoryEvents(t, r, "s2", nil)
	assertRoyaltyRequestFree(t, r, req, 3)
	assertRoyaltyRequestFree(t, r, clearReq, 3)
	assertRoyaltyRequestFree(t, r, s2Req, 3)

	// 其他系列已保存的规则、历史与封存状态完整保留。
	assertSeriesRule(t, r, "s3", ruleS3...)
	assertRoyaltyHistoryEvents(t, r, "s3", s3HistoryBaseline())
	assertSealed(t, r, "s3", true)
}

// TestSetRoyaltySaveFailureUnreadable 覆盖最严情形：写入失败且原数据同时被
// 改写为无法解析（commit 无法按磁盘重建状态）。返回的仍是本次写入错误而非
// ErrCorrupt；同一个仍打开的登记册上当前规则、历史、请求号、全局变更序号
// 全部维持提交前原样，其他系列同样不受影响。恢复后：无关成功保存不夹带
// 幻影；原请求按当时状态重新处理并真正成功一次（不回放，序号紧接为 4）；
// 再次提交才回放且不增加记录；首次发行固定的正是这次真正保存的新规则。
func TestSetRoyaltySaveFailureUnreadable(t *testing.T) {
	r := mustCreate(t, tempDir(t))
	seedRoyaltySaveFailureWorld(t, r)
	req := SetRoyaltyRequest{
		Operator: "alice", Reason: "改给 carol 和 erin", RequestID: "sr-fail-u",
		SeriesID: "s1", Shares: ruleS1New,
	}

	orig := corruptDisk(t, r)
	res, err := r.SetRoyalty(req)
	assertRoyaltySaveFailureEmpty(t, res, err)
	assertRoyaltyRequestFree(t, r, req, 3)
	assertSeriesRule(t, r, "s1", ruleS1Old2...)
	assertRoyaltyHistoryEvents(t, r, "s1", s1HistoryBaseline())
	// 其他系列在同一个仍打开的登记册中原样可读。
	assertSeriesRule(t, r, "s2")
	assertRoyaltyHistoryEvents(t, r, "s2", nil)
	assertSeriesRule(t, r, "s3", ruleS3...)
	assertRoyaltyHistoryEvents(t, r, "s3", s3HistoryBaseline())
	assertSealed(t, r, "s3", true)

	// 磁盘仍坏、保存仍失败：再次提交新规则与提交清空，都必须返回写入错误，
	// 旧规则与历史不变。
	res, err = r.SetRoyalty(req)
	assertRoyaltySaveFailureEmpty(t, res, err)
	assertRoyaltyRequestFree(t, r, req, 3)
	clearReq := SetRoyaltyRequest{
		Operator: "alice", Reason: "清空版税", RequestID: "sr-fail-u-clear",
		SeriesID: "s1",
	}
	cres, cerr := r.SetRoyalty(clearReq)
	assertRoyaltySaveFailureEmpty(t, cres, cerr)
	assertRoyaltyRequestFree(t, r, clearReq, 3)
	assertSeriesRule(t, r, "s1", ruleS1Old2...)
	assertRoyaltyHistoryEvents(t, r, "s1", s1HistoryBaseline())

	repairDisk(t, r, orig)

	// 另一次正常操作成功保存，不能夹带任何一笔失败请求的规则、历史或请求号。
	if err := r.RegisterAccount("frank", "路人二"); err != nil {
		t.Fatalf("无关操作应能成功保存: %v", err)
	}
	assertSeriesRule(t, r, "s1", ruleS1Old2...)
	assertRoyaltyHistoryEvents(t, r, "s1", s1HistoryBaseline())
	assertSeriesRule(t, r, "s2")
	assertRoyaltyRequestFree(t, r, req, 3)
	assertRoyaltyRequestFree(t, r, clearReq, 3)
	if r.state.NextRoyaltySeq != 3 {
		t.Fatalf("NextRoyaltySeq = %d, want 3", r.state.NextRoyaltySeq)
	}

	// 完全相同的新规则请求重提：系列仍未发行、未封存，按当时状态重新处理，
	// 正常成功而非回放；只新增这一条真正保存的变更，序号紧接此前的 3。
	res, err = r.SetRoyalty(req)
	if err != nil {
		t.Fatalf("恢复后原请求重提应正常成功: %v", err)
	}
	if res.Replayed || res.Err != nil || res.SeriesID != "s1" {
		t.Fatalf("重提首次成功不应标回放且应返回系列编号: %+v", res)
	}
	wantSharesEqual(t, res.Shares, ruleS1New...)
	assertSeriesRule(t, r, "s1", ruleS1New...)
	wantEvents := append(s1HistoryBaseline(), royaltyHistoryWant{
		seq: 4, operator: "alice", reason: "改给 carol 和 erin", rid: "sr-fail-u",
		before: ruleS1Old2, after: ruleS1New,
	})
	assertRoyaltyHistoryEvents(t, r, "s1", wantEvents)
	if r.state.NextRoyaltySeq != 4 {
		t.Fatalf("真正成功后 NextRoyaltySeq = %d, want 4", r.state.NextRoyaltySeq)
	}

	// 随后再次提交相同内容：回放成功结果，不再增加记录、不消耗序号。
	rep, err := r.SetRoyalty(req)
	if err != nil || !rep.Replayed || rep.Err != nil || rep.SeriesID != "s1" {
		t.Fatalf("再次提交应回放成功结果: %+v, err %v", rep, err)
	}
	wantSharesEqual(t, rep.Shares, ruleS1New...)
	assertRoyaltyHistoryEvents(t, r, "s1", wantEvents)
	if r.state.NextRoyaltySeq != 4 {
		t.Fatalf("回放不应消耗变更序号: %d", r.state.NextRoyaltySeq)
	}

	// 功能核对：首次发行固定的正是这次真正保存的新规则；转让按新规则
	// （carol 40%、erin 5%）计算应付，余款归转让前持有人。
	if _, err := r.Issue(issueReq("is1", "alice")); err != nil {
		t.Fatal(err)
	}
	xr := xferReq("alice", "is1", "bob", 1, "rt-s1-new-rule")
	xr.Price = 10000
	xres, err := r.Transfer(xr)
	if err != nil {
		t.Fatalf("新规则下转让应成功: %v", err)
	}
	pm := payableMap(t, xres.Payables)
	if len(pm) != 2 || pm["carol"].Amount != 4000 || pm["carol"].Rate != 4000 ||
		pm["erin"].Amount != 500 || pm["erin"].Rate != 500 || xres.Remainder != 5500 {
		t.Fatalf("转让应按真正保存的新规则计算应付: %+v", xres)
	}
}

// TestSetRoyaltySaveFailureClearUnreadable 专门覆盖"清空失败不能抹掉旧规则"
// 在磁盘不可读情形下的保障：恢复后无关成功保存不夹带幻影清空；原清空请求
// 真正执行一次（当前规则变为无版税，新增序号 4、前后份额为 旧规则→空 的
// 记录，首次不标回放）；再次提交回放且不增加记录。首次发行后固定为无版税。
func TestSetRoyaltySaveFailureClearUnreadable(t *testing.T) {
	r := mustCreate(t, tempDir(t))
	seedRoyaltySaveFailureWorld(t, r)
	req := SetRoyaltyRequest{
		Operator: "alice", Reason: "清空版税", RequestID: "sr-clear-u",
		SeriesID: "s1",
	}

	orig := corruptDisk(t, r)
	res, err := r.SetRoyalty(req)
	assertRoyaltySaveFailureEmpty(t, res, err)
	assertRoyaltyRequestFree(t, r, req, 3)
	assertSeriesRule(t, r, "s1", ruleS1Old2...)
	assertRoyaltyHistoryEvents(t, r, "s1", s1HistoryBaseline())

	repairDisk(t, r, orig)
	if err := r.RegisterAccount("frank", "路人二"); err != nil {
		t.Fatalf("无关操作应能成功保存: %v", err)
	}
	assertSeriesRule(t, r, "s1", ruleS1Old2...)
	assertRoyaltyHistoryEvents(t, r, "s1", s1HistoryBaseline())

	// 原清空请求真正执行一次：当前无版税，新增序号 4 的清空记录。
	res, err = r.SetRoyalty(req)
	if err != nil || res.Replayed || res.Err != nil || res.SeriesID != "s1" {
		t.Fatalf("清空请求重提首次成功不应标回放: %+v, err %v", res, err)
	}
	if len(res.Shares) != 0 {
		t.Fatalf("清空成功结果不应带份额: %+v", res.Shares)
	}
	assertSeriesRule(t, r, "s1")
	wantEvents := append(s1HistoryBaseline(), royaltyHistoryWait_clear())
	assertRoyaltyHistoryEvents(t, r, "s1", wantEvents)
	if r.state.NextRoyaltySeq != 4 {
		t.Fatalf("NextRoyaltySeq = %d, want 4", r.state.NextRoyaltySeq)
	}

	// 再次提交相同清空请求回放，不增加记录。
	rep, err := r.SetRoyalty(req)
	if err != nil || !rep.Replayed || rep.Err != nil || len(rep.Shares) != 0 {
		t.Fatalf("再次提交清空请求应回放空规则: %+v, err %v", rep, err)
	}
	assertSeriesRule(t, r, "s1")
	assertRoyaltyHistoryEvents(t, r, "s1", wantEvents)

	// 功能核对：此后首次发行固定为无版税，转让无应付、余款全归持有人。
	if _, err := r.Issue(issueReq("is1", "alice")); err != nil {
		t.Fatal(err)
	}
	xr := xferReq("alice", "is1", "bob", 1, "rt-s1-cleared")
	xr.Price = 10000
	xres, err := r.Transfer(xr)
	if err != nil {
		t.Fatalf("清空规则下转让应成功: %v", err)
	}
	if len(xres.Payables) != 0 || xres.Remainder != 10000 {
		t.Fatalf("无版税规则下应无应付、余款全归持有人: %+v", xres)
	}
}

func royaltyHistoryWait_clear() royaltyHistoryWant {
	return royaltyHistoryWant{
		seq: 4, operator: "alice", reason: "清空版税", rid: "sr-clear-u",
		before: ruleS1Old2, after: nil,
	}
}

// TestSetRoyaltySaveFailureNeverSetSeriesUnreadable 覆盖"此前未设置规则的
// 系列仍保持无版税"在磁盘不可读情形下的保障：s2 设置失败后无规则、无历史、
// 请求号不占用，s1 的已有规则与历史不被波及；恢复后原请求首次成功生成
// 序号 4 的变更（紧接全局序号），再次提交回放且不增加记录。
func TestSetRoyaltySaveFailureNeverSetSeriesUnreadable(t *testing.T) {
	r := mustCreate(t, tempDir(t))
	seedRoyaltySaveFailureWorld(t, r)
	s2Rule := []RoyaltyShare{{AccountID: "bob", Rate: 2500}}
	req := SetRoyaltyRequest{
		Operator: "alice", Reason: "给 s2 设规则", RequestID: "sr-s2-u",
		SeriesID: "s2", Shares: s2Rule,
	}

	orig := corruptDisk(t, r)
	res, err := r.SetRoyalty(req)
	assertRoyaltySaveFailureEmpty(t, res, err)
	assertRoyaltyRequestFree(t, r, req, 3)
	assertSeriesRule(t, r, "s2")
	assertRoyaltyHistoryEvents(t, r, "s2", nil)
	// s1 的已有规则与历史原样保留。
	assertSeriesRule(t, r, "s1", ruleS1Old2...)
	assertRoyaltyHistoryEvents(t, r, "s1", s1HistoryBaseline())

	repairDisk(t, r, orig)
	if err := r.RegisterAccount("frank", "路人二"); err != nil {
		t.Fatalf("无关操作应能成功保存: %v", err)
	}
	assertSeriesRule(t, r, "s2")
	assertRoyaltyHistoryEvents(t, r, "s2", nil)
	assertRoyaltyRequestFree(t, r, req, 3)

	// 原请求首次真正成功：s2 出现序号 4 的唯一变更，全局序号推进到 5。
	res, err = r.SetRoyalty(req)
	if err != nil || res.Replayed || res.Err != nil || res.SeriesID != "s2" {
		t.Fatalf("s2 原请求重提首次成功不应标回放: %+v, err %v", res, err)
	}
	assertSeriesRule(t, r, "s2", s2Rule...)
	assertRoyaltyHistoryEvents(t, r, "s2", []royaltyHistoryWant{
		{seq: 4, operator: "alice", reason: "给 s2 设规则", rid: "sr-s2-u",
			before: nil, after: s2Rule},
	})
	if r.state.NextRoyaltySeq != 4 {
		t.Fatalf("NextRoyaltySeq = %d, want 4", r.state.NextRoyaltySeq)
	}

	// 再次提交回放，不增加记录；s1 的历史自始至终未被波及。
	rep, err := r.SetRoyalty(req)
	if err != nil || !rep.Replayed || rep.Err != nil {
		t.Fatalf("s2 再次提交应回放: %+v, err %v", rep, err)
	}
	assertRoyaltyHistoryEvents(t, r, "s2", []royaltyHistoryWant{
		{seq: 4, operator: "alice", reason: "给 s2 设规则", rid: "sr-s2-u",
			before: nil, after: s2Rule},
	})
	assertRoyaltyHistoryEvents(t, r, "s1", s1HistoryBaseline())
}

// TestSetRoyaltySaveFailureRetryRefrozenByIssue 覆盖：保存失败不占用请求号，
// 恢复保存条件后完全相同的请求按当时的系列状态重新处理。这里恢复后先完成
// 首次发行（规则按旧规则固定），原请求重提应得到全新的 ErrRoyaltyFrozen
// 拒绝而非回放，规则保持旧规则、历史不增加；该拒绝保存后再次提交才回放。
func TestSetRoyaltySaveFailureRetryRefrozenByIssue(t *testing.T) {
	r := mustCreate(t, tempDir(t))
	seedRoyaltySaveFailureWorld(t, r)
	req := SetRoyaltyRequest{
		Operator: "alice", Reason: "改给 carol 和 erin", RequestID: "sr-fail-frozen",
		SeriesID: "s1", Shares: ruleS1New,
	}

	orig := corruptDisk(t, r)
	res, err := r.SetRoyalty(req)
	assertRoyaltySaveFailureEmpty(t, res, err)
	assertRoyaltyRequestFree(t, r, req, 3)

	repairDisk(t, r, orig)
	// 首次发行先完成：规则固定为失败前的旧规则 dave 20%。
	if _, err := r.Issue(issueReq("is1", "alice")); err != nil {
		t.Fatal(err)
	}
	assertSeriesRule(t, r, "s1", ruleS1Old2...)

	// 完全相同的请求重提：按当时状态重新判断，得到新保存的冻结拒绝，
	// 不标回放；旧规则与历史不变。
	res, err = r.SetRoyalty(req)
	if !errors.Is(err, ErrRoyaltyFrozen) {
		t.Fatalf("首次发行后重提应返回 ErrRoyaltyFrozen: %v", err)
	}
	if res.Replayed || res.SeriesID != "s1" || !errors.Is(res.Err, ErrRoyaltyFrozen) {
		t.Fatalf("冻结拒绝应为本次重新处理的结果而非回放: %+v", res)
	}
	assertSeriesRule(t, r, "s1", ruleS1Old2...)
	assertRoyaltyHistoryEvents(t, r, "s1", s1HistoryBaseline())
	if r.state.NextRoyaltySeq != 3 {
		t.Fatalf("业务拒绝不应消耗变更序号: %d", r.state.NextRoyaltySeq)
	}

	// 该拒绝已保存：再次提交相同请求回放首次冻结拒绝。
	res, err = r.SetRoyalty(req)
	if !errors.Is(err, ErrRoyaltyFrozen) || !res.Replayed ||
		!errors.Is(res.Err, ErrRoyaltyFrozen) {
		t.Fatalf("再次提交应回放已保存的冻结拒绝: %+v, err %v", res, err)
	}
	assertRoyaltyHistoryEvents(t, r, "s1", s1HistoryBaseline())
}

// TestSetRoyaltySaveFailureRetryAfterSeal 覆盖：恢复后系列已被封存的，原
// 请求重提按现状返回全新保存的 ErrSeriesSealed（不回放），规则与历史不变；
// 再次提交才回放该封存拒绝。
func TestSetRoyaltySaveFailureRetryAfterSeal(t *testing.T) {
	r := mustCreate(t, tempDir(t))
	seedRoyaltySaveFailureWorld(t, r)
	req := SetRoyaltyRequest{
		Operator: "alice", Reason: "改给 carol 和 erin", RequestID: "sr-fail-seal",
		SeriesID: "s1", Shares: ruleS1New,
	}

	orig := corruptDisk(t, r)
	res, err := r.SetRoyalty(req)
	assertRoyaltySaveFailureEmpty(t, res, err)
	assertRoyaltyRequestFree(t, r, req, 3)

	repairDisk(t, r, orig)
	if err := r.SealSeries("s1", "alice"); err != nil {
		t.Fatal(err)
	}
	res, err = r.SetRoyalty(req)
	if !errors.Is(err, ErrSeriesSealed) {
		t.Fatalf("封存后重提应返回 ErrSeriesSealed: %v", err)
	}
	if res.Replayed || res.SeriesID != "s1" || !errors.Is(res.Err, ErrSeriesSealed) {
		t.Fatalf("封存拒绝应为本次重新处理的结果而非回放: %+v", res)
	}
	assertSeriesRule(t, r, "s1", ruleS1Old2...)
	assertRoyaltyHistoryEvents(t, r, "s1", s1HistoryBaseline())

	res, err = r.SetRoyalty(req)
	if !errors.Is(err, ErrSeriesSealed) || !res.Replayed ||
		!errors.Is(res.Err, ErrSeriesSealed) {
		t.Fatalf("再次提交应回放已保存的封存拒绝: %+v, err %v", res, err)
	}
	assertRoyaltyHistoryEvents(t, r, "s1", s1HistoryBaseline())
}

// TestSetRoyaltyStatusRejectionSaveFailure 覆盖状态类业务拒绝自身的落盘
// 失败：对已封存系列设置（ErrSeriesSealed）与非创建账户设置
// （ErrForbidden）本应保存一条状态类拒绝，但拒绝结果尚未写入就遇到保存
// 错误时，返回实际保存错误、空结果、不占用请求号、不留历史；恢复后原请求
// 重新保存对应业务拒绝（首次不标回放，结果带系列编号与业务错误），再次
// 提交才回放。
func TestSetRoyaltyStatusRejectionSaveFailure(t *testing.T) {
	r := mustCreate(t, tempDir(t))
	seedRoyaltySaveFailureWorld(t, r)

	sealedReq := SetRoyaltyRequest{
		Operator: "alice", Reason: "封存后改规则", RequestID: "sr-rej-sealed",
		SeriesID: "s3", Shares: []RoyaltyShare{{AccountID: "bob", Rate: 1}},
	}
	forbiddenReq := SetRoyaltyRequest{
		Operator: "bob", Reason: "无权设置", RequestID: "sr-rej-forbidden",
		SeriesID: "s1", Shares: []RoyaltyShare{{AccountID: "bob", Rate: 1}},
	}

	// 普通写入失败（原数据可读）：两类状态拒绝的拒绝结果都没能保存。
	blockBatchSave(t, r)
	res, err := r.SetRoyalty(sealedReq)
	assertRoyaltySaveFailureEmpty(t, res, err)
	assertRoyaltyRequestFree(t, r, sealedReq, 3)
	res, err = r.SetRoyalty(forbiddenReq)
	assertRoyaltySaveFailureEmpty(t, res, err)
	assertRoyaltyRequestFree(t, r, forbiddenReq, 3)
	// 被拒绝的目标系列规则与历史都不变，也没有新增拒绝之外的任何变更记录。
	assertSeriesRule(t, r, "s3", ruleS3...)
	assertRoyaltyHistoryEvents(t, r, "s3", s3HistoryBaseline())
	assertSeriesRule(t, r, "s1", ruleS1Old2...)
	assertRoyaltyHistoryEvents(t, r, "s1", s1HistoryBaseline())
	restoreBatchSave(t, r)

	// 恢复后原请求重新保存对应业务拒绝：首次不标回放。
	res, err = r.SetRoyalty(sealedReq)
	if !errors.Is(err, ErrSeriesSealed) {
		t.Fatalf("重提应返回 ErrSeriesSealed: %v", err)
	}
	if res.Replayed || res.SeriesID != "s3" || !errors.Is(res.Err, ErrSeriesSealed) {
		t.Fatalf("封存拒绝的重提结果异常: %+v", res)
	}
	res, err = r.SetRoyalty(forbiddenReq)
	if !errors.Is(err, ErrForbidden) {
		t.Fatalf("重提应返回 ErrForbidden: %v", err)
	}
	if res.Replayed || res.SeriesID != "s1" || !errors.Is(res.Err, ErrForbidden) {
		t.Fatalf("无权拒绝的重提结果异常: %+v", res)
	}

	// 再次提交相同请求才回放已保存的拒绝；业务拒绝不新增规则变更历史。
	res, err = r.SetRoyalty(sealedReq)
	if !errors.Is(err, ErrSeriesSealed) || !res.Replayed ||
		!errors.Is(res.Err, ErrSeriesSealed) {
		t.Fatalf("封存拒绝应被回放: %+v, err %v", res, err)
	}
	res, err = r.SetRoyalty(forbiddenReq)
	if !errors.Is(err, ErrForbidden) || !res.Replayed ||
		!errors.Is(res.Err, ErrForbidden) {
		t.Fatalf("无权拒绝应被回放: %+v, err %v", res, err)
	}
	assertRoyaltyHistoryEvents(t, r, "s1", s1HistoryBaseline())
	assertRoyaltyHistoryEvents(t, r, "s3", s3HistoryBaseline())
}

// TestSetRoyaltySaveFailureAfterReopen 覆盖磁盘视角：普通写入失败（原数据
// 可读）后关闭重开，看到的仍是提交前状态——旧规则不变、无幻影历史、请求号
// 未占用、全局序号未消耗；恢复保存后用原请求重提正常成功一次（不回放），
// 再次提交回放，落盘后再重开结果一致。
func TestSetRoyaltySaveFailureAfterReopen(t *testing.T) {
	dir := tempDir(t)
	r := mustCreate(t, dir)
	seedRoyaltySaveFailureWorld(t, r)
	req := SetRoyaltyRequest{
		Operator: "alice", Reason: "改给 carol 和 erin", RequestID: "sr-reopen",
		SeriesID: "s1", Shares: ruleS1New,
	}
	s2Req := SetRoyaltyRequest{
		Operator: "alice", Reason: "给 s2 设规则", RequestID: "sr-reopen-s2",
		SeriesID: "s2", Shares: []RoyaltyShare{{AccountID: "bob", Rate: 2500}},
	}

	blockBatchSave(t, r)
	res, err := r.SetRoyalty(req)
	assertRoyaltySaveFailureEmpty(t, res, err)
	res, err = r.SetRoyalty(s2Req)
	assertRoyaltySaveFailureEmpty(t, res, err)
	if err := r.Close(); err != nil {
		t.Fatal(err)
	}

	r2, err := Open(dir)
	if err != nil {
		t.Fatalf("失败后原登记册必须仍能正常读取: %v", err)
	}
	t.Cleanup(func() { _ = r2.Close() })
	assertSeriesRule(t, r2, "s1", ruleS1Old2...)
	assertRoyaltyHistoryEvents(t, r2, "s1", s1HistoryBaseline())
	assertSeriesRule(t, r2, "s2")
	assertRoyaltyHistoryEvents(t, r2, "s2", nil)
	assertRoyaltyRequestFree(t, r2, req, 3)
	assertRoyaltyRequestFree(t, r2, s2Req, 3)
	assertSeriesRule(t, r2, "s3", ruleS3...)
	assertSealed(t, r2, "s3", true)

	restoreBatchSave(t, r2)
	// 原请求重提真正成功一次（序号 4），不回放。
	res, err = r2.SetRoyalty(req)
	if err != nil || res.Replayed || res.Err != nil || res.SeriesID != "s1" {
		t.Fatalf("重开后原请求重提应正常成功: %+v, err %v", res, err)
	}
	wantEvents := append(s1HistoryBaseline(), royaltyHistoryWant{
		seq: 4, operator: "alice", reason: "改给 carol 和 erin", rid: "sr-reopen",
		before: ruleS1Old2, after: ruleS1New,
	})
	assertRoyaltyHistoryEvents(t, r2, "s1", wantEvents)
	// 再次提交回放，不增加记录。
	res, err = r2.SetRoyalty(req)
	if err != nil || !res.Replayed || res.Err != nil {
		t.Fatalf("成功后重提应回放: %+v, err %v", res, err)
	}
	assertRoyaltyHistoryEvents(t, r2, "s1", wantEvents)

	// 落盘视角：再次重开，新规则与历史已持久化，失败请求从未留下痕迹。
	if err := r2.Close(); err != nil {
		t.Fatal(err)
	}
	r3, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = r3.Close() })
	assertSeriesRule(t, r3, "s1", ruleS1New...)
	assertRoyaltyHistoryEvents(t, r3, "s1", wantEvents)
	assertSeriesRule(t, r3, "s2")
	assertRoyaltyRequestFree(t, r3, s2Req, 4)
}
