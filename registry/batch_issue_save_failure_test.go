package registry

import (
	"errors"
	"testing"
)

// 本文件为整批发行补充"状态类拒绝结果保存失败"场景的回归保障：清单合法、
// 引用对象都存在，但因现有状态规则不能发行（初始持有人或操作者停用、系列
// 封存、藏品编号已占用等），而这次拒绝结果又落盘失败时——
//
//   - 返回的必须是保存错误（保留实际写入错误），不能再把业务拒绝作为本次
//     返回错误；结果整体为空：无发行结果清单、无失败藏品编号、结果中的
//     业务错误为空、不标记为重复回放；
//   - 这次没有保存的拒绝不占用操作者的请求号：同一个已打开的登记册上不
//     留下拒绝记录，后续其他正常操作成功保存时也不会把它一起写入；
//   - 整批中的任何藏品都不新增登记、持有或发行历史，原有藏品保持原状，
//     未使用的藏品编号不被占用，历史序号不被消耗，尚未首次发行的系列也
//     不因此固定版税规则；
//   - 保存条件恢复后用原请求号重提，按当时业务状态重新判断：拒绝条件仍在
//     则重新保存拒绝（此次不算回放），此后再重提才回放；状态或清单已变为
//     可发行时则整批正常发行，不因先前未保存的内容发生请求号冲突。
//
// 失败注入与整批转让保存失败测试相同：在临时文件路径上预先建一个目录，
// save 在 OpenFile 阶段即以 EISDIR 失败（rename 之前，原快照完好可读），
// 删除该目录即恢复保存条件；不依赖文件权限，root 下同样生效。

// assertIssueRejectSaveFailureResult 核对"拒绝结果保存失败"的返回：error
// 是保存错误而非业务拒绝，结果整体为空——没有发行结果、没有失败藏品编号、
// 不标回放、业务错误为空。
func assertIssueRejectSaveFailureResult(t *testing.T, res IssueBatchResult, err error) {
	t.Helper()
	assertSaveFailureError(t, err)
	if len(res.Items) != 0 || res.ItemID != "" || res.Replayed || res.Err != nil {
		t.Fatalf("拒绝保存失败必须整体失败，结果应为空: %+v", res)
	}
}

// assertIssueRequestFree 核对整批请求号未被占用、历史序号未被消耗。
func assertIssueRequestFree(t *testing.T, r *Registry, operator, requestID string, wantNextSeq int64) {
	t.Helper()
	if rec, ok := r.state.Requests[requestKey(operator, requestID)]; ok {
		t.Fatalf("未保存的拒绝不应占用请求号 %s，却存在记录: %+v", requestID, rec)
	}
	if r.state.NextSeq != wantNextSeq {
		t.Fatalf("NextSeq = %d，保存失败不应消耗历史序号，want %d", r.state.NextSeq, wantNextSeq)
	}
}

// assertItemsNeverIssued 核对给定藏品编号都没有因失败产生登记或持有。
func assertItemsNeverIssued(t *testing.T, r *Registry, ids ...string) {
	t.Helper()
	for _, id := range ids {
		if _, err := r.GetItem(id); !errors.Is(err, ErrNotFound) {
			t.Fatalf("失败后藏品 %s 不应被登记: GetItem err = %v", id, err)
		}
		if _, err := r.GetHolding(id); !errors.Is(err, ErrNotFound) {
			t.Fatalf("失败后藏品 %s 不应存在持有: GetHolding err = %v", id, err)
		}
	}
}

