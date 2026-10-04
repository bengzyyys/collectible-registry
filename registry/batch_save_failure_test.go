package registry

import (
	"errors"
	"testing"
)

// 本文件为整批发行补充"拒绝结果保存失败"场景的回归保障：请求内容合法、
// 引用对象存在，但因现有状态规则（停用、封存、无权、编号已占用等）整批
// 不能发行，而保存这次拒绝又失败时，必须返回实际的保存错误而非业务拒绝；
// 结果整体为空（无发行条目、无失败藏品编号、业务错误为空、不标回放），
// 请求号不被这次未保存的拒绝占用，任何藏品都不新增登记、持有或发行历史，
// 历史序号不被消耗，未首次发行的系列也不因此固定版税规则。保存条件恢复
// 后用原请求重提按当时状态重新判断与保存，而不是回放未保存的拒绝。
//
// 失败注入复用 blockBatchSave/restoreBatchSave：在临时文件路径上预置目录，
// save 在 rename 替换 registry.json 之前确定性失败，原快照完好可读。

// rejectIssueReq 是各用例共用的整批发行请求：i1 给 bob、i2 给 carol。
func rejectIssueReq(rid string) IssueBatchRequest {
	return batchReq(rid, be("i1", "bob"), be("i2", "carol"))
}

// assertIssueRejectSaveFailureResult 核对"拒绝结果保存失败"的返回：error
// 是保存错误而非业务拒绝；结果为空——没有发行条目、没有失败藏品编号、
// 不标回放、业务错误为空。
func assertIssueRejectSaveFailureResult(t *testing.T, res IssueBatchResult, err error) {
	t.Helper()
	assertSaveFailureError(t, err)
	if len(res.Items) != 0 || res.ItemID != "" || res.Replayed || res.Err != nil {
		t.Fatalf("拒绝保存失败必须整体失败，结果应为空: %+v", res)
	}
}

// assertIssueBatchWorldPristine 核对拒绝保存失败后世界仍是发行前的样子：
// 清单内藏品都不存在、没有任何发行历史、历史序号未消耗、请求号未占用，
// 未首次发行的系列版税规则仍可设置。
func assertIssueBatchWorldPristine(t *testing.T, r *Registry, rid string) {
	t.Helper()
	for _, id := range []string{"i1", "i2"} {
		if _, err := r.GetItem(id); !errors.Is(err, ErrNotFound) {
			t.Fatalf("失败后 %s 不应被登记: %v", id, err)
		}
		if _, err := r.GetHolding(id); !errors.Is(err, ErrNotFound) {
			t.Fatalf("失败后 %s 不应有持有: %v", id, err)
		}
		if _, err := r.History(id); !errors.Is(err, ErrNotFound) {
			t.Fatalf("失败后 %s 不应有历史: %v", id, err)
		}
	}
	if r.state.NextSeq != 0 {
		t.Fatalf("NextSeq = %d，失败不应消耗历史序号，仍应为 0", r.state.NextSeq)
	}
	if _, ok := r.state.Requests[requestKey("alice", rid)]; ok {
		t.Fatalf("未保存的拒绝不应占用请求号 %s", rid)
	}
	// 未首次发行的系列不能因此固定版税规则：系列中不能出现任何藏品。
	for _, it := range r.state.Items {
		if it.SeriesID == "s1" {
			t.Fatalf("失败后系列 s1 不应有藏品被登记: %+v", it)
		}
	}
}

