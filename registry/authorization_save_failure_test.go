package registry

import (
	"errors"
	"os"
	"testing"
	"time"
)

// 本文件为创建代转授权补充"保存失败"场景的回归保障，与单件/整批发行、
// 单件/整批转让（issue/transfer/batch/batch_transfer_save_failure_test.go）
// 行为对齐：成功或状态类业务拒绝（授权编号占用、账户停用、持有版本不符）
// 都以保存完成为准。本次结果尚未写入原登记册而保存失败时，必须返回实际
// 保存错误——不能报告创建成功，也不能只返回原业务拒绝；结果为空（授权
// 编号与状态为空、绑定版本为零、不标回放、业务错误为空），请求号、授权
// 历史序号都不被这次未保存的操作消耗，即使失败后原数据暂时无法读取、
// 状态未能按磁盘重建，也不留下任何新增授权、创建记录或请求号占用。
// 保存条件恢复后用完全相同的请求重提，按当时的业务状态重新判断：拒绝
// 条件仍在则重新保存此次拒绝并返回对应业务错误，状态已满足请求则正常
// 创建，首次重提不标回放。
//
// 失败注入方式与其他场景共用：在临时文件路径 .registry.json.tmp 上预先
// 建一个目录，save 在 OpenFile 阶段即以 EISDIR 失败，原快照完好可读；
// 另有用例同时破坏数据文件，覆盖"原数据暂时无法读取"的情形。

// assertCreateAuthzSaveFailureEmpty 核对"保存失败"的返回：error 是保存
// 错误而非创建成功或任何业务拒绝；结果为空——不携带授权编号、状态或
// 绑定版本，业务错误为空，也不标记为重复返回。
func assertCreateAuthzSaveFailureEmpty(t *testing.T, res CreateAuthorizationResult, err error) {
	t.Helper()
	assertSaveFailureError(t, err)
	if res.AuthID != "" || res.Status != "" || res.GrantVer != 0 ||
		res.Replayed || res.Err != nil {
		t.Fatalf("保存失败必须返回空结果: %+v", res)
	}
}

// assertAuthzRequestFree 核对请求号未被这次未保存的操作占用、授权历史
// 序号未被消耗（在同一个已打开的登记册上检查）。
func assertAuthzRequestFree(t *testing.T, r *Registry, req CreateAuthorizationRequest, wantNextAuthSeq int64) {
	t.Helper()
	if _, ok := r.state.Requests[requestKey(req.Operator, req.RequestID)]; ok {
		t.Fatalf("未保存的结果不应占用请求号 %s", req.RequestID)
	}
	if r.state.NextAuthSeq != wantNextAuthSeq {
		t.Fatalf("NextAuthSeq = %d, want %d", r.state.NextAuthSeq, wantNextAuthSeq)
	}
}

// assertAuthzAbsent 核对失败的新授权查询为不存在，且藏品的授权历史保持
// 操作前的内容与条数。
func assertAuthzAbsent(t *testing.T, r *Registry, authID, itemID string, wantEvents int) {
	t.Helper()
	if _, err := r.GetAuthorization(authID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("未保存的授权 %s 应查询为不存在: %v", authID, err)
	}
	evs, err := r.AuthorizationHistory(itemID)
	if err != nil {
		t.Fatalf("AuthorizationHistory %s: %v", itemID, err)
	}
	if len(evs) != wantEvents {
		t.Fatalf("授权历史应保持 %d 条: %+v", wantEvents, evs)
	}
}

