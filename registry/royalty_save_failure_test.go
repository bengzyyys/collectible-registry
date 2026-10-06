package registry

import (
	"errors"
	"os"
	"testing"
)

// 本文件为版税规则设置（SetRoyalty）补充"保存失败"场景的自动化回归
// 保障，与发行、转让、授权等操作的 *_save_failure_test.go 行为对齐：
// 系列尚未发行、尚未封存，创建账户与收款账户均已登记且可用时，新规则
// （含用空规则清空）以一次保存完成为准。新内容尚未替换原登记册数据就
// 写入失败时，必须返回本次实际的保存错误——不能报告设置成功，也不能
// 只返回业务拒绝，更不能用失败后重新读取原数据时的错误（ErrCorrupt）
// 顶替保存错误；结果为空（系列编号、份额与业务错误均为空，不标回放）。
//
// 同一个仍打开的登记册必须保持提交前的样子：已有规则的收款账户及比例
// 不被换成新方案，清空失败不抹掉旧规则，此前未设置规则的系列仍无版税；
// 规则变更历史不多本次操作，已有记录的操作者、原因、请求号、前后份额与
// 时间保持原样，请求号与变更序号都不被消耗。这些既要在原数据仍可读取
// 的普通写入失败下成立，也要在原数据暂时无法读取或解析、状态无法重新
// 载入时成立。随后另一次正常操作成功保存不能夹带这次未保存的规则、变更
// 记录或请求号占用；保存条件恢复后用完全相同的请求重提，按当时的系列
// 状态重新处理（首次重提不标回放），此后原样重提才回放且不新增记录。
//
// 失败注入方式与其他场景共用：在临时文件路径 .registry.json.tmp 上
// 预先建一个目录，save 在 OpenFile 阶段即以 EISDIR 失败，原快照完好
// 可读；另有用例同时把数据文件写坏，覆盖"原数据暂时无法读取、无法重新
// 载入"的情形。

// assertSetRoyaltySaveFailureEmpty 核对保存失败的返回：error 是实际保存
// 错误，而不是设置成功、任何业务拒绝，或失败后重新读取原数据的错误；
// 结果为空——系列编号、份额为空，业务错误为空，不标记为重复返回。
func assertSetRoyaltySaveFailureEmpty(t *testing.T, res SetRoyaltyResult, err error) {
	t.Helper()
	assertSaveFailureError(t, err)
	if errors.Is(err, ErrCorrupt) {
		t.Fatalf("返回的应是本次保存错误，不能用失败后重读原数据的错误顶替: %v", err)
	}
	if errors.Is(err, ErrRoyaltyFrozen) {
		t.Fatalf("返回的应是保存错误，不能是业务拒绝 ErrRoyaltyFrozen: %v", err)
	}
	if res.SeriesID != "" || len(res.Shares) != 0 || res.Replayed || res.Err != nil {
		t.Fatalf("保存失败必须返回空结果: %+v", res)
	}
}

// assertRoyaltyRequestFree 在同一个已打开的登记册上核对：失败请求没有
// 占用操作者的请求号，规则变更序号没有被消耗。
func assertRoyaltyRequestFree(t *testing.T, r *Registry, operator, rid string, wantNextRoyaltySeq int64) {
	t.Helper()
	if _, ok := r.state.Requests[requestKey(operator, rid)]; ok {
		t.Fatalf("未保存的结果不应占用请求号 %s", rid)
	}
	if r.state.NextRoyaltySeq != wantNextRoyaltySeq {
		t.Fatalf("NextRoyaltySeq = %d, want %d", r.state.NextRoyaltySeq, wantNextRoyaltySeq)
	}
}

// assertRoyaltyRule 核对系列当前规则（含条数、账户与比例，顺序按登记册
// 的规范化排序）。
func assertRoyaltyRule(t *testing.T, r *Registry, series string, want ...RoyaltyShare) {
	t.Helper()
	got, err := r.GetSeriesRoyalty(series)
	if err != nil {
		t.Fatalf("GetSeriesRoyalty %s: %v", series, err)
	}
	wantSharesEqual(t, got, want...)
}

// royaltyEventWant 描述一条期望的版税规则变更记录（操作者与原因固定为
// setRoyaltyReq 的默认值，除非显式给出）。
type royaltyEventWant struct {
	seq    int64
	op     string
	reason string
	rid    string
	before []RoyaltyShare
	after  []RoyaltyShare
}

// assertRoyaltyEvents 按顺序核对系列的全部规则变更记录：序号、操作者、
// 原因、请求号与前后份额逐条一致。
func assertRoyaltyEvents(t *testing.T, r *Registry, series string, want []royaltyEventWant) {
	t.Helper()
	got, err := r.RoyaltyHistory(series)
	if err != nil {
		t.Fatalf("RoyaltyHistory %s: %v", series, err)
	}
	if len(got) != len(want) {
		t.Fatalf("%s 历史条数 = %d，期望 %d（实际 %+v）", series, len(got), len(want), got)
	}
	for i, w := range want {
		e := got[i]
		if e.Seq != w.seq || e.SeriesID != series || e.Operator != w.op ||
			e.Reason != w.reason || e.RequestID != w.rid {
			t.Fatalf("%s 第 %d 条记录元数据 = %+v，期望 seq=%d op=%s reason=%q rid=%s",
				series, i+1, e, w.seq, w.op, w.reason, w.rid)
		}
		if e.OccurredAt.IsZero() {
			t.Fatalf("%s 第 %d 条记录缺少发生时间", series, i+1)
		}
		wantSharesEqual(t, e.Before, w.before...)
		wantSharesEqual(t, e.After, w.after...)
	}
}