// TestIssueBatchRejectSaveFailureRetrySameState 覆盖题述核心场景：初始
// 持有人停用的拒绝落盘失败时返回保存错误、结果为空、请求号不被占用；
// 保存恢复后保持原清单重提，重新判断并重新保存该拒绝（这次不算回放），
// 只有此后相同内容再次提交才回放已保存的拒绝。
func TestIssueBatchRejectSaveFailureRetrySameState(t *testing.T) {
	r := mustCreate(t, tempDir(t))
	setupBatchWorld(t, r)
	if err := r.DeactivateAccount("carol"); err != nil {
		t.Fatal(err)
	}
	req := rejectIssueReq("rb-rej")

	// 拒绝条件成立（i2 的初始持有人 carol 已停用），但保存这次拒绝失败。
	blockBatchSave(t, r)
	res, err := r.IssueBatch(req)
	assertIssueRejectSaveFailureResult(t, res, err)
	assertIssueBatchWorldPristine(t, r, req.RequestID)

	// 保存条件恢复、拒绝条件仍在：原清单重提重新判断并保存此次拒绝，
	// 保存成功后才返回业务错误本身；这是一次全新判断，不能标回放。
	restoreBatchSave(t, r)
	res, err = r.IssueBatch(req)
	if !errors.Is(err, ErrAccountInactive) {
		t.Fatalf("重提应在保存成功后返回业务拒绝 ErrAccountInactive: %v", err)
	}
	if res.Replayed || res.ItemID != "i2" || !errors.Is(res.Err, ErrAccountInactive) ||
		len(res.Items) != 0 {
		t.Fatalf("重提结果异常: %+v", res)
	}
	if _, ok := r.state.Requests[requestKey(req.Operator, req.RequestID)]; !ok {
		t.Fatal("拒绝保存成功后应登记请求结果")
	}

	// 拒绝已保存：此后相同内容再次提交才回放该拒绝。
	res, err = r.IssueBatch(req)
	if !errors.Is(err, ErrAccountInactive) || !res.Replayed || res.ItemID != "i2" ||
		!errors.Is(res.Err, ErrAccountInactive) {
		t.Fatalf("保存后的再次提交应回放已保存的拒绝: %+v, err %v", res, err)
	}

	// 系列从未成功发行，被拒（含那次未保存的失败）不固定版税规则，仍可设置。
	set := SetRoyaltyRequest{
		Operator: "alice", Reason: "拒绝后设置版税", RequestID: "rr-freeze-check",
		SeriesID: "s1", Shares: []RoyaltyShare{{AccountID: "bob", Rate: 500}},
	}
	if _, err := r.SetRoyalty(set); err != nil {
		t.Fatalf("未成功发行的系列不应固定版税规则: %v", err)
	}
}

// TestIssueBatchRejectSaveFailureRetryAfterFix 覆盖：未保存的拒绝不占用
// 请求号——保存恢复后把停用持有人那一件改为已登记且可用的账户，用同一
// 操作者和请求号提交，应按修改后的清单正常发行，不返回请求号冲突，序号
// 从 1 连续开始。
func TestIssueBatchRejectSaveFailureRetryAfterFix(t *testing.T) {
	r := mustCreate(t, tempDir(t))
	setupBatchWorld(t, r)
	if err := r.RegisterAccount("dave", ""); err != nil {
		t.Fatal(err)
	}
	if err := r.DeactivateAccount("carol"); err != nil {
		t.Fatal(err)
	}
	req := rejectIssueReq("rb-rej-fix")

	blockBatchSave(t, r)
	res, err := r.IssueBatch(req)
	assertIssueRejectSaveFailureResult(t, res, err)

	// 恢复保存条件，把 i2 的初始持有人改为可用账户 dave；同一操作者与请求号。
	restoreBatchSave(t, r)
	fixed := req
	fixed.Entries = []IssueBatchEntry{be("i1", "bob"), be("i2", "dave")}
	res, err = r.IssueBatch(fixed)
	if err != nil {
		t.Fatalf("修改清单后用原请求号重提应正常发行，不能报请求号冲突: %v", err)
	}
	if res.Replayed || res.ItemID != "" || res.Err != nil || len(res.Items) != 2 {
		t.Fatalf("重提结果异常: %+v", res)
	}
	if res.Items[0].ItemID != "i1" || res.Items[0].OwnerID != "bob" ||
		res.Items[0].Version != 1 || res.Items[0].TxSeq != 1 {
		t.Fatalf("第一件发行结果异常: %+v", res.Items[0])
	}
	if res.Items[1].ItemID != "i2" || res.Items[1].OwnerID != "dave" ||
		res.Items[1].Version != 1 || res.Items[1].TxSeq != 2 {
		t.Fatalf("第二件发行结果异常: %+v", res.Items[1])
	}
	if r.state.NextSeq != 2 {
		t.Fatalf("NextSeq = %d, want 2", r.state.NextSeq)
	}
	for _, want := range []struct{ item, owner string }{
		{"i1", "bob"}, {"i2", "dave"},
	} {
		h, err := r.GetHolding(want.item)
		if err != nil || h.OwnerID != want.owner || h.Version != 1 {
			t.Fatalf("%s 持有异常: %+v, err %v", want.item, h, err)
		}
	}

	// 成功结果保存后，相同内容再提回放首次成功；停用的 carol 与已封存都
	// 不影响回放内容。
	replay, err := r.IssueBatch(fixed)
	if err != nil || !replay.Replayed || len(replay.Items) != 2 {
		t.Fatalf("成功后重提应回放: %+v, err %v", replay, err)
	}
}