// TestCreateAuthzSuccessSaveFailure 覆盖：创建授权的成功结果落盘失败时
// 返回保存错误、结果为空；同一个仍打开的登记册中授权查询为不存在、授权
// 历史为空、请求号与授权历史序号都未被消耗、藏品持有不变。保存条件恢复
// 后，其他操作成功保存不会把这次未保存的授权带入登记册；用完全相同的
// 请求重提正常创建（不标回放），此后相同请求回放首次成功。
func TestCreateAuthzSuccessSaveFailure(t *testing.T) {
	r := mustCreate(t, tempDir(t))
	authzWorld(t, r)
	req := createAuthReq(r, "a1", time.Hour)

	blockBatchSave(t, r)
	res, err := r.CreateAuthorization(req)
	assertCreateAuthzSaveFailureEmpty(t, res, err)
	assertAuthzRequestFree(t, r, req, 0)
	assertAuthzAbsent(t, r, "a1", "i1", 0)
	assertHoldingUnchanged(t, r, "i1", "alice", 1, 1)

	// 保存条件未恢复时再次提交同一请求：仍重新尝试保存并失败，不能把
	// 上次未保存的结果当成已保存回放。
	res, err = r.CreateAuthorization(req)
	assertCreateAuthzSaveFailureEmpty(t, res, err)
	assertAuthzRequestFree(t, r, req, 0)

	// 保存条件恢复后，其他操作成功保存也不能把这次未保存的授权或创建
	// 记录带入登记册。
	restoreBatchSave(t, r)
	if err := r.RegisterAccount("dave", "路人"); err != nil {
		t.Fatal(err)
	}
	assertAuthzRequestFree(t, r, req, 0)
	assertAuthzAbsent(t, r, "a1", "i1", 0)

	// 用完全相同的请求重提：正常创建，不标回放。
	res, err = r.CreateAuthorization(req)
	if err != nil {
		t.Fatalf("恢复后重提应正常创建: %v", err)
	}
	if res.Replayed || res.Err != nil || res.AuthID != "a1" ||
		res.Status != AuthActive || res.GrantVer != 1 {
		t.Fatalf("重提结果异常: %+v", res)
	}
	a, err := r.GetAuthorization("a1")
	if err != nil {
		t.Fatal(err)
	}
	if a.Status != AuthActive || a.GrantVer != 1 || a.GranterID != "alice" {
		t.Fatalf("授权内容异常: %+v", a)
	}
	evs, err := r.AuthorizationHistory("i1")
	if err != nil || len(evs) != 1 || evs[0].Seq != 1 || evs[0].Kind != "create" {
		t.Fatalf("授权历史异常: %+v, %v", evs, err)
	}

	// 成功结果保存后，相同请求回放首次成功。
	res, err = r.CreateAuthorization(req)
	if err != nil || !res.Replayed || res.AuthID != "a1" ||
		res.Status != AuthActive || res.GrantVer != 1 {
		t.Fatalf("成功后重提应回放: %+v, err %v", res, err)
	}
}

// TestCreateAuthzSuccessSaveFailureUnreadableDisk 覆盖：保存失败且原数据
// 暂时无法读取（状态未能按磁盘重建）时，未保存的授权、创建记录与请求号
// 占用也不能留在当前登记册中；恢复正常读写后，其他操作成功保存不带入
// 这次未保存的内容，原请求重提正常创建。
func TestCreateAuthzSuccessSaveFailureUnreadableDisk(t *testing.T) {
	r := mustCreate(t, tempDir(t))
	authzWorld(t, r)
	req := createAuthReq(r, "a1", time.Hour)

	// 让保存失败，同时让原数据暂时无法读取：commit 无法按磁盘重建状态。
	orig, err := os.ReadFile(dataFile(r.dir))
	if err != nil {
		t.Fatal(err)
	}
	blockBatchSave(t, r)
	if err := os.WriteFile(dataFile(r.dir), []byte("{corrupt"), fileMode); err != nil {
		t.Fatal(err)
	}

	res, err := r.CreateAuthorization(req)
	assertCreateAuthzSaveFailureEmpty(t, res, err)
	// 状态未能重建时也必须显式撤销：同一个仍打开的登记册中查不到这次
	// 未保存的授权与创建记录，请求号与授权历史序号都未被消耗。
	assertAuthzRequestFree(t, r, req, 0)
	assertAuthzAbsent(t, r, "a1", "i1", 0)
	assertHoldingUnchanged(t, r, "i1", "alice", 1, 1)

	// 恢复正常读写。
	if err := os.WriteFile(dataFile(r.dir), orig, fileMode); err != nil {
		t.Fatal(err)
	}
	restoreBatchSave(t, r)

	// 其他操作成功保存不能把这次未保存的授权或请求号占用带入登记册。
	if err := r.RegisterAccount("dave", "路人"); err != nil {
		t.Fatal(err)
	}
	assertAuthzRequestFree(t, r, req, 0)
	assertAuthzAbsent(t, r, "a1", "i1", 0)

	// 用完全相同的请求重提：正常创建，不标回放。
	res, err = r.CreateAuthorization(req)
	if err != nil {
		t.Fatalf("恢复后重提应正常创建: %v", err)
	}
	if res.Replayed || res.AuthID != "a1" || res.Status != AuthActive || res.GrantVer != 1 {
		t.Fatalf("重提结果异常: %+v", res)
	}
	if r.state.NextAuthSeq != 1 {
		t.Fatalf("授权历史序号应连续: NextAuthSeq = %d", r.state.NextAuthSeq)
	}
}