// TestIssueBatchRejectSaveFailureRetryAfterStateChange 覆盖任务给出的核心
// 例子：清单只有一件的初始持有人已停用、其余条件均满足；拒绝保存失败后
// 恢复保存条件，把该件初始持有人改为另一个已登记且可用的账户，用同一操作
// 者和同一请求号提交，应按修改后的清单正常发行，不因先前未保存的内容返回
// 请求号冲突。
func TestIssueBatchRejectSaveFailureRetryAfterStateChange(t *testing.T) {
	r := mustCreate(t, tempDir(t))
	setupBatchWorld(t, r)
	if err := r.DeactivateAccount("bob"); err != nil {
		t.Fatal(err)
	}
	// i1 的初始持有人 bob 已停用，i2 的持有人 carol 正常；其余条件均满足。
	req := batchReq("rb-rej", be("i1", "bob"), be("i2", "carol"))

	blockBatchSave(t, r)
	res, err := r.IssueBatch(req)
	assertIssueRejectSaveFailureResult(t, res, err)
	assertIssueRequestFree(t, r, req.Operator, req.RequestID, 0)
	assertItemsNeverIssued(t, r, "i1", "i2")

	// 恢复保存条件。尚未首次发行的系列不能因这次失败固定版税规则：此时仍可
	// 设置规则（失败期间没有任何藏品落盘）。
	restoreBatchSave(t, r)
	if _, err := r.SetRoyalty(SetRoyaltyRequest{
		Operator: "alice", Reason: "首发前设置版税", RequestID: "rr1",
		SeriesID: "s1", Shares: []RoyaltyShare{{AccountID: "carol", Rate: 1000}},
	}); err != nil {
		t.Fatalf("未成功发行前版税规则不应被固定: %v", err)
	}

	// 再进行一笔与本次请求无关的正常登记操作：它成功保存时不能把先前未
	// 保存的拒绝一起写入。
	if err := r.RegisterAccount("dave", ""); err != nil {
		t.Fatal(err)
	}
	if _, ok := r.state.Requests[requestKey(req.Operator, req.RequestID)]; ok {
		t.Fatal("其他正常操作保存后，仍不应出现先前未保存的拒绝记录")
	}

	// 把 i1 的初始持有人改为已登记且可用的 dave，操作者与请求号不变。
	modified := batchReq("rb-rej", be("i1", "dave"), be("i2", "carol"))
	res, err = r.IssueBatch(modified)
	if err != nil {
		t.Fatalf("按修改后的清单用原请求号重提应正常发行，不能报请求号冲突: %v", err)
	}
	if res.Replayed || res.ItemID != "" || res.Err != nil || len(res.Items) != 2 {
		t.Fatalf("修改后清单的发行结果异常: %+v", res)
	}
	if res.Items[0].ItemID != "i1" || res.Items[0].OwnerID != "dave" ||
		res.Items[0].Version != 1 || res.Items[0].TxSeq != 1 {
		t.Fatalf("第一件发行结果异常: %+v", res.Items[0])
	}
	if res.Items[1].ItemID != "i2" || res.Items[1].OwnerID != "carol" ||
		res.Items[1].Version != 1 || res.Items[1].TxSeq != 2 {
		t.Fatalf("第二件发行结果异常: %+v", res.Items[1])
	}

	// 登记、持有与发行历史按修改后的清单落盘，历史序号连续。
	for _, want := range []struct {
		item, owner string
		seq         int64
	}{{"i1", "dave", 1}, {"i2", "carol", 2}} {
		h, err := r.GetHolding(want.item)
		if err != nil {
			t.Fatalf("GetHolding %s: %v", want.item, err)
		}
		if h.OwnerID != want.owner || h.Version != 1 {
			t.Fatalf("%s 持有异常: %+v", want.item, h)
		}
		hist, err := r.History(want.item)
		if err != nil {
			t.Fatalf("History %s: %v", want.item, err)
		}
		if len(hist) != 1 || hist[0].Kind != "issue" || hist[0].Seq != want.seq ||
			hist[0].RequestID != "rb-rej" || hist[0].ToID != want.owner {
			t.Fatalf("%s 发行历史异常: %+v", want.item, hist)
		}
	}
	if r.state.NextSeq != 2 {
		t.Fatalf("NextSeq = %d, want 2", r.state.NextSeq)
	}
	// 请求号下保存的是本次成功结果而不是先前那次拒绝。
	rec := r.state.Requests[requestKey(req.Operator, req.RequestID)]
	if rec.Rejected || len(rec.Batch) != 2 {
		t.Fatalf("请求号下应为成功整批结果: %+v", rec)
	}

	// 相同（修改后）内容重提回放首次成功，不再发行第二次。
	replay, err := r.IssueBatch(modified)
	if err != nil || !replay.Replayed || len(replay.Items) != 2 {
		t.Fatalf("成功后重提应回放: %+v, err %v", replay, err)
	}
	if r.state.NextSeq != 2 {
		t.Fatalf("回放不应消耗历史序号: NextSeq = %d", r.state.NextSeq)
	}
	// 此时再用原（未修改）清单搭配同一请求号：成功结果已保存，参数不同按
	// 请求号冲突拒绝——冲突来自已保存的成功，而非那次未保存的拒绝。
	if _, err := r.IssueBatch(req); !errors.Is(err, ErrRequestConflict) {
		t.Fatalf("已保存成功后改动参数应返回 ErrRequestConflict: %v", err)
	}
	// 首次成功发行已落盘，系列版税规则随之固定。
	if _, err := r.SetRoyalty(SetRoyaltyRequest{
		Operator: "alice", Reason: "首发后再设", RequestID: "rr2",
		SeriesID: "s1", Shares: []RoyaltyShare{{AccountID: "carol", Rate: 2000}},
	}); !errors.Is(err, ErrRoyaltyFrozen) {
		t.Fatalf("成功首发后版税规则应已固定: %v", err)
	}
}