// TestIssueBatchRejectSaveFailureAfterReopen 覆盖磁盘视角：保存失败后原
// 登记册仍可正常读取——关闭重开看到的仍是拒绝前状态，请求号未占用；
// 恢复保存后用原请求重提重新保存拒绝。
func TestIssueBatchRejectSaveFailureAfterReopen(t *testing.T) {
	dir := tempDir(t)
	r := mustCreate(t, dir)
	setupBatchWorld(t, r)
	if err := r.SealSeries("s1", "alice"); err != nil {
		t.Fatal(err)
	}
	req := batchReq("rb-rej-reopen", be("i1", "bob"))

	blockBatchSave(t, r)
	res, err := r.IssueBatch(req)
	assertIssueRejectSaveFailureResult(t, res, err)

	if err := r.Close(); err != nil {
		t.Fatal(err)
	}
	r2, err := Open(dir)
	if err != nil {
		t.Fatalf("失败后原登记册必须仍能正常读取: %v", err)
	}
	t.Cleanup(func() { _ = r2.Close() })
	if _, err := r2.GetItem("i1"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("重开后 i1 不应存在: %v", err)
	}
	if _, ok := r2.state.Requests[requestKey(req.Operator, req.RequestID)]; ok {
		t.Fatal("重开后未保存的拒绝不应占用请求号")
	}

	restoreBatchSave(t, r2)
	res, err = r2.IssueBatch(req)
	if !errors.Is(err, ErrSeriesSealed) || res.Replayed || res.Err == nil {
		t.Fatalf("重开后重提应重新判断并保存封存拒绝: %+v, err %v", res, err)
	}
	// 再次提交才回放。
	res, err = r2.IssueBatch(req)
	if !errors.Is(err, ErrSeriesSealed) || !res.Replayed {
		t.Fatalf("再次提交应回放已保存的拒绝: %+v, err %v", res, err)
	}
}

// TestIssueBatchRejectSaveFailureOperatorError 覆盖操作者层面的拒绝（无权
// 或停用）保存失败时结果不附藏品编号；恢复后重提重新保存并返回原错误。
func TestIssueBatchRejectSaveFailureOperatorError(t *testing.T) {
	r := mustCreate(t, tempDir(t))
	setupBatchWorld(t, r)
	// bob 不是系列 s1 的创建账户：ErrForbidden，与具体藏品无关。
	req := batchReq("rb-rej-op", be("i1", "bob"))
	req.Operator = "bob"

	blockBatchSave(t, r)
	res, err := r.IssueBatch(req)
	assertIssueRejectSaveFailureResult(t, res, err)
	if _, ok := r.state.Requests[requestKey("bob", req.RequestID)]; ok {
		t.Fatal("未保存的拒绝不应占用请求号")
	}

	restoreBatchSave(t, r)
	res, err = r.IssueBatch(req)
	if !errors.Is(err, ErrForbidden) {
		t.Fatalf("重提应返回 ErrForbidden: %v", err)
	}
	if res.ItemID != "" || res.Replayed || !errors.Is(res.Err, ErrForbidden) {
		t.Fatalf("操作者层面错误的重提结果异常: %+v", res)
	}
	res, err = r.IssueBatch(req)
	if !errors.Is(err, ErrForbidden) || !res.Replayed {
		t.Fatalf("再次提交应回放已保存的拒绝: %+v, err %v", res, err)
	}
}

// TestIssueBatchRejectSaveFailureIDOccupied 覆盖：编号已占用的拒绝保存
// 失败后，未占用的编号不被这次失败占用、历史序号不消耗；恢复后同一请求
// 重新保存编号占用拒绝，而清单中原本空闲的编号此刻仍可正常用于另一请求。
func TestIssueBatchRejectSaveFailureIDOccupied(t *testing.T) {
	r := mustCreate(t, tempDir(t))
	setupBatchWorld(t, r)
	// i2 已由一次成功的单件发行占用。
	if _, err := r.Issue(issueReq("i2", "bob")); err != nil {
		t.Fatal(err)
	}
	req := rejectIssueReq("rb-rej-exists")

	blockBatchSave(t, r)
	res, err := r.IssueBatch(req)
	assertIssueRejectSaveFailureResult(t, res, err)
	if r.state.NextSeq != 1 {
		t.Fatalf("NextSeq = %d，失败不应消耗历史序号，仍应为 1", r.state.NextSeq)
	}
	if _, err := r.GetItem("i1"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("失败后 i1 不应被登记: %v", err)
	}

	restoreBatchSave(t, r)
	res, err = r.IssueBatch(req)
	if !errors.Is(err, ErrAlreadyExists) {
		t.Fatalf("重提应返回 ErrAlreadyExists: %v", err)
	}
	if res.Replayed || res.ItemID != "i2" || !errors.Is(res.Err, ErrAlreadyExists) {
		t.Fatalf("重提结果异常: %+v", res)
	}

	// 原清单中未占用的 i1 没有被失败或后来的拒绝占用，可换另一个请求发行。
	ok := batchReq("rb-i1-now", be("i1", "bob"))
	if _, err := r.IssueBatch(ok); err != nil {
		t.Fatalf("未占用编号应仍可发行: %v", err)
	}
	if r.state.NextSeq != 2 {
		t.Fatalf("NextSeq = %d, want 2（原 i2 占 1，新发 i1 占 2）", r.state.NextSeq)
	}
}

