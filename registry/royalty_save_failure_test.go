package registry

import (
	"errors"
	"os"
	"testing"
)

// 本文件为版税规则设置补充"保存失败"场景的回归保障：只有本次规则、变更
// 记录与请求结果都保存完成，SetRoyalty 才返回成功；状态类业务拒绝（停用、
// 无权、封存、首次发行后规则固定等）也必须在拒绝结果保存后才返回该业务
// 错误。本次内容尚未写入原登记册就保存失败时，调用者收到实际保存错误，
// 结果为空（系列编号、份额、业务错误均为空，不标回放）；仍打开的登记册中
// 当前规则与历次变更保持操作前的内容与顺序，未保存的变更不占用变更序号
// 或请求号；即使失败后原数据暂时无法读取，未保存的规则、变更或拒绝也不
// 留在当前登记册中，后续其他操作成功保存不会顺带写入。保存条件恢复后用
// 完全相同的请求重提，按当时状态重新判断；结果保存完成后再次提交才回放。
//
// 失败注入复用 blockBatchSave/restoreBatchSave：在临时文件路径上预置目录，
// save 在 rename 替换 registry.json 之前确定性失败，原快照完好可读；再覆写
// 数据文件为损坏内容可模拟原数据同时暂时无法读取。

// assertRoyaltySaveFailureEmpty 核对保存失败的返回：error 是保存错误而非
// 业务拒绝；结果为空——无系列编号、无份额、不标回放、业务错误为空。
func assertRoyaltySaveFailureEmpty(t *testing.T, res SetRoyaltyResult, err error) {
	t.Helper()
	assertSaveFailureError(t, err)
	if res.SeriesID != "" || len(res.Shares) != 0 || res.Replayed || res.Err != nil {
		t.Fatalf("保存失败必须整体失败，结果应为空: %+v", res)
	}
}

// assertRoyaltyRequestFree 核对请求号未被占用、变更序号与变更记录保持
// 操作前状态。
func assertRoyaltyRequestFree(t *testing.T, r *Registry, operator, rid string, wantSeq int64, wantEvents int) {
	t.Helper()
	if _, ok := r.state.Requests[requestKey(operator, rid)]; ok {
		t.Fatalf("未保存的请求不应占用请求号 %s", rid)
	}
	if r.state.NextRoyaltySeq != wantSeq {
		t.Fatalf("NextRoyaltySeq = %d, want %d", r.state.NextRoyaltySeq, wantSeq)
	}
	if len(r.state.RoyaltyEvents) != wantEvents {
		t.Fatalf("RoyaltyEvents = %d 条, want %d", len(r.state.RoyaltyEvents), wantEvents)
	}
}

// assertCurrentRoyalty 核对系列当前规则与期望完全一致（账户与比例、顺序）。
func assertCurrentRoyalty(t *testing.T, r *Registry, seriesID string, want ...RoyaltyShare) {
	t.Helper()
	got, err := r.GetSeriesRoyalty(seriesID)
	if err != nil {
		t.Fatalf("GetSeriesRoyalty: %v", err)
	}
	if len(got) != len(want) {
		t.Fatalf("当前规则 = %+v, want %+v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("当前规则 = %+v, want %+v", got, want)
		}
	}
}