// TestCreateAuthzRejectSaveFailureRetrySameState 覆盖：持有版本不符的拒绝
// 落盘失败时返回保存错误、结果为空、请求号不被占用；保存恢复且拒绝条件
// 仍在时，用完全相同的请求重提会重新保存此次拒绝，保存成功后才返回业务
// 错误（首次重提不标回放）；此后相同请求回放该拒绝。
func TestCreateAuthzRejectSaveFailureRetrySameState(t *testing.T) {
	r := mustCreate(t, tempDir(t))
	authzWorld(t, r)
	// i1 当前为 alice 版本 1，期望版本 2：ErrConflict。
	req := createAuthReq(r, "a1", time.Hour)
	req.ExpectedVer = 2

	blockBatchSave(t, r)
	res, err := r.CreateAuthorization(req)
	assertCreateAuthzSaveFailureEmpty(t, res, err)
	assertAuthzRequestFree(t, r, req, 0)
	assertAuthzAbsent(t, r, "a1", "i1", 0)

	// 保存条件未恢复时再次提交同一请求：仍按当时状态重新判断并再次尝试
	// 保存，不能把上次未保存的拒绝当成已保存的拒绝回放。
	res, err = r.CreateAuthorization(req)
	assertCreateAuthzSaveFailureEmpty(t, res, err)
	assertAuthzRequestFree(t, r, req, 0)

	// 保存条件恢复、拒绝条件仍在：重提重新保存此次拒绝，保存成功后返回
	// 业务错误本身并附授权编号，这次重新处理不标记为重复。
	restoreBatchSave(t, r)
	res, err = r.CreateAuthorization(req)
	if !errors.Is(err, ErrConflict) {
		t.Fatalf("重提应在保存成功后返回业务拒绝 ErrConflict: %v", err)
	}
	if res.Replayed || res.AuthID != "a1" || !errors.Is(res.Err, ErrConflict) ||
		res.Status != "" || res.GrantVer != 0 {
		t.Fatalf("重提结果异常: %+v", res)
	}
	if _, ok := r.state.Requests[requestKey(req.Operator, req.RequestID)]; !ok {
		t.Fatal("拒绝保存成功后应登记请求结果")
	}
	// 拒绝不创建授权，也不消耗授权历史序号。
	assertAuthzAbsent(t, r, "a1", "i1", 0)
	if r.state.NextAuthSeq != 0 {
		t.Fatalf("拒绝不应消耗授权历史序号: NextAuthSeq = %d", r.state.NextAuthSeq)
	}

	// 拒绝已保存：相同内容重提回放原拒绝。
	res, err = r.CreateAuthorization(req)
	if !errors.Is(err, ErrConflict) || !res.Replayed || res.AuthID != "a1" ||
		!errors.Is(res.Err, ErrConflict) {
		t.Fatalf("保存后的重提应回放原拒绝: %+v, err %v", res, err)
	}
}