// TestIssueBatchValidationErrorIgnoresSaveFailure 覆盖：空清单、重复编号、
// 必填缺失或引用不存在属于无需保存的参数/对象错误，即使存储暂时不可写，
// 也仍返回原错误而不是保存错误，并且不占用请求号。
func TestIssueBatchValidationErrorIgnoresSaveFailure(t *testing.T) {
	r := mustCreate(t, tempDir(t))
	setupBatchWorld(t, r)

	blockBatchSave(t, r)
	defer restoreBatchSave(t, r)

	// 空清单。
	if _, err := r.IssueBatch(batchReq("rb-empty")); !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("空清单应返回 ErrInvalidArgument，不能改报保存错误: %v", err)
	}
	// 清单内编号重复。
	if _, err := r.IssueBatch(batchReq("rb-dup", be("i1", "bob"), be("i1", "carol"))); !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("重复编号应返回 ErrInvalidArgument，不能改报保存错误: %v", err)
	}
	// 必填缺失（缺系列）。
	missing := IssueBatchRequest{
		Operator: "alice", Reason: "r", RequestID: "rb-missing", BatchNo: "b1",
		Entries: []IssueBatchEntry{be("i1", "bob")},
	}
	if _, err := r.IssueBatch(missing); !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("必填缺失应返回 ErrInvalidArgument，不能改报保存错误: %v", err)
	}
	// 引用对象不存在：未登记的初始持有人，结果指出该件。
	res, err := r.IssueBatch(batchReq("rb-nf", be("i1", "bob"), be("i2", "ghost")))
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("引用不存在应返回 ErrNotFound，不能改报保存错误: %v", err)
	}
	if res.ItemID != "i2" || !errors.Is(res.Err, ErrNotFound) {
		t.Fatalf("引用不存在的结果异常: %+v", res)
	}
	// 操作者未登记。
	opMissing := batchReq("rb-nf-op", be("i1", "bob"))
	opMissing.Operator = "nobody"
	if _, err := r.IssueBatch(opMissing); !errors.Is(err, ErrNotFound) {
		t.Fatalf("操作者不存在应返回 ErrNotFound: %v", err)
	}

	// 无需保存的错误一律不占用请求号，也不消耗历史序号。
	for _, rid := range []string{"rb-empty", "rb-dup", "rb-missing", "rb-nf", "rb-nf-op"} {
		if _, ok := r.state.Requests[requestKey("alice", rid)]; ok {
			t.Fatalf("参数/对象错误不应占用请求号 %s", rid)
		}
	}
	if r.state.NextSeq != 0 {
		t.Fatalf("NextSeq = %d, want 0", r.state.NextSeq)
	}
}

// TestIssueBatchRejectSaveFailureNotCarriedByLaterCommit 覆盖：未保存的
// 拒绝在同一已打开登记册上必须彻底撤销——保存条件恢复后另一笔正常操作
// 成功落盘时，不能把那次拒绝一起写入。
func TestIssueBatchRejectSaveFailureNotCarriedByLaterCommit(t *testing.T) {
	r := mustCreate(t, tempDir(t))
	setupBatchWorld(t, r)
	if err := r.DeactivateAccount("carol"); err != nil {
		t.Fatal(err)
	}
	req := rejectIssueReq("rb-rej-carry")

	blockBatchSave(t, r)
	if _, err := r.IssueBatch(req); err == nil {
		t.Fatal("预期保存失败")
	}

	// 保存条件恢复，另一笔完全无关的正常操作成功落盘。
	restoreBatchSave(t, r)
	other := batchReq("rb-other", be("i9", "bob"))
	if _, err := r.IssueBatch(other); err != nil {
		t.Fatalf("其他正常操作应能成功保存: %v", err)
	}

	// 成功落盘的快照中不能夹带那次未保存的拒绝。
	if _, ok := r.state.Requests[requestKey(req.Operator, req.RequestID)]; ok {
		t.Fatal("后续成功保存不能把未保存的拒绝一起写入")
	}
	if _, err := r.GetItem("i1"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("i1 不应随其他操作被一起写入: %v", err)
	}

	// 原请求号此刻仍可自由使用：状态依旧拒绝时重新保存，成功后才占号。
	res, err := r.IssueBatch(req)
	if !errors.Is(err, ErrAccountInactive) || res.Replayed {
		t.Fatalf("原请求号应仍可使用并重新判断: %+v, err %v", res, err)
	}
}