// TestSetRoyaltySuccessSaveFailure 覆盖题述核心场景：设置通过业务检查但
// 保存失败时返回实际保存错误、结果为空；仍打开的登记册中规则仍为空、无
// 变更记录、序号与请求号未消耗；恢复后其他操作成功保存不夹带这次未保存
// 的规则；原请求重提正常设置（不标回放），保存成功后再次提交才回放。
func TestSetRoyaltySuccessSaveFailure(t *testing.T) {
	r := mustCreate(t, tempDir(t))
	royaltyWorld(t, r)
	req := setRoyaltyReq("rr-1", RoyaltyShare{AccountID: "carol", Rate: 500})

	blockBatchSave(t, r)
	res, err := r.SetRoyalty(req)
	assertRoyaltySaveFailureEmpty(t, res, err)
	assertRoyaltyRequestFree(t, r, "alice", "rr-1", 0, 0)
	assertCurrentRoyalty(t, r, "s1")

	// 保存条件恢复后，其他账户登记等正常操作成功保存，不得顺带写入这次
	// 未保存的规则、变更或请求结果。
	restoreBatchSave(t, r)
	if err := r.RegisterAccount("erin", "路人"); err != nil {
		t.Fatalf("其他正常操作应能成功保存: %v", err)
	}
	assertRoyaltyRequestFree(t, r, "alice", "rr-1", 0, 0)
	assertCurrentRoyalty(t, r, "s1")

	// 用完全相同的请求重提：按当时状态重新判断并正常设置，不标回放。
	res, err = r.SetRoyalty(req)
	if err != nil {
		t.Fatalf("恢复后重提应正常设置: %v", err)
	}
	if res.Replayed || res.Err != nil || res.SeriesID != "s1" ||
		len(res.Shares) != 1 || res.Shares[0] != (RoyaltyShare{AccountID: "carol", Rate: 500}) {
		t.Fatalf("重提结果异常: %+v", res)
	}
	assertCurrentRoyalty(t, r, "s1", RoyaltyShare{AccountID: "carol", Rate: 500})
	evs, err := r.RoyaltyHistory("s1")
	if err != nil || len(evs) != 1 || evs[0].Seq != 1 || evs[0].RequestID != "rr-1" ||
		len(evs[0].Before) != 0 || len(evs[0].After) != 1 {
		t.Fatalf("变更记录异常: %+v, %v", evs, err)
	}

	// 结果保存完成后，再次相同提交才按原有规则回放。
	res, err = r.SetRoyalty(req)
	if err != nil || !res.Replayed || res.SeriesID != "s1" ||
		len(res.Shares) != 1 || res.Shares[0] != (RoyaltyShare{AccountID: "carol", Rate: 500}) {
		t.Fatalf("保存后重提应回放: %+v, err %v", res, err)
	}
	if evs, _ := r.RoyaltyHistory("s1"); len(evs) != 1 {
		t.Fatalf("回放不应新增变更记录: %+v", evs)
	}
}

// TestSetRoyaltyReplaceSaveFailureUnreadableDisk 覆盖：已有非空规则被替换，
// 保存失败且原数据同时暂时无法读取（commit 无法按磁盘重建状态）时，未保存
// 的新规则、变更记录与请求号占用也不能留在当前登记册中——原规则完整保留，
// 变更记录与序号保持操作前状态；恢复正常读写后其他操作成功保存不夹带本次
// 内容，原请求重提正常生效。
func TestSetRoyaltyReplaceSaveFailureUnreadableDisk(t *testing.T) {
	r := mustCreate(t, tempDir(t))
	royaltyWorld(t, r)
	mustSetRoyalty(t, r, "rr-1", RoyaltyShare{AccountID: "carol", Rate: 500})
	req := setRoyaltyReq("rr-2", RoyaltyShare{AccountID: "dave", Rate: 800})

	// 让保存失败，同时让原数据暂时无法读取：commit 无法按磁盘重建状态。
	orig, err := os.ReadFile(dataFile(r.dir))
	if err != nil {
		t.Fatal(err)
	}
	blockBatchSave(t, r)
	if err := os.WriteFile(dataFile(r.dir), []byte("{corrupt"), fileMode); err != nil {
		t.Fatal(err)
	}

	res, err := r.SetRoyalty(req)
	assertRoyaltySaveFailureEmpty(t, res, err)
	// 状态未能重建时也必须显式撤销：原规则完整保留，变更记录仍只有首次
	// 设置的一条，序号与请求号未被消耗。
	assertRoyaltyRequestFree(t, r, "alice", "rr-2", 1, 1)
	assertCurrentRoyalty(t, r, "s1", RoyaltyShare{AccountID: "carol", Rate: 500})
	evs, err := r.RoyaltyHistory("s1")
	if err != nil || len(evs) != 1 || evs[0].Seq != 1 || evs[0].RequestID != "rr-1" {
		t.Fatalf("不可读磁盘失败后变更记录应保持操作前: %+v, %v", evs, err)
	}

	// 恢复正常读写。
	if err := os.WriteFile(dataFile(r.dir), orig, fileMode); err != nil {
		t.Fatal(err)
	}
	restoreBatchSave(t, r)

	// 其他操作成功保存不能把这次未保存的替换带进登记册。
	if err := r.RegisterAccount("erin", "路人"); err != nil {
		t.Fatalf("其他正常操作应能成功保存: %v", err)
	}
	assertRoyaltyRequestFree(t, r, "alice", "rr-2", 1, 1)
	assertCurrentRoyalty(t, r, "s1", RoyaltyShare{AccountID: "carol", Rate: 500})

	// 原请求重提首次成功，不标回放；变更序号接着首次设置递增。
	res, err = r.SetRoyalty(req)
	if err != nil || res.Replayed || res.SeriesID != "s1" ||
		len(res.Shares) != 1 || res.Shares[0] != (RoyaltyShare{AccountID: "dave", Rate: 800}) {
		t.Fatalf("恢复后重提应正常设置: %+v, err %v", res, err)
	}
	assertCurrentRoyalty(t, r, "s1", RoyaltyShare{AccountID: "dave", Rate: 800})
	evs, err = r.RoyaltyHistory("s1")
	if err != nil || len(evs) != 2 || evs[1].Seq != 2 || evs[1].RequestID != "rr-2" ||
		len(evs[1].Before) != 1 || evs[1].Before[0] != (RoyaltyShare{AccountID: "carol", Rate: 500}) {
		t.Fatalf("替换后的变更记录异常: %+v, %v", evs, err)
	}
}