// TestCreateAuthzRejectSaveFailureRetryAfterStateChange 覆盖：未保存的冲突
// 拒绝不阻碍后续重提——保存条件恢复后，藏品经合法转让恰好达到原请求要求
// 的持有人与版本，且授权仍未到期，用完全相同的请求重提应正常创建。
func TestCreateAuthzRejectSaveFailureRetryAfterStateChange(t *testing.T) {
	r := mustCreate(t, tempDir(t))
	setupWorld(t, r)
	if err := r.RegisterAccount("carol", "接收人"); err != nil {
		t.Fatal(err)
	}
	// i1 发行给 bob（版本 1）。
	if _, err := r.Issue(issueReq("i1", "bob")); err != nil {
		t.Fatal(err)
	}
	// alice 期望自己持有版本 2：当前为 bob 版本 1，先被冲突拒绝。
	req := CreateAuthorizationRequest{
		Operator: "alice", Reason: "委托代转", RequestID: "create-a1",
		AuthID: "a1", ItemID: "i1", TrusteeID: "bob", ToID: "carol",
		ExpectedOwner: "alice", ExpectedVer: 2,
		ExpiresAt: r.now().Add(time.Hour),
	}

	blockBatchSave(t, r)
	res, err := r.CreateAuthorization(req)
	assertCreateAuthzSaveFailureEmpty(t, res, err)
	assertAuthzRequestFree(t, r, req, 0)

	// 保存条件恢复后，bob 把 i1 合法转让给 alice：i1 变为 alice 版本 2，
	// 恰好满足原请求的期望持有人与版本。
	restoreBatchSave(t, r)
	if _, err := r.Transfer(xferReq("bob", "i1", "alice", 1, "rt-i1")); err != nil {
		t.Fatal(err)
	}

	// 用完全相同的原请求重提：按当前业务状态判断，正常创建不标回放。
	res, err = r.CreateAuthorization(req)
	if err != nil {
		t.Fatalf("状态满足后重提应正常创建，不能回放未保存的冲突: %v", err)
	}
	if res.Replayed || res.Err != nil || res.AuthID != "a1" ||
		res.Status != AuthActive || res.GrantVer != 2 {
		t.Fatalf("重提结果异常: %+v", res)
	}
	a, err := r.GetAuthorization("a1")
	if err != nil {
		t.Fatal(err)
	}
	if a.Status != AuthActive || a.GrantVer != 2 || a.GranterID != "alice" {
		t.Fatalf("授权内容异常: %+v", a)
	}

	// 成功结果保存后，相同请求回放首次成功。
	replay, err := r.CreateAuthorization(req)
	if err != nil || !replay.Replayed || replay.GrantVer != 2 {
		t.Fatalf("成功后重提应回放: %+v, err %v", replay, err)
	}
}

// TestCreateAuthzRejectSaveFailureAuthIDTaken 覆盖：授权编号已被占用的
// 拒绝落盘失败时返回保存错误、结果为空；失败请求使用了已存在的授权
// 编号，原授权及其历史完整保留；保存恢复后重提返回 ErrAlreadyExists。
func TestCreateAuthzRejectSaveFailureAuthIDTaken(t *testing.T) {
	r := mustCreate(t, tempDir(t))
	authzWorld(t, r)
	if _, err := r.CreateAuthorization(createAuthReq(r, "a1", time.Hour)); err != nil {
		t.Fatal(err)
	}
	// 换一个请求号再次使用已占用的授权编号 a1：ErrAlreadyExists。
	req := createAuthReq(r, "a1", time.Hour)
	req.RequestID = "create-a1-again"

	blockBatchSave(t, r)
	res, err := r.CreateAuthorization(req)
	assertCreateAuthzSaveFailureEmpty(t, res, err)
	assertAuthzRequestFree(t, r, req, 1)

	// 原授权及其创建历史完整保留。
	a, err := r.GetAuthorization("a1")
	if err != nil {
		t.Fatalf("原授权不应受失败请求影响: %v", err)
	}
	if a.Status != AuthActive || a.GranterID != "alice" || a.TrusteeID != "bob" ||
		a.ToID != "carol" || a.GrantVer != 1 {
		t.Fatalf("原授权内容被改变: %+v", a)
	}
	evs, err := r.AuthorizationHistory("i1")
	if err != nil || len(evs) != 1 || evs[0].Kind != "create" || evs[0].AuthID != "a1" {
		t.Fatalf("原授权历史被改变: %+v, %v", evs, err)
	}

	restoreBatchSave(t, r)
	res, err = r.CreateAuthorization(req)
	if !errors.Is(err, ErrAlreadyExists) {
		t.Fatalf("重提应在保存成功后返回 ErrAlreadyExists: %v", err)
	}
	if res.Replayed || res.AuthID != "a1" || !errors.Is(res.Err, ErrAlreadyExists) {
		t.Fatalf("编号占用拒绝的重提结果异常: %+v", res)
	}
	// 原授权内容仍保持不变。
	a, _ = r.GetAuthorization("a1")
	if a.Status != AuthActive || a.GrantVer != 1 {
		t.Fatalf("原授权内容被改变: %+v", a)
	}
}