// TestIssueBatchRejectSaveFailureRetrySameState 覆盖：保持原清单（初始持有
// 人仍停用）重提，保存条件恢复后重新判断并保存账户停用的拒绝，这次返回
// 业务错误且不算回放；只有此后相同内容再次提交，才回放已保存的拒绝。
func TestIssueBatchRejectSaveFailureRetrySameState(t *testing.T) {
	r := mustCreate(t, tempDir(t))
	setupBatchWorld(t, r)
	if err := r.DeactivateAccount("bob"); err != nil {
		t.Fatal(err)
	}
	req := batchReq("rb-rej", be("i1", "bob"), be("i2", "carol"))

	blockBatchSave(t, r)
	res, err := r.IssueBatch(req)
	assertIssueRejectSaveFailureResult(t, res, err)
	assertIssueRequestFree(t, r, req.Operator, req.RequestID, 0)
	assertItemsNeverIssued(t, r, "i1", "i2")

	// 保存条件恢复、拒绝条件仍在：重提重新保存此次拒绝，保存成功后才返回
	// 业务错误本身，并指出清单中最前的失败藏品；此次不是回放。
	restoreBatchSave(t, r)
	res, err = r.IssueBatch(req)
	if !errors.Is(err, ErrAccountInactive) {
		t.Fatalf("重提应在保存成功后返回业务拒绝 ErrAccountInactive: %v", err)
	}
	if res.Replayed || res.ItemID != "i1" || !errors.Is(res.Err, ErrAccountInactive) ||
		len(res.Items) != 0 {
		t.Fatalf("重新保存拒绝的结果异常: %+v", res)
	}
	rec, ok := r.state.Requests[requestKey(req.Operator, req.RequestID)]
	if !ok || !rec.Rejected || rec.Reason != errCode(ErrAccountInactive) || rec.ItemID != "i1" {
		t.Fatalf("拒绝保存成功后应登记拒绝结果: %+v ok=%v", rec, ok)
	}
	assertItemsNeverIssued(t, r, "i1", "i2")
	if r.state.NextSeq != 0 {
		t.Fatalf("拒绝不应消耗历史序号: NextSeq = %d", r.state.NextSeq)
	}

	// 拒绝已保存：相同内容再次提交回放原拒绝（即使账户之后恢复也不重判）。
	res, err = r.IssueBatch(req)
	if !errors.Is(err, ErrAccountInactive) || !res.Replayed ||
		res.ItemID != "i1" || !errors.Is(res.Err, ErrAccountInactive) {
		t.Fatalf("保存后的再次提交应回放原拒绝: %+v, err %v", res, err)
	}
	assertItemsNeverIssued(t, r, "i1", "i2")
}