// assertRoyaltyHistoryIdentical 核对失败前后查询到的历史完全相同：条数、
// 序号、操作者、原因、请求号、前后份额与发生时间逐项一致。
func assertRoyaltyHistoryIdentical(t *testing.T, before, after []RoyaltyEvent) {
	t.Helper()
	if len(before) != len(after) {
		t.Fatalf("历史条数被改变: 失败前 %d 条，失败后 %d 条", len(before), len(after))
	}
	for i := range before {
		b, a := before[i], after[i]
		if b.Seq != a.Seq || b.SeriesID != a.SeriesID || b.Operator != a.Operator ||
			b.Reason != a.Reason || b.RequestID != a.RequestID ||
			!a.OccurredAt.Equal(b.OccurredAt) {
			t.Fatalf("第 %d 条历史记录被改变:\n失败前 %+v\n失败后 %+v", i+1, b, a)
		}
		wantSharesEqual(t, a.Before, b.Before...)
		wantSharesEqual(t, a.After, b.After...)
	}
}

// royaltyCorruptDataFile 把数据文件暂时写坏（无法解析，commit 的重新
// 载入必然失败），返回原内容供恢复。
func royaltyCorruptDataFile(t *testing.T, r *Registry) []byte {
	t.Helper()
	orig, err := os.ReadFile(dataFile(r.dir))
	if err != nil {
		t.Fatalf("读取原数据文件: %v", err)
	}
	if err := os.WriteFile(dataFile(r.dir), []byte("{corrupt"), fileMode); err != nil {
		t.Fatalf("写坏数据文件: %v", err)
	}
	return orig
}

// royaltyRestoreDataFile 恢复数据文件原内容。
func royaltyRestoreDataFile(t *testing.T, r *Registry, orig []byte) {
	t.Helper()
	if err := os.WriteFile(dataFile(r.dir), orig, fileMode); err != nil {
		t.Fatalf("恢复数据文件: %v", err)
	}
}

// ---- 换新规则成功结果落盘失败 ----

// TestSetRoyaltyNewRuleSaveFailure 覆盖：系列已有规则时，提交新收款账户
// 与比例的成功结果落盘失败（原数据仍可读取的普通写入失败），返回保存
// 错误、结果为空；同一个仍打开的登记册中当前规则与变更历史与提交前
// 完全相同，请求号不占用、变更序号不消耗。保存未恢复时再次提交仍失败、
// 不回放；关闭重开读到的也是原数据。恢复后无关操作成功保存不夹带失败
// 内容，相同请求按当前状态正常设置一次（不标回放，新记录紧接原有序号），
// 此后相同请求回放首次成功且不新增记录。
func TestSetRoyaltyNewRuleSaveFailure(t *testing.T) {
	dir := tempDir(t)
	r := mustCreate(t, dir)
	royaltyWorld(t, r)
	// 失败前已有两次真实变更，序号 1、2。
	ruleOld1 := []RoyaltyShare{{AccountID: "carol", Rate: 2000}}
	ruleOld2 := []RoyaltyShare{{AccountID: "dave", Rate: 300}}
	mustSetRoyalty(t, r, "rs-old-1", ruleOld1...)
	mustSetRoyalty(t, r, "rs-old-2", ruleOld2...)

	// 新方案：两个收款账户（顺序故意不按规范化排序）。
	newRule := []RoyaltyShare{
		{AccountID: "dave", Rate: 500},
		{AccountID: "bob", Rate: 1000},
	}
	wantNew := []RoyaltyShare{
		{AccountID: "bob", Rate: 1000},
		{AccountID: "dave", Rate: 500},
	}
	req := setRoyaltyReq("rs-new", newRule...)

	histBefore, err := r.RoyaltyHistory("s1")
	if err != nil {
		t.Fatal(err)
	}
	blockBatchSave(t, r)
	res, err := r.SetRoyalty(req)
	assertSetRoyaltySaveFailureEmpty(t, res, err)

	// 同一个仍打开的登记册：规则还是旧规则，历史与提交前完全相同，
	// 请求号与序号 3 都未被消耗。
	assertRoyaltyRequestFree(t, r, "alice", "rs-new", 2)
	assertRoyaltyRule(t, r, "s1", ruleOld2...)
	histAfter, err := r.RoyaltyHistory("s1")
	if err != nil {
		t.Fatal(err)
	}
	assertRoyaltyHistoryIdentical(t, histBefore, histAfter)

	// 保存条件未恢复时再次提交：仍重新尝试保存并失败，不能把上次未保存
	// 的结果当成已保存结果回放。
	res, err = r.SetRoyalty(req)
	assertSetRoyaltySaveFailureEmpty(t, res, err)
	assertRoyaltyRequestFree(t, r, "alice", "rs-new", 2)
	assertRoyaltyRule(t, r, "s1", ruleOld2...)

	// 数据文件从未被替换：关闭后重新打开读到的仍是提交前的原数据。
	if err := r.Close(); err != nil {
		t.Fatal(err)
	}
	r2, err := Open(dir)
	if err != nil {
		t.Fatalf("失败后原登记册必须仍能正常读取: %v", err)
	}
	t.Cleanup(func() { _ = r2.Close() })
	assertRoyaltyRequestFree(t, r2, "alice", "rs-new", 2)
	assertRoyaltyRule(t, r2, "s1", ruleOld2...)
	reopenedHist, err := r2.RoyaltyHistory("s1")
	if err != nil {
		t.Fatal(err)
	}
	assertRoyaltyHistoryIdentical(t, histBefore, reopenedHist)

	// 保存条件恢复后，无关操作（登记新账户）成功保存不能把这次未保存的
	// 规则、变更记录或请求号占用夹带落盘。
	restoreBatchSave(t, r2)
	if err := r2.RegisterAccount("erin", "路人"); err != nil {
		t.Fatal(err)
	}
	assertRoyaltyRequestFree(t, r2, "alice", "rs-new", 2)
	assertRoyaltyRule(t, r2, "s1", ruleOld2...)
	assertRoyaltyEvents(t, r2, "s1", []royaltyEventWant{
		{seq: 1, op: "alice", reason: "设定版税", rid: "rs-old-1", before: nil, after: ruleOld1},
		{seq: 2, op: "alice", reason: "设定版税", rid: "rs-old-2", before: ruleOld1, after: ruleOld2},
	})

	// 用完全相同的请求重提：正常设置一次，不标回放；新记录序号紧接此前
	// 的变更（序号 3），变更前是旧规则，变更后是新方案。
	res, err = r2.SetRoyalty(req)
	if err != nil {
		t.Fatalf("恢复后重提应正常设置: %v", err)
	}
	if res.Replayed || res.Err != nil || res.SeriesID != "s1" {
		t.Fatalf("重提结果异常: %+v", res)
	}
	wantSharesEqual(t, res.Shares, wantNew...)
	assertRoyaltyRule(t, r2, "s1", wantNew...)
	assertRoyaltyEvents(t, r2, "s1", []royaltyEventWant{
		{seq: 1, op: "alice", reason: "设定版税", rid: "rs-old-1", before: nil, after: ruleOld1},
		{seq: 2, op: "alice", reason: "设定版税", rid: "rs-old-2", before: ruleOld1, after: ruleOld2},
		{seq: 3, op: "alice", reason: "设定版税", rid: "rs-new", before: ruleOld2, after: wantNew},
	})

	// 成功结果保存后，相同请求（书写顺序不同但规范化后相同）回放首次
	// 成功，不新增变更记录。
	replay, err := r2.SetRoyalty(setRoyaltyReq("rs-new",
		RoyaltyShare{AccountID: "bob", Rate: 1000},
		RoyaltyShare{AccountID: "dave", Rate: 500},
	))
	if err != nil || !replay.Replayed || replay.SeriesID != "s1" || replay.Err != nil {
		t.Fatalf("成功后再提应回放: %+v, err %v", replay, err)
	}
	wantSharesEqual(t, replay.Shares, wantNew...)
	evs, _ := r2.RoyaltyHistory("s1")
	if len(evs) != 3 || evs[2].RequestID != "rs-new" {
		t.Fatalf("回放不应新增历史: %+v", evs)
	}
}

