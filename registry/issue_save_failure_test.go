package registry

import (
	"errors"
	"testing"
)

// 本文件为单件发行补充"拒绝结果保存失败"场景的回归保障：请求内容合法、
// 引用对象存在，但因现有状态规则（封存、停用、无权、编号已占用等）不能
// 发行，而保存这次拒绝又失败时，必须返回实际的保存错误而非业务拒绝；
// 结果整体为空（无藏品编号、无持有人、无版本、无历史序号、业务错误为空、
// 不标回放），请求号不被这次未保存的拒绝占用，不登记藏品、不建立持有、
// 不追加发行历史、不消耗历史序号，未首次发行的系列也不因此固定版税规则。
// 保存条件恢复后用原请求重提按当时状态重新判断与保存，而不是回放未保存
// 的拒绝。
//
// 失败注入复用 blockBatchSave/restoreBatchSave：在临时文件路径上预置目录，
// save 在 rename 替换 registry.json 之前确定性失败，原快照完好可读。

// assertIssueRejectSaveFailureResult 核对单件发行"拒绝结果保存失败"的
// 返回：error 是保存错误而非业务拒绝；结果为空——没有藏品编号、持有人、
// 版本、历史序号，不标回放，业务错误为空。
func assertSingleIssueRejectSaveFailureResult(t *testing.T, res IssueResult, err error) {
	t.Helper()
	assertSaveFailureError(t, err)
	if res.ItemID != "" || res.OwnerID != "" || res.Version != 0 || res.TxSeq != 0 ||
		res.Replayed || res.Err != nil {
		t.Fatalf("拒绝保存失败必须整体失败，结果应为空: %+v", res)
	}
}

// assertIssueWorldPristine 核对拒绝保存失败后世界仍是发行前的样子：藏品
// 不存在、没有持有与发行历史、历史序号未消耗、请求号未占用。
func assertIssueWorldPristine(t *testing.T, r *Registry, itemID, rid string) {
	t.Helper()
	if _, err := r.GetItem(itemID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("失败后 %s 不应被登记: %v", itemID, err)
	}
	if _, err := r.GetHolding(itemID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("失败后 %s 不应有持有: %v", itemID, err)
	}
	if _, err := r.History(itemID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("失败后 %s 不应有历史: %v", itemID, err)
	}
	if r.state.NextSeq != 0 {
		t.Fatalf("NextSeq = %d，失败不应消耗历史序号，仍应为 0", r.state.NextSeq)
	}
	if _, ok := r.state.Requests[requestKey("alice", rid)]; ok {
		t.Fatalf("未保存的拒绝不应占用请求号 %s", rid)
	}
}

// TestIssueRejectSaveFailureRetrySameState 覆盖题述核心场景：初始持有人
// 停用的拒绝落盘失败时返回保存错误、结果为空、请求号不被占用；保存恢复
// 后保持原请求重提，重新判断并重新保存该拒绝（这次不算回放），只有此后
// 相同内容再次提交才回放已保存的拒绝。
func TestIssueRejectSaveFailureRetrySameState(t *testing.T) {
	r := mustCreate(t, tempDir(t))
	setupWorld(t, r)
	if err := r.DeactivateAccount("bob"); err != nil {
		t.Fatal(err)
	}
	req := issueReq("i1", "bob")

	// 拒绝条件成立（初始持有人 bob 已停用），但保存这次拒绝失败。
	blockBatchSave(t, r)
	res, err := r.Issue(req)
	assertSingleIssueRejectSaveFailureResult(t, res, err)
	assertIssueWorldPristine(t, r, req.ItemID, req.RequestID)

	// 保存条件恢复、拒绝条件仍在：原请求重提重新判断并保存此次拒绝，
	// 保存成功后才返回业务错误本身；这是一次全新判断，不能标回放。
	restoreBatchSave(t, r)
	res, err = r.Issue(req)
	if !errors.Is(err, ErrAccountInactive) {
		t.Fatalf("重提应在保存成功后返回业务拒绝 ErrAccountInactive: %v", err)
	}
	if res.Replayed || res.ItemID != "i1" || !errors.Is(res.Err, ErrAccountInactive) ||
		res.OwnerID != "" || res.Version != 0 || res.TxSeq != 0 {
		t.Fatalf("重提结果异常: %+v", res)
	}
	if _, ok := r.state.Requests[requestKey(req.Operator, req.RequestID)]; !ok {
		t.Fatal("拒绝保存成功后应登记请求结果")
	}

	// 拒绝已保存：此后相同内容再次提交才回放该拒绝。
	res, err = r.Issue(req)
	if !errors.Is(err, ErrAccountInactive) || !res.Replayed || res.ItemID != "i1" ||
		!errors.Is(res.Err, ErrAccountInactive) {
		t.Fatalf("保存后的再次提交应回放已保存的拒绝: %+v, err %v", res, err)
	}

	// 系列从未成功发行，被拒（含那次未保存的失败）不固定版税规则，仍可设置。
	set := SetRoyaltyRequest{
		Operator: "alice", Reason: "拒绝后设置版税", RequestID: "rr-freeze-check",
		SeriesID: "s1", Shares: []RoyaltyShare{{AccountID: "alice", Rate: 500}},
	}
	if _, err := r.SetRoyalty(set); err != nil {
		t.Fatalf("未成功发行的系列不应固定版税规则: %v", err)
	}
}