// TestIssueBatchRejectSaveFailureBizReasons 逐一覆盖系列封存与编号已占用这
// 两类状态拒绝：拒绝落盘失败时同样返回保存错误、结果为空、不占用请求号；
// 恢复后重提重新保存对应业务拒绝，再重提才回放。
func TestIssueBatchRejectSaveFailureBizReasons(t *testing.T) {
	t.Run("series_sealed", func(t *testing.T) {
		r := mustCreate(t, tempDir(t))
		setupBatchWorld(t, r)
		if err := r.SealSeries("s1", "alice"); err != nil {
			t.Fatal(err)
		}
		// 系列层面的拒绝不关联具体藏品编号。
		req := batchReq("rb-seal", be("i1", "bob"))

		blockBatchSave(t, r)
		res, err := r.IssueBatch(req)
		assertIssueRejectSaveFailureResult(t, res, err)
		assertIssueRequestFree(t, r, req.Operator, req.RequestID, 0)
		assertItemsNeverIssued(t, r, "i1")

		restoreBatchSave(t, r)
		res, err = r.IssueBatch(req)
		if !errors.Is(err, ErrSeriesSealed) {
			t.Fatalf("重提应返回 ErrSeriesSealed: %v", err)
		}
		if res.Replayed || res.ItemID != "" || !errors.Is(res.Err, ErrSeriesSealed) {
			t.Fatalf("重新保存封存拒绝的结果异常: %+v", res)
		}
		res, err = r.IssueBatch(req)
		if !errors.Is(err, ErrSeriesSealed) || !res.Replayed || res.ItemID != "" {
			t.Fatalf("再次提交应回放封存拒绝: %+v, err %v", res, err)
		}
		assertItemsNeverIssued(t, r, "i1")
	})

	t.Run("item_already_exists", func(t *testing.T) {
		r := mustCreate(t, tempDir(t))
		setupBatchWorld(t, r)
		// 先成功发行 i1（历史序号 1）占用编号，原有藏品由 bob 持有。
		if _, err := r.Issue(IssueRequest{
			Operator: "alice", Reason: "先占用编号", RequestID: "ri1",
			ItemID: "i1", SeriesID: "s1", BatchNo: "b0", HolderID: "bob",
		}); err != nil {
			t.Fatal(err)
		}
		// 整批中 i1 编号已占用：整批按编号占用拒绝并指出 i1。
		req := batchReq("rb-exist", be("i1", "bob"), be("i2", "carol"))

		blockBatchSave(t, r)
		res, err := r.IssueBatch(req)
		assertIssueRejectSaveFailureResult(t, res, err)
		assertIssueRequestFree(t, r, req.Operator, req.RequestID, 1)
		// i2 这个未使用编号不能被占用，历史序号也停在 1。
		assertItemsNeverIssued(t, r, "i2")

		// 原有藏品 i1 保持原状：仍由 bob 持有版本 1，只有原先一条发行历史。
		h, _ := r.GetHolding("i1")
		if h.OwnerID != "bob" || h.Version != 1 {
			t.Fatalf("原有藏品 i1 持有被改变: %+v", h)
		}
		hist1, _ := r.History("i1")
		if len(hist1) != 1 || hist1[0].RequestID != "ri1" {
			t.Fatalf("原有藏品 i1 历史被改变: %+v", hist1)
		}

		restoreBatchSave(t, r)
		res, err = r.IssueBatch(req)
		if !errors.Is(err, ErrAlreadyExists) {
			t.Fatalf("重提应返回 ErrAlreadyExists: %v", err)
		}
		if res.Replayed || res.ItemID != "i1" || !errors.Is(res.Err, ErrAlreadyExists) {
			t.Fatalf("重新保存编号占用拒绝的结果异常: %+v", res)
		}
		res, err = r.IssueBatch(req)
		if !errors.Is(err, ErrAlreadyExists) || !res.Replayed || res.ItemID != "i1" {
			t.Fatalf("再次提交应回放编号占用拒绝: %+v, err %v", res, err)
		}

		// 未使用的 i2 与历史序号 2 都没有被这次失败消耗：随后可正常发行。
		if _, err := r.Issue(IssueRequest{
			Operator: "alice", Reason: "补发行 i2", RequestID: "ri2",
			ItemID: "i2", SeriesID: "s1", BatchNo: "b1", HolderID: "bob",
		}); err != nil {
			t.Fatalf("未占用的 i2 应仍可发行: %v", err)
		}
		h2, _ := r.GetHolding("i2")
		if h2.OwnerID != "bob" || h2.Version != 1 {
			t.Fatalf("i2 发行结果异常: %+v", h2)
		}
		hist2, _ := r.History("i2")
		if len(hist2) != 1 || hist2[0].Seq != 2 {
			t.Fatalf("i2 应使用历史序号 2: %+v", hist2)
		}
		if r.state.NextSeq != 2 {
			t.Fatalf("NextSeq = %d, want 2", r.state.NextSeq)
		}
	})
}