// TestSetRoyaltyNewRuleSaveFailureUnreadableDisk 覆盖：保存失败且原数据
// 暂时无法读取、解析，状态无法按磁盘重新载入时，同一个仍打开的登记册
// 也必须保持提交前的样子（回滚不依赖重新读取成功）；恢复正常读写后
// 无关操作不夹带，原请求正常设置一次而非回放。
func TestSetRoyaltyNewRuleSaveFailureUnreadableDisk(t *testing.T) {
	r := mustCreate(t, tempDir(t))
	royaltyWorld(t, r)
	oldRule := []RoyaltyShare{{AccountID: "carol", Rate: 2000}}
	mustSetRoyalty(t, r, "rs-old", oldRule...)

	newRule := []RoyaltyShare{
		{AccountID: "bob", Rate: 1000},
		{AccountID: "dave", Rate: 500},
	}
	req := setRoyaltyReq("rs-new", newRule...)
	histBefore, _ := r.RoyaltyHistory("s1")

	orig := royaltyCorruptDataFile(t, r)
	blockBatchSave(t, r)
	res, err := r.SetRoyalty(req)
	assertSetRoyaltySaveFailureEmpty(t, res, err)

	// 状态未能按磁盘重建时也必须显式撤销：同册查询仍是旧规则、原历史，
	// 请求号与序号 2 未被消耗。
	assertRoyaltyRequestFree(t, r, "alice", "rs-new", 1)
	assertRoyaltyRule(t, r, "s1", oldRule...)
	histAfter, _ := r.RoyaltyHistory("s1")
	assertRoyaltyHistoryIdentical(t, histBefore, histAfter)

	// 恢复正常读写。
	royaltyRestoreDataFile(t, r, orig)
	restoreBatchSave(t, r)

	// 无关操作成功保存不能夹带未保存的规则或请求号占用。
	if err := r.RegisterAccount("erin", "路人"); err != nil {
		t.Fatal(err)
	}
	assertRoyaltyRequestFree(t, r, "alice", "rs-new", 1)
	assertRoyaltyRule(t, r, "s1", oldRule...)
	histAfter, _ = r.RoyaltyHistory("s1")
	assertRoyaltyHistoryIdentical(t, histBefore, histAfter)

	// 原请求按当前状态完整执行一次（序号 2），不标回放；再提才回放。
	res, err = r.SetRoyalty(req)
	if err != nil {
		t.Fatalf("恢复后重提应正常设置: %v", err)
	}
	if res.Replayed || res.Err != nil || res.SeriesID != "s1" || len(res.Shares) != 2 {
		t.Fatalf("重提结果异常: %+v", res)
	}
	if r.state.NextRoyaltySeq != 2 {
		t.Fatalf("成功变更序号应紧接此前: NextRoyaltySeq = %d", r.state.NextRoyaltySeq)
	}
	replay, err := r.SetRoyalty(req)
	if err != nil || !replay.Replayed || replay.SeriesID != "s1" {
		t.Fatalf("成功后再提应回放: %+v, err %v", replay, err)
	}
	evs, _ := r.RoyaltyHistory("s1")
	if len(evs) != 2 {
		t.Fatalf("回放不应新增历史: %+v", evs)
	}
}

// ---- 清空规则成功结果落盘失败 ----