// TestIssueRejectSaveFailureRetryAfterFix 覆盖：未保存的拒绝不占用请求
// 号——保存恢复后把停用的初始持有人换成已登记且可用的账户，用同一操作者
// 和请求号提交，应正常发行，不返回请求号冲突，序号从 1 开始。
func TestIssueRejectSaveFailureRetryAfterFix(t *testing.T) {
	r := mustCreate(t, tempDir(t))
	setupWorld(t, r)
	if err := r.RegisterAccount("carol", ""); err != nil {
		t.Fatal(err)
	}
	if err := r.DeactivateAccount("bob"); err != nil {
		t.Fatal(err)
	}
	req := issueReq("i1", "bob")

	blockBatchSave(t, r)
	res, err := r.Issue(req)
	assertSingleIssueRejectSaveFailureResult(t, res, err)

	// 恢复保存条件，把初始持有人改为可用账户 carol；同一操作者与请求号。
	restoreBatchSave(t, r)
	fixed := req
	fixed.HolderID = "carol"
	res, err = r.Issue(fixed)
	if err != nil {
		t.Fatalf("修正请求后用原请求号重提应正常发行，不能报请求号冲突: %v", err)
	}
	if res.Replayed || res.Err != nil || res.ItemID != "i1" || res.OwnerID != "carol" ||
		res.Version != 1 || res.TxSeq != 1 {
		t.Fatalf("重提结果异常: %+v", res)
	}
	if r.state.NextSeq != 1 {
		t.Fatalf("NextSeq = %d, want 1", r.state.NextSeq)
	}
	h, err := r.GetHolding("i1")
	if err != nil || h.OwnerID != "carol" || h.Version != 1 {
		t.Fatalf("i1 持有异常: %+v, err %v", h, err)
	}

	// 成功结果保存后，相同内容再提回放首次成功；停用的 bob 不影响回放。
	replay, err := r.Issue(fixed)
	if err != nil || !replay.Replayed || replay.OwnerID != "carol" || replay.TxSeq != 1 {
		t.Fatalf("成功后重提应回放: %+v, err %v", replay, err)
	}
}

// TestIssueRejectSaveFailureAfterReopen 覆盖磁盘视角：保存失败后原登记册
// 仍可正常读取——关闭重开看到的仍是拒绝前状态，请求号未占用；恢复保存后
// 用原请求重提重新保存拒绝。
func TestIssueRejectSaveFailureAfterReopen(t *testing.T) {
	dir := tempDir(t)
	r := mustCreate(t, dir)
	setupWorld(t, r)
	if err := r.SealSeries("s1", "alice"); err != nil {
		t.Fatal(err)
	}
	req := issueReq("i1", "bob")

	blockBatchSave(t, r)
	res, err := r.Issue(req)
	assertSingleIssueRejectSaveFailureResult(t, res, err)

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
	res, err = r2.Issue(req)
	if !errors.Is(err, ErrSeriesSealed) || res.Replayed || res.Err == nil {
		t.Fatalf("重开后重提应重新判断并保存封存拒绝: %+v, err %v", res, err)
	}
	// 再次提交才回放。
	res, err = r2.Issue(req)
	if !errors.Is(err, ErrSeriesSealed) || !res.Replayed {
		t.Fatalf("再次提交应回放已保存的拒绝: %+v, err %v", res, err)
	}
}

// TestIssueRejectSaveFailureOperatorError 覆盖操作者层面的拒绝（非创建
// 账户）保存失败时结果不附任何发行内容；恢复后重提重新保存并返回原错误。
func TestIssueRejectSaveFailureOperatorError(t *testing.T) {
	r := mustCreate(t, tempDir(t))
	setupWorld(t, r)
	// bob 不是系列 s1 的创建账户：ErrForbidden。
	req := issueReq("i1", "bob")
	req.Operator = "bob"

	blockBatchSave(t, r)
	res, err := r.Issue(req)
	assertSingleIssueRejectSaveFailureResult(t, res, err)
	if _, ok := r.state.Requests[requestKey("bob", req.RequestID)]; ok {
		t.Fatal("未保存的拒绝不应占用请求号")
	}

	restoreBatchSave(t, r)
	res, err = r.Issue(req)
	if !errors.Is(err, ErrForbidden) {
		t.Fatalf("重提应返回 ErrForbidden: %v", err)
	}
	if res.ItemID != "i1" || res.Replayed || !errors.Is(res.Err, ErrForbidden) {
		t.Fatalf("操作者层面错误的重提结果异常: %+v", res)
	}
	res, err = r.Issue(req)
	if !errors.Is(err, ErrForbidden) || !res.Replayed {
		t.Fatalf("再次提交应回放已保存的拒绝: %+v, err %v", res, err)
	}
}