// TestIssueBatchRejectSaveFailureInterveningSuccess 覆盖：拒绝保存失败后，
// 同一已打开登记册上的其他正常操作成功保存时，不能把这次未保存的拒绝一起
// 写入；之后用原请求号重提仍按全新请求处理（重新保存拒绝或直接冲突都不能
// 源于那次未保存的记录）。
func TestIssueBatchRejectSaveFailureInterveningSuccess(t *testing.T) {
	r := mustCreate(t, tempDir(t))
	setupBatchWorld(t, r)
	if err := r.DeactivateAccount("bob"); err != nil {
		t.Fatal(err)
	}
	req := batchReq("rb-inter", be("i1", "bob"))

	blockBatchSave(t, r)
	res, err := r.IssueBatch(req)
	assertIssueRejectSaveFailureResult(t, res, err)
	assertIssueRequestFree(t, r, req.Operator, req.RequestID, 0)

	// 恢复保存后做一笔与该请求号无关的成功操作（另一个请求号发行 i9）。
	restoreBatchSave(t, r)
	if _, err := r.Issue(IssueRequest{
		Operator: "alice", Reason: "无关的单件发行", RequestID: "ri9",
		ItemID: "i9", SeriesID: "s1", BatchNo: "b9", HolderID: "alice",
	}); err != nil {
		t.Fatal(err)
	}
	if _, ok := r.state.Requests[requestKey(req.Operator, req.RequestID)]; ok {
		t.Fatal("无关操作成功保存后，未保存的拒绝仍不应出现")
	}
	if r.state.NextSeq != 1 {
		t.Fatalf("NextSeq = %d, want 1（只有无关发行占用序号）", r.state.NextSeq)
	}
	assertItemsNeverIssued(t, r, "i1")

	// bob 仍停用：用原请求号重提是一次全新判断，重新保存停用拒绝并返回
	// 业务错误（不是回放，也不是请求号冲突）。
	res, err = r.IssueBatch(req)
	if !errors.Is(err, ErrAccountInactive) || res.Replayed || res.ItemID != "i1" {
		t.Fatalf("重提应作为全新请求重新保存停用拒绝: %+v, err %v", res, err)
	}
	assertItemsNeverIssued(t, r, "i1")
	if r.state.NextSeq != 1 {
		t.Fatalf("拒绝仍不应消耗历史序号: NextSeq = %d", r.state.NextSeq)
	}
}