// TestSetRoyaltyClearSaveFailure 覆盖：已有规则的系列用空规则清空时保存
// 失败，旧规则不能被抹掉；普通写入失败与原数据不可读两种情形都检查。
// 恢复后相同请求正常清空一次（不标回放），再提回放且不新增记录。
func TestSetRoyaltyClearSaveFailure(t *testing.T) {
	cases := []struct {
		name    string
		corrupt bool
	}{
		{name: "readable_disk", corrupt: false},
		{name: "unreadable_disk", corrupt: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := mustCreate(t, tempDir(t))
			royaltyWorld(t, r)
			oldRule := []RoyaltyShare{{AccountID: "carol", Rate: 2000}}
			mustSetRoyalty(t, r, "rs-old", oldRule...)
			// 再来一次设置，让失败前有两条历史。
			oldRule2 := []RoyaltyShare{{AccountID: "dave", Rate: 400}}
			mustSetRoyalty(t, r, "rs-old-2", oldRule2...)
			req := setRoyaltyReq("rs-clear") // 空规则：清空版税

			histBefore, _ := r.RoyaltyHistory("s1")
			var orig []byte
			blockBatchSave(t, r)
			if tc.corrupt {
				orig = royaltyCorruptDataFile(t, r)
			}
			res, err := r.SetRoyalty(req)
			assertSetRoyaltySaveFailureEmpty(t, res, err)

			// 清空失败不能抹掉旧规则；历史与请求号、序号保持原样。
			assertRoyaltyRequestFree(t, r, "alice", "rs-clear", 2)
			assertRoyaltyRule(t, r, "s1", oldRule2...)
			histAfter, _ := r.RoyaltyHistory("s1")
			assertRoyaltyHistoryIdentical(t, histBefore, histAfter)

			if tc.corrupt {
				royaltyRestoreDataFile(t, r, orig)
			}
			restoreBatchSave(t, r)

			// 无关操作成功保存不夹带这次未保存的清空。
			if err := r.RegisterAccount("erin", "路人"); err != nil {
				t.Fatal(err)
			}
			assertRoyaltyRule(t, r, "s1", oldRule2...)
			if evs, _ := r.RoyaltyHistory("s1"); len(evs) != 2 {
				t.Fatalf("无关保存后历史应仍为 2 条: %+v", evs)
			}

			// 相同请求正常清空一次：序号 3，变更前为旧规则、变更后为空。
			res, err = r.SetRoyalty(req)
			if err != nil {
				t.Fatalf("恢复后重提应正常清空: %v", err)
			}
			if res.Replayed || res.Err != nil || res.SeriesID != "s1" || len(res.Shares) != 0 {
				t.Fatalf("清空结果异常: %+v", res)
			}
			assertRoyaltyRule(t, r, "s1")
			assertRoyaltyEvents(t, r, "s1", []royaltyEventWant{
				{seq: 1, op: "alice", reason: "设定版税", rid: "rs-old", before: nil, after: oldRule},
				{seq: 2, op: "alice", reason: "设定版税", rid: "rs-old-2", before: oldRule, after: oldRule2},
				{seq: 3, op: "alice", reason: "设定版税", rid: "rs-clear", before: oldRule2, after: nil},
			})

			// 再提回放这次清空，不新增记录。
			replay, err := r.SetRoyalty(req)
			if err != nil || !replay.Replayed || replay.Err != nil || len(replay.Shares) != 0 {
				t.Fatalf("清空成功后再提应回放: %+v, err %v", replay, err)
			}
			if evs, _ := r.RoyaltyHistory("s1"); len(evs) != 3 {
				t.Fatalf("回放不应新增历史: %+v", evs)
			}
		})
	}
}

// ---- 此前未设置规则的系列 ----

// TestSetRoyaltyNeverSetSaveFailure 覆盖：此前未设置规则（无版税）的系列，
// 无论是提交新收款账户与比例还是用空规则"清空"，保存失败后仍保持无
// 版税、历史为空、序号从 0 起、请求号未占用；普通写入失败与原数据不可
// 读两种情形都检查。恢复后无关保存不夹带，原请求正常生效一次（序号 1），
// 再提回放且不新增记录。
func TestSetRoyaltyNeverSetSaveFailure(t *testing.T) {
	cases := []struct {
		name    string
		corrupt bool
		shares  []RoyaltyShare // 失败请求的新内容
		want    []RoyaltyShare // 重提成功后期望生效的规则
	}{
		{name: "new_rule_readable", shares: []RoyaltyShare{{AccountID: "carol", Rate: 3000}},
			want: []RoyaltyShare{{AccountID: "carol", Rate: 3000}}},
		{name: "new_rule_unreadable", corrupt: true,
			shares: []RoyaltyShare{{AccountID: "bob", Rate: 100}, {AccountID: "dave", Rate: 200}},
			want:   []RoyaltyShare{{AccountID: "bob", Rate: 100}, {AccountID: "dave", Rate: 200}}},
		{name: "empty_rule_readable", shares: nil, want: nil},
		{name: "empty_rule_unreadable", corrupt: true, shares: nil, want: nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := mustCreate(t, tempDir(t))
			royaltyWorld(t, r)
			req := setRoyaltyReq("rs-first", tc.shares...)

			var orig []byte
			blockBatchSave(t, r)
			if tc.corrupt {
				orig = royaltyCorruptDataFile(t, r)
			}
			res, err := r.SetRoyalty(req)
			assertSetRoyaltySaveFailureEmpty(t, res, err)

			// 此前未设置规则的系列仍保持无版税：无当前规则、无历史、序号
			// 未消耗、请求号未占用。
			assertRoyaltyRequestFree(t, r, "alice", "rs-first", 0)
			assertRoyaltyRule(t, r, "s1")
			if evs, _ := r.RoyaltyHistory("s1"); len(evs) != 0 {
				t.Fatalf("未设置过规则的系列失败后历史应仍为空: %+v", evs)
			}

			if tc.corrupt {
				royaltyRestoreDataFile(t, r, orig)
			}
			restoreBatchSave(t, r)

			// 无关操作成功保存不能夹带这次未保存的规则。
			if err := r.RegisterAccount("erin", "路人"); err != nil {
				t.Fatal(err)
			}
			assertRoyaltyRequestFree(t, r, "alice", "rs-first", 0)
			assertRoyaltyRule(t, r, "s1")

			// 相同请求按当前状态正常生效一次：序号 1，首次重提不标回放。
			res, err = r.SetRoyalty(req)
			if err != nil {
				t.Fatalf("恢复后重提应正常处理: %v", err)
			}
			if res.Replayed || res.Err != nil || res.SeriesID != "s1" {
				t.Fatalf("重提结果异常: %+v", res)
			}
			wantSharesEqual(t, res.Shares, tc.want...)
			assertRoyaltyRule(t, r, "s1", tc.want...)
			assertRoyaltyEvents(t, r, "s1", []royaltyEventWant{
				{seq: 1, op: "alice", reason: "设定版税", rid: "rs-first", before: nil, after: tc.want},
			})

			// 再提回放首次成功，不新增记录。
			replay, err := r.SetRoyalty(req)
			if err != nil || !replay.Replayed || replay.Err != nil {
				t.Fatalf("成功后再提应回放: %+v, err %v", replay, err)
			}
			if evs, _ := r.RoyaltyHistory("s1"); len(evs) != 1 {
				t.Fatalf("回放不应新增历史: %+v", evs)
			}
		})
	}
}