// TestSetRoyaltyClearSaveFailureUnreadableDisk 覆盖：清空已有非空规则保存
// 失败且原数据暂时无法读取时，原规则完整保留；恢复后重提清空正常生效，
// 系列回到空规则。
func TestSetRoyaltyClearSaveFailureUnreadableDisk(t *testing.T) {
	r := mustCreate(t, tempDir(t))
	royaltyWorld(t, r)
	mustSetRoyalty(t, r, "rr-1",
		RoyaltyShare{AccountID: "carol", Rate: 500},
		RoyaltyShare{AccountID: "dave", Rate: 300})
	req := setRoyaltyReq("rr-clear") // 空份额：清空规则

	orig, err := os.ReadFile(dataFile(r.dir))
	if err != nil {
		t.Fatal(err)
	}
	blockBatchSave(t, r)
	if err := os.WriteFile(dataFile(r.dir), []byte("{corrupt"), fileMode); err != nil {
		t.Fatal(err)
	}

	res, err := r.SetRoyalty(req)
	assertRoyaltySaveFailureEmpty(t, res, err)
	assertRoyaltyRequestFree(t, r, "alice", "rr-clear", 1, 1)
	assertCurrentRoyalty(t, r, "s1",
		RoyaltyShare{AccountID: "carol", Rate: 500},
		RoyaltyShare{AccountID: "dave", Rate: 300})

	if err := os.WriteFile(dataFile(r.dir), orig, fileMode); err != nil {
		t.Fatal(err)
	}
	restoreBatchSave(t, r)

	res, err = r.SetRoyalty(req)
	if err != nil || res.Replayed || res.SeriesID != "s1" || len(res.Shares) != 0 {
		t.Fatalf("恢复后重提清空应正常生效: %+v, err %v", res, err)
	}
	assertCurrentRoyalty(t, r, "s1")
	evs, err := r.RoyaltyHistory("s1")
	if err != nil || len(evs) != 2 || evs[1].Seq != 2 || len(evs[1].Before) != 2 ||
		len(evs[1].After) != 0 {
		t.Fatalf("清空后的变更记录异常: %+v, %v", evs, err)
	}
}

// TestSetRoyaltyRejectSaveFailure 覆盖：状态类业务拒绝（系列封存）的拒绝
// 结果保存失败时，返回实际保存错误而非该业务错误，结果为空，请求号不被
// 这次未保存的拒绝占用；保存恢复后原请求重提重新判断并保存此次拒绝（首次
// 重提不标回放），只有拒绝保存完成后再次提交才回放。
func TestSetRoyaltyRejectSaveFailure(t *testing.T) {
	r := mustCreate(t, tempDir(t))
	royaltyWorld(t, r)
	if err := r.SealSeries("s1", "alice"); err != nil {
		t.Fatal(err)
	}
	req := setRoyaltyReq("rr-sealed", RoyaltyShare{AccountID: "carol", Rate: 500})

	blockBatchSave(t, r)
	res, err := r.SetRoyalty(req)
	assertRoyaltySaveFailureEmpty(t, res, err)
	assertRoyaltyRequestFree(t, r, "alice", "rr-sealed", 0, 0)
	assertCurrentRoyalty(t, r, "s1")

	// 保存条件恢复、拒绝条件仍在：原请求重提重新判断并保存此次拒绝，
	// 保存成功后才返回业务错误本身；这是一次全新判断，不能标回放。
	restoreBatchSave(t, r)
	res, err = r.SetRoyalty(req)
	if !errors.Is(err, ErrSeriesSealed) {
		t.Fatalf("重提应在保存成功后返回业务拒绝 ErrSeriesSealed: %v", err)
	}
	if res.Replayed || res.SeriesID != "s1" || !errors.Is(res.Err, ErrSeriesSealed) ||
		len(res.Shares) != 0 {
		t.Fatalf("重提结果异常: %+v", res)
	}
	if _, ok := r.state.Requests[requestKey("alice", "rr-sealed")]; !ok {
		t.Fatal("拒绝保存成功后应登记请求结果")
	}

	// 拒绝已保存：此后相同内容再次提交才回放该拒绝。
	res, err = r.SetRoyalty(req)
	if !errors.Is(err, ErrSeriesSealed) || !res.Replayed ||
		res.SeriesID != "s1" || !errors.Is(res.Err, ErrSeriesSealed) {
		t.Fatalf("保存后的再次提交应回放已保存的拒绝: %+v, err %v", res, err)
	}
	// 拒绝不改变规则，也不产生变更记录。
	assertCurrentRoyalty(t, r, "s1")
	assertRoyaltyRequestFree(t, r, "alice", "rr-never", 0, 0)
}