// TestIssueBatchValidationErrorIgnoresSaveFailure 覆盖：空清单、清单内重复
// 编号、必填内容缺失按参数错误拒绝；操作者、系列或初始持有人不存在按对象
// 不存在拒绝。这些无需保存的错误即使在存储不可写时也仍返回原错误，不被
// 替换为保存错误，且不占用请求号、不消耗历史序号。
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
	dup := batchReq("rb-dup", be("i1", "bob"), be("i1", "carol"))
	if _, err := r.IssueBatch(dup); !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("重复编号应返回 ErrInvalidArgument，不能改报保存错误: %v", err)
	}
	assertIssueRequestFree(t, r, dup.Operator, dup.RequestID, 0)
	// 必填内容缺失（无操作者、系列、批次号、请求号与清单）。
	if _, err := r.IssueBatch(IssueBatchRequest{Reason: "x"}); !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("必填缺失应返回 ErrInvalidArgument: %v", err)
	}
	// 初始持有人不存在：对象不存在错误，并指出涉及的藏品编号。
	missingHolder := batchReq("rb-nf-holder", be("i1", "ghost"))
	res, err := r.IssueBatch(missingHolder)
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("初始持有人不存在应返回 ErrNotFound，不能改报保存错误: %v", err)
	}
	if res.ItemID != "i1" || !errors.Is(res.Err, ErrNotFound) {
		t.Fatalf("引用不存在的结果异常: %+v", res)
	}
	assertIssueRequestFree(t, r, missingHolder.Operator, missingHolder.RequestID, 0)
	assertItemsNeverIssued(t, r, "i1")
	// 系列不存在：操作者层面正常，系列层面返回对象不存在且不附藏品编号。
	missingSeries := IssueBatchRequest{
		Operator: "alice", Reason: "整批首发", RequestID: "rb-nf-series",
		SeriesID: "nope", BatchNo: "b1", Entries: []IssueBatchEntry{be("i1", "bob")},
	}
	res, err = r.IssueBatch(missingSeries)
	if !errors.Is(err, ErrNotFound) || res.ItemID != "" || !errors.Is(res.Err, ErrNotFound) {
		t.Fatalf("系列不存在应返回 ErrNotFound 且不附藏品编号: %+v, err %v", res, err)
	}
	assertIssueRequestFree(t, r, missingSeries.Operator, missingSeries.RequestID, 0)
	// 操作者不存在：对象不存在且不附藏品编号。
	missingOperator := IssueBatchRequest{
		Operator: "ghost", Reason: "整批首发", RequestID: "rb-nf-op",
		SeriesID: "s1", BatchNo: "b1", Entries: []IssueBatchEntry{be("i1", "bob")},
	}
	res, err = r.IssueBatch(missingOperator)
	if !errors.Is(err, ErrNotFound) || res.ItemID != "" || !errors.Is(res.Err, ErrNotFound) {
		t.Fatalf("操作者不存在应返回 ErrNotFound 且不附藏品编号: %+v, err %v", res, err)
	}
	assertItemsNeverIssued(t, r, "i1")
}

// TestIssueBatchRejectSaveFailureRetryAfterReopen 覆盖磁盘视角：拒绝保存
// 失败后原数据文件从未被替换，关闭并重新 Open 看到的仍是拒绝前的状态，
// 没有任何拒绝记录、藏品或历史；恢复保存条件后用原请求重提，按当时状态
// 重新判断并全新保存一次拒绝（不是回放）。
func TestIssueBatchRejectSaveFailureRetryAfterReopen(t *testing.T) {
	dir := tempDir(t)
	r := mustCreate(t, dir)
	t.Cleanup(func() { _ = r.Close() })
	setupBatchWorld(t, r)
	if err := r.DeactivateAccount("bob"); err != nil {
		t.Fatal(err)
	}
	req := batchReq("rb-reopen", be("i1", "bob"))

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
	assertIssueRequestFree(t, r2, req.Operator, req.RequestID, 0)
	assertItemsNeverIssued(t, r2, "i1")

	restoreBatchSave(t, r2)
	res, err = r2.IssueBatch(req)
	if !errors.Is(err, ErrAccountInactive) || res.Replayed || res.ItemID != "i1" {
		t.Fatalf("重开后重提应作为全新请求重新保存拒绝: %+v, err %v", res, err)
	}
	res, err = r2.IssueBatch(req)
	if !errors.Is(err, ErrAccountInactive) || !res.Replayed || res.ItemID != "i1" {
		t.Fatalf("保存成功后再次提交应回放拒绝: %+v, err %v", res, err)
	}
}