// ---- 其他系列与后续正常操作的隔离 ----

// TestSetRoyaltySaveFailureSeriesIsolation 覆盖：一个系列保存失败的规则
// 不影响此前已成功设置规则与历史的其他系列；恢复后另一个系列的一次正常
// 设置成功保存时，也不能夹带失败系列的规则或历史；随后失败系列原请求
// 正常成功，两个系列的记录各自独立。
func TestSetRoyaltySaveFailureSeriesIsolation(t *testing.T) {
	cases := []struct {
		name    string
		corrupt bool
	}{
		{name: "readable_disk"},
		{name: "unreadable_disk", corrupt: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := mustCreate(t, tempDir(t))
			royaltyWorld(t, r)
			if err := r.CreateSeries("s2", "alice", "另一个系列"); err != nil {
				t.Fatal(err)
			}
			s1Rule := []RoyaltyShare{{AccountID: "carol", Rate: 1000}}
			s2Rule := []RoyaltyShare{{AccountID: "bob", Rate: 2000}}
			mustSetRoyalty(t, r, "rs-s1-old", s1Rule...)
			s2req := setRoyaltyReq("rs-s2-old", s2Rule...)
			s2req.SeriesID = "s2"
			if _, err := r.SetRoyalty(s2req); err != nil {
				t.Fatal(err)
			}

			// 版税变更序号跨系列全局递增：s1 旧记录占序号 1，s2 旧记录占 2。
			// s1 失败不消耗序号，NextRoyaltySeq 仍为 2。
			failReq := setRoyaltyReq("rs-s1-new", RoyaltyShare{AccountID: "dave", Rate: 500})
			var orig []byte
			blockBatchSave(t, r)
			if tc.corrupt {
				orig = royaltyCorruptDataFile(t, r)
			}
			res, err := r.SetRoyalty(failReq)
			assertSetRoyaltySaveFailureEmpty(t, res, err)

			// s1 保持旧规则旧历史；s2 的规则与历史不受影响。
			assertRoyaltyRequestFree(t, r, "alice", "rs-s1-new", 2)
			assertRoyaltyRule(t, r, "s1", s1Rule...)
			assertRoyaltyEvents(t, r, "s1", []royaltyEventWant{
				{seq: 1, op: "alice", reason: "设定版税", rid: "rs-s1-old", before: nil, after: s1Rule},
			})
			assertRoyaltyRule(t, r, "s2", s2Rule...)
			assertRoyaltyEvents(t, r, "s2", []royaltyEventWant{
				{seq: 2, op: "alice", reason: "设定版税", rid: "rs-s2-old", before: nil, after: s2Rule},
			})

			if tc.corrupt {
				royaltyRestoreDataFile(t, r, orig)
			}
			restoreBatchSave(t, r)

			// s2 上一次正常设置成功保存（全局序号 3）：不能夹带 s1 的失败规则或历史。
			s2New := []RoyaltyShare{{AccountID: "dave", Rate: 700}}
			s2newReq := setRoyaltyReq("rs-s2-new", s2New...)
			s2newReq.SeriesID = "s2"
			if _, err := r.SetRoyalty(s2newReq); err != nil {
				t.Fatalf("s2 的正常设置应成功: %v", err)
			}
			assertRoyaltyRequestFree(t, r, "alice", "rs-s1-new", 3)
			assertRoyaltyRule(t, r, "s1", s1Rule...)
			assertRoyaltyEvents(t, r, "s1", []royaltyEventWant{
				{seq: 1, op: "alice", reason: "设定版税", rid: "rs-s1-old", before: nil, after: s1Rule},
			})
			assertRoyaltyRule(t, r, "s2", s2New...)
			assertRoyaltyEvents(t, r, "s2", []royaltyEventWant{
				{seq: 2, op: "alice", reason: "设定版税", rid: "rs-s2-old", before: nil, after: s2Rule},
				{seq: 3, op: "alice", reason: "设定版税", rid: "rs-s2-new", before: s2Rule, after: s2New},
			})

			// s1 原请求随后正常成功（全局序号 4），不影响 s2。
			res, err = r.SetRoyalty(failReq)
			if err != nil || res.Replayed || res.Err != nil {
				t.Fatalf("s1 原请求应正常成功: %+v, err %v", res, err)
			}
			assertRoyaltyRule(t, r, "s1", RoyaltyShare{AccountID: "dave", Rate: 500})
			s1events, _ := r.RoyaltyHistory("s1")
			if len(s1events) != 2 || s1events[1].Seq != 4 ||
				s1events[1].RequestID != "rs-s1-new" {
				t.Fatalf("s1 新记录序号应紧接全局序号: %+v", s1events)
			}
			if evs, _ := r.RoyaltyHistory("s2"); len(evs) != 2 {
				t.Fatalf("s1 的成功不应改变 s2 历史: %+v", evs)
			}
		})
	}
}