// TestSetRoyaltyRejectSaveFailureUnreadableDisk 覆盖：状态类业务拒绝（首次
// 发行后规则固定）的拒绝结果保存失败、且原数据同时暂时无法读取时，未保存
// 的拒绝不留在当前登记册；恢复后其他操作成功保存不夹带该拒绝，原请求重提
// 重新判断并保存拒绝。
func TestSetRoyaltyRejectSaveFailureUnreadableDisk(t *testing.T) {
	r := mustCreate(t, tempDir(t))
	royaltyWorld(t, r)
	// 首次成功发行后规则固定。
	if _, err := r.Issue(issueReq("i1", "bob")); err != nil {
		t.Fatal(err)
	}
	req := setRoyaltyReq("rr-frozen", RoyaltyShare{AccountID: "carol", Rate: 500})

	orig, err := os.ReadFile(dataFile(r.dir))
	if err != nil {
		t.Fatal(err)
	}
	blockBatchSave(t, r)
	if err := os.WriteFile(dataFile(r.dir), []byte("{corrupt"), fileMode); err != nil {
		t.Fatal(err)
	}

	res, err := r.SetRoyalty(req)
	assertRoyaltySaveFailureEmpty(t, res, err)
	assertRoyaltyRequestFree(t, r, "alice", "rr-frozen", 0, 0)
	assertCurrentRoyalty(t, r, "s1")

	if err := os.WriteFile(dataFile(r.dir), orig, fileMode); err != nil {
		t.Fatal(err)
	}
	restoreBatchSave(t, r)

	// 其他操作成功保存不能把这次未保存的拒绝带进登记册。
	if err := r.RegisterAccount("erin", "路人"); err != nil {
		t.Fatalf("其他正常操作应能成功保存: %v", err)
	}
	assertRoyaltyRequestFree(t, r, "alice", "rr-frozen", 0, 0)

	// 拒绝条件仍在：原请求重提重新保存并返回规则固定拒绝，不标回放。
	res, err = r.SetRoyalty(req)
	if !errors.Is(err, ErrRoyaltyFrozen) || res.Replayed ||
		res.SeriesID != "s1" || !errors.Is(res.Err, ErrRoyaltyFrozen) {
		t.Fatalf("重提应重新判断并保存规则固定拒绝: %+v, err %v", res, err)
	}
	res, err = r.SetRoyalty(req)
	if !errors.Is(err, ErrRoyaltyFrozen) || !res.Replayed {
		t.Fatalf("再次提交应回放已保存的拒绝: %+v, err %v", res, err)
	}
}