// TestIssueRejectSaveFailureIDOccupied 覆盖：编号已占用的拒绝保存失败
// 后，历史序号不消耗、原有藏品与持有保持原状；恢复后同一请求重新保存
// 编号占用拒绝。
func TestIssueRejectSaveFailureIDOccupied(t *testing.T) {
	r := mustCreate(t, tempDir(t))
	setupWorld(t, r)
	// i1 已由一次成功的发行占用，归 bob 所有。
	if _, err := r.Issue(issueReq("i1", "bob")); err != nil {
		t.Fatal(err)
	}
	req := issueReq("i1", "bob")
	req.RequestID = "req-dup"

	blockBatchSave(t, r)
	res, err := r.Issue(req)
	assertSingleIssueRejectSaveFailureResult(t, res, err)
	if r.state.NextSeq != 1 {
		t.Fatalf("NextSeq = %d，失败不应消耗历史序号，仍应为 1", r.state.NextSeq)
	}
	// 原有藏品与持有记录保持原状。
	h, err := r.GetHolding("i1")
	if err != nil || h.OwnerID != "bob" || h.Version != 1 {
		t.Fatalf("原有持有不应被失败影响: %+v, err %v", h, err)
	}

	restoreBatchSave(t, r)
	res, err = r.Issue(req)
	if !errors.Is(err, ErrAlreadyExists) {
		t.Fatalf("重提应返回 ErrAlreadyExists: %v", err)
	}
	if res.Replayed || res.ItemID != "i1" || !errors.Is(res.Err, ErrAlreadyExists) {
		t.Fatalf("重提结果异常: %+v", res)
	}
	h, err = r.GetHolding("i1")
	if err != nil || h.OwnerID != "bob" || h.Version != 1 {
		t.Fatalf("拒绝后原有持有仍应保持原状: %+v, err %v", h, err)
	}
}

// TestIssueValidationErrorIgnoresSaveFailure 覆盖：必填缺失或引用不存在
// 属于无需保存的参数/对象错误，即使存储暂时不可写，也仍返回原错误而不是
// 保存错误，并且不占用请求号。
func TestIssueValidationErrorIgnoresSaveFailure(t *testing.T) {
	r := mustCreate(t, tempDir(t))
	setupWorld(t, r)

	blockBatchSave(t, r)
	defer restoreBatchSave(t, r)

	// 必填缺失（缺系列）。
	missing := issueReq("i1", "bob")
	missing.SeriesID = ""
	if _, err := r.Issue(missing); !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("必填缺失应返回 ErrInvalidArgument，不能改报保存错误: %v", err)
	}
	// 引用对象不存在：未登记的初始持有人。
	ghost := issueReq("i1", "ghost")
	res, err := r.Issue(ghost)
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("引用不存在应返回 ErrNotFound，不能改报保存错误: %v", err)
	}
	if res.ItemID != "i1" || !errors.Is(res.Err, ErrNotFound) {
		t.Fatalf("引用不存在的结果异常: %+v", res)
	}
	// 操作者未登记。
	opMissing := issueReq("i1", "bob")
	opMissing.Operator = "nobody"
	if _, err := r.Issue(opMissing); !errors.Is(err, ErrNotFound) {
		t.Fatalf("操作者不存在应返回 ErrNotFound: %v", err)
	}

	// 无需保存的错误一律不占用请求号，也不消耗历史序号。
	for _, rid := range []string{missing.RequestID, ghost.RequestID, opMissing.RequestID} {
		if _, ok := r.state.Requests[requestKey("alice", rid)]; ok {
			t.Fatalf("参数/对象错误不应占用请求号 %s", rid)
		}
	}
	if _, ok := r.state.Requests[requestKey("nobody", opMissing.RequestID)]; ok {
		t.Fatal("参数/对象错误不应占用请求号")
	}
	if r.state.NextSeq != 0 {
		t.Fatalf("NextSeq = %d, want 0", r.state.NextSeq)
	}
}

// TestIssueRejectSaveFailureNotCarriedByLaterCommit 覆盖：未保存的拒绝在
// 同一已打开登记册上必须彻底撤销——保存条件恢复后另一笔正常操作成功落盘
// 时，不能把那次拒绝一起写入。
func TestIssueRejectSaveFailureNotCarriedByLaterCommit(t *testing.T) {
	r := mustCreate(t, tempDir(t))
	setupWorld(t, r)
	if err := r.DeactivateAccount("bob"); err != nil {
		t.Fatal(err)
	}
	req := issueReq("i1", "bob")

	blockBatchSave(t, r)
	if _, err := r.Issue(req); err == nil {
		t.Fatal("预期保存失败")
	}

	// 保存条件恢复，另一笔完全无关的正常操作成功落盘。
	restoreBatchSave(t, r)
	if err := r.RegisterAccount("carol", ""); err != nil {
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
	res, err := r.Issue(req)
	if !errors.Is(err, ErrAccountInactive) || res.Replayed {
		t.Fatalf("原请求号应仍可使用并重新判断: %+v, err %v", res, err)
	}
}