// ---- 恢复后系列状态已变化：按当时状态重新处理 ----

// TestSetRoyaltySaveFailureRetryAfterStateChange 覆盖：保存条件恢复后
// 完全相同的请求按当时的系列状态重新判断，而不是回放失败时尚未保存的
// 成功——失败后系列首次发行（规则固定为无版税）或被封存的，重提分别
// 返回 ErrRoyaltyFrozen / ErrSeriesSealed（首次重提不标回放、是新保存的
// 拒绝）；此后原样重提才回放该拒绝。普通写入失败与原数据不可读都覆盖。
func TestSetRoyaltySaveFailureRetryAfterStateChange(t *testing.T) {
	cases := []struct {
		name    string
		corrupt bool
		sealed  bool // true：恢复后封存；false：恢复后首次发行固定
		wantErr error
	}{
		{name: "frozen_readable", wantErr: ErrRoyaltyFrozen},
		{name: "frozen_unreadable", corrupt: true, wantErr: ErrRoyaltyFrozen},
		{name: "sealed_readable", sealed: true, wantErr: ErrSeriesSealed},
		{name: "sealed_unreadable", sealed: true, corrupt: true, wantErr: ErrSeriesSealed},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := mustCreate(t, tempDir(t))
			royaltyWorld(t, r)
			req := setRoyaltyReq("rs-new",
				RoyaltyShare{AccountID: "carol", Rate: 1000},
				RoyaltyShare{AccountID: "dave", Rate: 500},
			)

			var orig []byte
			blockBatchSave(t, r)
			if tc.corrupt {
				orig = royaltyCorruptDataFile(t, r)
			}
			res, err := r.SetRoyalty(req)
			assertSetRoyaltySaveFailureEmpty(t, res, err)
			assertRoyaltyRequestFree(t, r, "alice", "rs-new", 0)
			assertRoyaltyRule(t, r, "s1")

			if tc.corrupt {
				royaltyRestoreDataFile(t, r, orig)
			}
			restoreBatchSave(t, r)

			// 恢复期间系列状态变化：首次发行固定规则，或被封存。
			if tc.sealed {
				if err := r.SealSeries("s1", "alice"); err != nil {
					t.Fatal(err)
				}
			} else {
				if _, err := r.Issue(issueReq("i1", "alice")); err != nil {
					t.Fatal(err)
				}
			}

			// 相同请求按当前状态重新处理：保存成功后返回对应业务拒绝，
			// 这次重新处理不标回放；拒绝不改变规则、不新增变更记录。
			res, err = r.SetRoyalty(req)
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("重提应按当前状态返回 %v: %v", tc.wantErr, err)
			}
			if res.Replayed || res.SeriesID != "s1" || !errors.Is(res.Err, tc.wantErr) ||
				len(res.Shares) != 0 {
				t.Fatalf("重新处理的拒绝结果异常: %+v", res)
			}
			if _, ok := r.state.Requests[requestKey("alice", "rs-new")]; !ok {
				t.Fatal("状态类拒绝保存成功后应登记请求结果")
			}
			assertRoyaltyRule(t, r, "s1")
			if evs, _ := r.RoyaltyHistory("s1"); len(evs) != 0 {
				t.Fatalf("拒绝不应新增变更记录: %+v", evs)
			}
			if r.state.NextRoyaltySeq != 0 {
				t.Fatalf("拒绝不应消耗变更序号: NextRoyaltySeq = %d", r.state.NextRoyaltySeq)
			}

			// 此后原样重提回放这条已保存的拒绝，仍不产生规则与历史。
			res, err = r.SetRoyalty(req)
			if !errors.Is(err, tc.wantErr) || !res.Replayed ||
				!errors.Is(res.Err, tc.wantErr) || res.SeriesID != "s1" {
				t.Fatalf("拒绝保存后再提应回放该拒绝: %+v, err %v", res, err)
			}
			assertRoyaltyRule(t, r, "s1")
			if evs, _ := r.RoyaltyHistory("s1"); len(evs) != 0 {
				t.Fatalf("回放拒绝不应新增变更记录: %+v", evs)
			}
		})
	}
}

// ---- 状态类业务拒绝的落盘失败 ----