// TestCreateAuthzRejectSaveFailureAccountInactive 覆盖：账户停用的拒绝
// 落盘失败时同样返回保存错误、结果为空；保存恢复后重提重新保存拒绝并
// 返回账户错误。
func TestCreateAuthzRejectSaveFailureAccountInactive(t *testing.T) {
	r := mustCreate(t, tempDir(t))
	authzWorld(t, r)
	if err := r.DeactivateAccount("bob"); err != nil {
		t.Fatal(err)
	}
	req := createAuthReq(r, "a1", time.Hour)

	blockBatchSave(t, r)
	res, err := r.CreateAuthorization(req)
	assertCreateAuthzSaveFailureEmpty(t, res, err)
	assertAuthzRequestFree(t, r, req, 0)
	assertAuthzAbsent(t, r, "a1", "i1", 0)

	restoreBatchSave(t, r)
	res, err = r.CreateAuthorization(req)
	if !errors.Is(err, ErrAccountInactive) {
		t.Fatalf("重提应在保存成功后返回 ErrAccountInactive: %v", err)
	}
	if res.AuthID != "a1" || !errors.Is(res.Err, ErrAccountInactive) || res.Replayed {
		t.Fatalf("账户停用拒绝的重提结果异常: %+v", res)
	}
}

// TestCreateAuthzValidationErrorIgnoresSaveFailure 覆盖：必填内容缺失、
// 到期时间不合法与引用对象不存在的请求不占用请求号、不要求保存，即使
// 数据位置暂时不可写也仍返回原参数/引用错误，不能改报保存错误。
func TestCreateAuthzValidationErrorIgnoresSaveFailure(t *testing.T) {
	r := mustCreate(t, tempDir(t))
	authzWorld(t, r)

	blockBatchSave(t, r)
	defer restoreBatchSave(t, r)

	// 引用不存在的藏品：返回 ErrNotFound，不是保存错误。
	notFoundReq := createAuthReq(r, "a-nf", time.Hour)
	notFoundReq.ItemID = "nope"
	res, err := r.CreateAuthorization(notFoundReq)
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("引用不存在应返回 ErrNotFound，不能改报保存错误: %v", err)
	}
	if res.AuthID != "a-nf" || !errors.Is(res.Err, ErrNotFound) {
		t.Fatalf("引用不存在的结果异常: %+v", res)
	}
	assertAuthzRequestFree(t, r, notFoundReq, 0)

	// 到期时间不晚于当前时间：返回 ErrInvalidArgument，不是保存错误。
	expiredReq := createAuthReq(r, "a-exp", -time.Hour)
	res, err = r.CreateAuthorization(expiredReq)
	if !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("到期时间不合法应返回 ErrInvalidArgument，不能改报保存错误: %v", err)
	}
	if res.AuthID != "a-exp" || !errors.Is(res.Err, ErrInvalidArgument) {
		t.Fatalf("到期时间不合法的结果异常: %+v", res)
	}
	assertAuthzRequestFree(t, r, expiredReq, 0)

	// 必填内容缺失：返回 ErrInvalidArgument，不是保存错误。
	missingReq := createAuthReq(r, "a-miss", time.Hour)
	missingReq.TrusteeID = ""
	if _, err := r.CreateAuthorization(missingReq); !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("必填缺失应返回 ErrInvalidArgument，不能改报保存错误: %v", err)
	}
	assertAuthzRequestFree(t, r, missingReq, 0)
}