// TestSetRoyaltyValidationErrorIgnoresSaveFailure 覆盖：参数错误与引用不
// 存在属于无需保存的错误，即使存储暂时不可写，也仍返回原错误而不是保存
// 错误，并且不占用请求号、不消耗变更序号。
func TestSetRoyaltyValidationErrorIgnoresSaveFailure(t *testing.T) {
	r := mustCreate(t, tempDir(t))
	royaltyWorld(t, r)

	blockBatchSave(t, r)
	defer restoreBatchSave(t, r)

	// 比例不合法（参数错误）。
	badRate := setRoyaltyReq("rr-bad-rate", RoyaltyShare{AccountID: "carol", Rate: 0})
	if _, err := r.SetRoyalty(badRate); !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("比例不合法应返回 ErrInvalidArgument，不能改报保存错误: %v", err)
	}
	// 收款账户重复（参数错误）。
	dup := setRoyaltyReq("rr-dup",
		RoyaltyShare{AccountID: "carol", Rate: 100},
		RoyaltyShare{AccountID: "carol", Rate: 200})
	if _, err := r.SetRoyalty(dup); !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("账户重复应返回 ErrInvalidArgument: %v", err)
	}
	// 收款账户不存在（引用不存在）。
	ghost := setRoyaltyReq("rr-ghost", RoyaltyShare{AccountID: "ghost", Rate: 100})
	res, err := r.SetRoyalty(ghost)
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("收款账户不存在应返回 ErrNotFound，不能改报保存错误: %v", err)
	}
	if res.SeriesID != "s1" || !errors.Is(res.Err, ErrNotFound) {
		t.Fatalf("引用不存在的结果异常: %+v", res)
	}
	// 系列不存在（引用不存在）。
	noSeries := setRoyaltyReq("rr-no-series", RoyaltyShare{AccountID: "carol", Rate: 100})
	noSeries.SeriesID = "sX"
	if _, err := r.SetRoyalty(noSeries); !errors.Is(err, ErrNotFound) {
		t.Fatalf("系列不存在应返回 ErrNotFound: %v", err)
	}
	// 必填缺失。
	missing := setRoyaltyReq("rr-missing", RoyaltyShare{AccountID: "carol", Rate: 100})
	missing.Reason = ""
	if _, err := r.SetRoyalty(missing); !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("必填缺失应返回 ErrInvalidArgument: %v", err)
	}

	// 无需保存的错误一律不占用请求号，也不消耗变更序号。
	for _, rid := range []string{"rr-bad-rate", "rr-dup", "rr-ghost", "rr-no-series", "rr-missing"} {
		if _, ok := r.state.Requests[requestKey("alice", rid)]; ok {
			t.Fatalf("参数/对象错误不应占用请求号 %s", rid)
		}
	}
	if r.state.NextRoyaltySeq != 0 || len(r.state.RoyaltyEvents) != 0 {
		t.Fatalf("参数/对象错误不应消耗变更序号或产生变更记录: seq=%d events=%d",
			r.state.NextRoyaltySeq, len(r.state.RoyaltyEvents))
	}
	assertCurrentRoyalty(t, r, "s1")
}

// TestSetRoyaltySaveFailureAfterReopen 覆盖磁盘视角：保存失败后原登记册
// 仍可正常读取——关闭重开看到的仍是设置前状态，请求号未占用；恢复保存后
// 用原请求重提正常设置。
func TestSetRoyaltySaveFailureAfterReopen(t *testing.T) {
	dir := tempDir(t)
	r := mustCreate(t, dir)
	royaltyWorld(t, r)
	mustSetRoyalty(t, r, "rr-1", RoyaltyShare{AccountID: "carol", Rate: 500})
	req := setRoyaltyReq("rr-2", RoyaltyShare{AccountID: "dave", Rate: 800})

	blockBatchSave(t, r)
	res, err := r.SetRoyalty(req)
	assertRoyaltySaveFailureEmpty(t, res, err)

	if err := r.Close(); err != nil {
		t.Fatal(err)
	}
	r2, err := Open(dir)
	if err != nil {
		t.Fatalf("失败后原登记册必须仍能正常读取: %v", err)
	}
	t.Cleanup(func() { _ = r2.Close() })
	assertCurrentRoyalty(t, r2, "s1", RoyaltyShare{AccountID: "carol", Rate: 500})
	assertRoyaltyRequestFree(t, r2, "alice", "rr-2", 1, 1)

	restoreBatchSave(t, r2)
	res, err = r2.SetRoyalty(req)
	if err != nil || res.Replayed || res.SeriesID != "s1" ||
		len(res.Shares) != 1 || res.Shares[0] != (RoyaltyShare{AccountID: "dave", Rate: 800}) {
		t.Fatalf("重开后重提应正常设置: %+v, err %v", res, err)
	}
	res, err = r2.SetRoyalty(req)
	if err != nil || !res.Replayed {
		t.Fatalf("再次提交应回放已保存的结果: %+v, err %v", res, err)
	}
}