// TestSetRoyaltyRejectSaveFailure 覆盖：业务参数合法但状态类拒绝（已封存、
// 创建账户停用、非创建账户无权）的拒绝结果落盘失败时，同样返回保存错误、
// 结果为空、请求号不占用、不新增变更记录；封存场景同时覆盖原数据不可读。
// 保存恢复后相同请求重新保存此次拒绝并返回业务错误（首次重提不标回放），
// 此后相同请求回放该拒绝。
func TestSetRoyaltyRejectSaveFailure(t *testing.T) {
	t.Run("sealed", func(t *testing.T) {
		for _, corrupt := range []bool{false, true} {
			name := "readable"
			if corrupt {
				name = "unreadable"
			}
			t.Run(name, func(t *testing.T) {
				r := mustCreate(t, tempDir(t))
				royaltyWorld(t, r)
				if err := r.SealSeries("s1", "alice"); err != nil {
					t.Fatal(err)
				}
				req := setRoyaltyReq("rs-sealed", RoyaltyShare{AccountID: "carol", Rate: 100})

				var orig []byte
				blockBatchSave(t, r)
				if corrupt {
					orig = royaltyCorruptDataFile(t, r)
				}
				res, err := r.SetRoyalty(req)
				assertSetRoyaltySaveFailureEmpty(t, res, err)
				assertRoyaltyRequestFree(t, r, "alice", "rs-sealed", 0)
				assertRoyaltyRule(t, r, "s1")
				if evs, _ := r.RoyaltyHistory("s1"); len(evs) != 0 {
					t.Fatalf("未保存的拒绝不应新增历史: %+v", evs)
				}

				if corrupt {
					royaltyRestoreDataFile(t, r, orig)
				}
				restoreBatchSave(t, r)

				// 重提先成功保存拒绝，再返回 ErrSeriesSealed；不标回放。
				res, err = r.SetRoyalty(req)
				if !errors.Is(err, ErrSeriesSealed) {
					t.Fatalf("重提应返回 ErrSeriesSealed: %v", err)
				}
				if res.Replayed || res.SeriesID != "s1" ||
					!errors.Is(res.Err, ErrSeriesSealed) || len(res.Shares) != 0 {
					t.Fatalf("拒绝结果异常: %+v", res)
				}
				if r.state.NextRoyaltySeq != 0 {
					t.Fatalf("拒绝不应消耗变更序号: %d", r.state.NextRoyaltySeq)
				}
				// 再提回放该拒绝。
				res, err = r.SetRoyalty(req)
				if !errors.Is(err, ErrSeriesSealed) || !res.Replayed ||
					!errors.Is(res.Err, ErrSeriesSealed) {
					t.Fatalf("拒绝保存后再提应回放: %+v, err %v", res, err)
				}
			})
		}
	})

	t.Run("creator_inactive", func(t *testing.T) {
		r := mustCreate(t, tempDir(t))
		royaltyWorld(t, r)
		if err := r.DeactivateAccount("alice"); err != nil {
			t.Fatal(err)
		}
		req := setRoyaltyReq("rs-inactive", RoyaltyShare{AccountID: "carol", Rate: 100})

		blockBatchSave(t, r)
		res, err := r.SetRoyalty(req)
		assertSetRoyaltySaveFailureEmpty(t, res, err)
		assertRoyaltyRequestFree(t, r, "alice", "rs-inactive", 0)

		restoreBatchSave(t, r)
		res, err = r.SetRoyalty(req)
		if !errors.Is(err, ErrAccountInactive) {
			t.Fatalf("重提应返回 ErrAccountInactive: %v", err)
		}
		if res.Replayed || res.SeriesID != "s1" || !errors.Is(res.Err, ErrAccountInactive) {
			t.Fatalf("停用拒绝结果异常: %+v", res)
		}
		res, err = r.SetRoyalty(req)
		if !errors.Is(err, ErrAccountInactive) || !res.Replayed {
			t.Fatalf("拒绝保存后再提应回放: %+v, err %v", res, err)
		}
	})

	t.Run("non_creator_forbidden", func(t *testing.T) {
		r := mustCreate(t, tempDir(t))
		royaltyWorld(t, r)
		req := setRoyaltyReq("rs-forbidden", RoyaltyShare{AccountID: "carol", Rate: 100})
		req.Operator = "bob" // 非创建账户：ErrForbidden

		blockBatchSave(t, r)
		res, err := r.SetRoyalty(req)
		assertSetRoyaltySaveFailureEmpty(t, res, err)
		assertRoyaltyRequestFree(t, r, "bob", "rs-forbidden", 0)

		// 无权的未保存拒绝不影响创建账户继续设置自己的规则。
		restoreBatchSave(t, r)
		if _, err := r.SetRoyalty(setRoyaltyReq("rs-ok",
			RoyaltyShare{AccountID: "dave", Rate: 9})); err != nil {
			t.Fatalf("创建账户的正常设置不应受失败拒绝影响: %v", err)
		}

		res, err = r.SetRoyalty(req)
		if !errors.Is(err, ErrForbidden) {
			t.Fatalf("重提应返回 ErrForbidden: %v", err)
		}
		if res.Replayed || res.SeriesID != "s1" || !errors.Is(res.Err, ErrForbidden) {
			t.Fatalf("无权拒绝结果异常: %+v", res)
		}
		res, err = r.SetRoyalty(req)
		if !errors.Is(err, ErrForbidden) || !res.Replayed {
			t.Fatalf("拒绝保存后再提应回放: %+v, err %v", res, err)
		}
	})
}

// ---- 参数错误与引用不存在不要求保存 ----

// TestSetRoyaltyValidationErrorIgnoresSaveFailure 覆盖：份额形状不合法、
// 必填内容缺失或引用对象不存在的请求不占用请求号、不落任何记录，即使
// 数据位置暂时不可写也仍返回原参数/引用错误，不能改报保存错误；原规则
// 不变，该请求号恢复后仍可用于一次合法设置。
func TestSetRoyaltyValidationErrorIgnoresSaveFailure(t *testing.T) {
	r := mustCreate(t, tempDir(t))
	royaltyWorld(t, r)
	oldRule := []RoyaltyShare{{AccountID: "dave", Rate: 777}}
	mustSetRoyalty(t, r, "rs-old", oldRule...)

	blockBatchSave(t, r)
	defer restoreBatchSave(t, r)

	// 重复收款账户：ErrInvalidArgument，不是保存错误；结果带系列与业务错误。
	dup := setRoyaltyReq("rs-bad-1",
		RoyaltyShare{AccountID: "carol", Rate: 1}, RoyaltyShare{AccountID: "carol", Rate: 2})
	res, err := r.SetRoyalty(dup)
	if !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("重复账户应返回 ErrInvalidArgument，不能改报保存错误: %v", err)
	}
	if res.SeriesID != "s1" || !errors.Is(res.Err, ErrInvalidArgument) || res.Replayed {
		t.Fatalf("参数错误的结果异常: %+v", res)
	}
	assertRoyaltyRequestFree(t, r, "alice", "rs-bad-1", 1)

	// 比例合计超限：同样是参数错误。
	over := setRoyaltyReq("rs-bad-2",
		RoyaltyShare{AccountID: "carol", Rate: 6000}, RoyaltyShare{AccountID: "dave", Rate: 4001})
	if _, err := r.SetRoyalty(over); !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("合计超限应返回 ErrInvalidArgument: %v", err)
	}
	assertRoyaltyRequestFree(t, r, "alice", "rs-bad-2", 1)

	// 必填内容缺失（请求号为空）：ErrInvalidArgument，空结果。
	missing := setRoyaltyReq("", RoyaltyShare{AccountID: "carol", Rate: 1})
	missing.RequestID = ""
	if _, err := r.SetRoyalty(missing); !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("必填缺失应返回 ErrInvalidArgument: %v", err)
	}

	// 系列不存在：ErrNotFound（引用不存在），不占用请求号。
	noSeries := setRoyaltyReq("rs-bad-3", RoyaltyShare{AccountID: "carol", Rate: 1})
	noSeries.SeriesID = "nope"
	res, err = r.SetRoyalty(noSeries)
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("系列不存在应返回 ErrNotFound: %v", err)
	}
	if res.SeriesID != "nope" || !errors.Is(res.Err, ErrNotFound) {
		t.Fatalf("引用不存在的结果异常: %+v", res)
	}
	assertRoyaltyRequestFree(t, r, "alice", "rs-bad-3", 1)

	// 收款账户不存在：ErrNotFound，同一请求号不被占用。
	noPayee := setRoyaltyReq("rs-bad-4", RoyaltyShare{AccountID: "ghost", Rate: 1})
	if _, err := r.SetRoyalty(noPayee); !errors.Is(err, ErrNotFound) {
		t.Fatalf("收款账户不存在应返回 ErrNotFound: %v", err)
	}
	assertRoyaltyRequestFree(t, r, "alice", "rs-bad-4", 1)

	// 所有被拒绝的请求都没有改变规则，也没有新增历史。
	assertRoyaltyRule(t, r, "s1", oldRule...)
	if evs, _ := r.RoyaltyHistory("s1"); len(evs) != 1 {
		t.Fatalf("参数/引用错误不应新增历史: %+v", evs)
	}

	// 恢复后原请求号可用于一次合法设置（不回放任何失败）。defer 再恢复一次
	// 只是幂等清理；此处显式恢复保证合法设置可以真正落盘。
	restoreBatchSave(t, r)
	ok := setRoyaltyReq("rs-bad-4", RoyaltyShare{AccountID: "carol", Rate: 1})
	res, err = r.SetRoyalty(ok)
	if err != nil || res.Replayed || res.SeriesID != "s1" {
		t.Fatalf("原请求号应仍可用于合法设置: %+v, err %v", res, err)
	}
}

// ---- 请求号在全部操作间共用，失败请求不占用 ----

// TestSetRoyaltySaveFailureRequestIDNotConsumed 覆盖：版税设置保存失败
// 不占用操作者在全部操作间共用的请求号——同号的发行请求可以正常完成，
// 不会遭遇请求号冲突或回放到未保存的版税结果；该发行成功保存时也不夹带
// 失败请求的规则或历史。
func TestSetRoyaltySaveFailureRequestIDNotConsumed(t *testing.T) {
	r := mustCreate(t, tempDir(t))
	royaltyWorld(t, r)
	// 提交新规则但保存失败（请求号 shared-1）。
	req := setRoyaltyReq("shared-1", RoyaltyShare{AccountID: "carol", Rate: 500})
	blockBatchSave(t, r)
	res, err := r.SetRoyalty(req)
	assertSetRoyaltySaveFailureEmpty(t, res, err)
	assertRoyaltyRequestFree(t, r, "alice", "shared-1", 0)
	restoreBatchSave(t, r)

	// 同一操作者用同一请求号发起发行：版税设置未保存成功，发行应完整
	// 执行一次（而非请求号冲突或版税回放）；发行使规则固定为无版税。
	ireq := issueReq("i1", "alice")
	ireq.RequestID = "shared-1"
	ires, err := r.Issue(ireq)
	if err != nil {
		t.Fatalf("失败请求不应占用共用请求号: %v", err)
	}
	if ires.Replayed || ires.TxSeq != 1 || ires.OwnerID != "alice" || ires.Version != 1 {
		t.Fatalf("发行应完整执行一次: %+v", ires)
	}
	// 发行保存不夹带失败的版税规则：规则仍为空、无版税变更历史。
	assertRoyaltyRule(t, r, "s1")
	if evs, _ := r.RoyaltyHistory("s1"); len(evs) != 0 {
		t.Fatalf("发行保存不应夹带失败的版税历史: %+v", evs)
	}
	if r.state.NextRoyaltySeq != 0 {
		t.Fatalf("失败版税请求不应消耗变更序号: %d", r.state.NextRoyaltySeq)
	}
	// 同号再提发行，回放的是已保存的发行结果（而非版税结果）。
	replay, err := r.Issue(ireq)
	if err != nil || !replay.Replayed || replay.TxSeq != 1 {
		t.Fatalf("同号发行应回放发行结果: %+v, err %v", replay, err)
	}
	// 此时同号提交版税设置按请求号冲突拒绝（请求号已真正归属发行）。
	if _, err := r.SetRoyalty(req); !errors.Is(err, ErrRequestConflict) {
		t.Fatalf("同号版税请求应报请求号冲突: %v", err)
	}
	// 首次发行后规则固定：换新请求号设置仍被冻结拒绝。
	frozen := setRoyaltyReq("rs-after-issue", RoyaltyShare{AccountID: "carol", Rate: 500})
	if _, err := r.SetRoyalty(frozen); !errors.Is(err, ErrRoyaltyFrozen) {
		t.Fatalf("首次发行后设置应被冻结拒绝: %v", err)
	}
}
