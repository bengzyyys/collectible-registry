package registry

import (
	"errors"
	"testing"
)

// 验证含零字符的账户编号与请求号不再互相占用请求号。
func TestNULRequestKeyIsolation(t *testing.T) {
	dir := t.TempDir()
	r, err := Create(dir)
	if err != nil {
		t.Fatal(err)
	}
	// 账户甲 "a"，账户乙 "a\x00b"
	for _, id := range []string{"a", "a\x00b"} {
		if err := r.RegisterAccount(id, ""); err != nil {
			t.Fatal(err)
		}
	}
	if err := r.CreateSeries("s1", "a", ""); err != nil {
		t.Fatal(err)
	}
	if err := r.CreateSeries("s2", "a\x00b", ""); err != nil {
		t.Fatal(err)
	}

	// 甲用请求号 "b\x00c" 发行 item1
	res1, err := r.Issue(IssueRequest{Operator: "a", Reason: "r", RequestID: "b\x00c",
		ItemID: "item1", SeriesID: "s1", BatchNo: "B", HolderID: "a"})
	if err != nil {
		t.Fatalf("甲发行失败: %v", err)
	}
	// 乙用请求号 "c" 发行 item2 —— 旧代码会在此误报请求号冲突
	res2, err := r.Issue(IssueRequest{Operator: "a\x00b", Reason: "r", RequestID: "c",
		ItemID: "item2", SeriesID: "s2", BatchNo: "B", HolderID: "a\x00b"})
	if err != nil {
		t.Fatalf("乙发行被错误拒绝: %v", err)
	}
	if res1.TxSeq == res2.TxSeq || res2.ItemID != "item2" {
		t.Fatalf("结果混淆: %+v %+v", res1, res2)
	}

	// 甲用相同请求号与内容重提 → 回放自己的首次结果
	again, err := r.Issue(IssueRequest{Operator: "a", Reason: "r", RequestID: "b\x00c",
		ItemID: "item1", SeriesID: "s1", BatchNo: "B", HolderID: "a"})
	if err != nil || !again.Replayed || again.TxSeq != res1.TxSeq {
		t.Fatalf("甲重提未回放: %+v err=%v", again, err)
	}
	// 甲用相同请求号改内容 → ErrRequestConflict
	_, err = r.Issue(IssueRequest{Operator: "a", Reason: "r", RequestID: "b\x00c",
		ItemID: "itemX", SeriesID: "s1", BatchNo: "B", HolderID: "a"})
	if !errors.Is(err, ErrRequestConflict) {
		t.Fatalf("改内容应为 ErrRequestConflict, got %v", err)
	}
	// 乙提交与甲相同业务内容（用自己的系列）→ 按自己账户状态处理，不回放甲的结果
	_, err = r.Issue(IssueRequest{Operator: "a\x00b", Reason: "r", RequestID: "b\x00c",
		ItemID: "item1", SeriesID: "s1", BatchNo: "B", HolderID: "a"})
	if !errors.Is(err, ErrForbidden) {
		t.Fatalf("乙冒用甲的内容应按乙的权限拒绝, got %v", err)
	}

	// 落盘后重开：请求仍归原账户所有
	if err := r.Close(); err != nil {
		t.Fatal(err)
	}
	r2, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer r2.Close()
	re, err := r2.Issue(IssueRequest{Operator: "a", Reason: "r", RequestID: "b\x00c",
		ItemID: "item1", SeriesID: "s1", BatchNo: "B", HolderID: "a"})
	if err != nil || !re.Replayed || re.TxSeq != res1.TxSeq {
		t.Fatalf("重开后甲的回放失败: %+v err=%v", re, err)
	}
	re2, err := r2.Issue(IssueRequest{Operator: "a\x00b", Reason: "r", RequestID: "c",
		ItemID: "item2", SeriesID: "s2", BatchNo: "B", HolderID: "a\x00b"})
	if err != nil || !re2.Replayed || re2.TxSeq != res2.TxSeq {
		t.Fatalf("重开后乙的回放失败: %+v err=%v", re2, err)
	}
}

// 验证旧格式（直接拼接键）落盘的请求在修复后仍归原提交账户所有，
// 且碰巧混淆的另一组编号可独立使用。
func TestNULRequestKeyLegacyData(t *testing.T) {
	dir := t.TempDir()
	r, err := Create(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := r.RegisterAccount("a", ""); err != nil {
		t.Fatal(err)
	}
	if err := r.RegisterAccount("a\x00b", ""); err != nil {
		t.Fatal(err)
	}
	if err := r.CreateSeries("s1", "a", ""); err != nil {
		t.Fatal(err)
	}
	if err := r.CreateSeries("s2", "a\x00b", ""); err != nil {
		t.Fatal(err)
	}
	res1, err := r.Issue(IssueRequest{Operator: "a", Reason: "r", RequestID: "b\x00c",
		ItemID: "item1", SeriesID: "s1", BatchNo: "B", HolderID: "a"})
	if err != nil {
		t.Fatal(err)
	}
	// 模拟修复前落盘的旧格式键：operator + "\x00" + requestID
	legacy := make(map[string]request, len(r.state.Requests))
	for _, req := range r.state.Requests {
		legacy[req.Operator+"\x00"+req.RequestID] = req
	}
	r.state.Requests = legacy
	if err := r.commit(); err != nil {
		t.Fatal(err)
	}
	if err := r.Close(); err != nil {
		t.Fatal(err)
	}

	r2, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer r2.Close()
	// 旧请求仍归账户甲所有，原参数回放原结果
	re, err := r2.Issue(IssueRequest{Operator: "a", Reason: "r", RequestID: "b\x00c",
		ItemID: "item1", SeriesID: "s1", BatchNo: "B", HolderID: "a"})
	if err != nil || !re.Replayed || re.TxSeq != res1.TxSeq {
		t.Fatalf("旧请求回放失败: %+v err=%v", re, err)
	}
	// 碰巧混淆的另一组 ("a\x00b", "c") 不占用原请求，可独立发行
	res2, err := r2.Issue(IssueRequest{Operator: "a\x00b", Reason: "r", RequestID: "c",
		ItemID: "item2", SeriesID: "s2", BatchNo: "B", HolderID: "a\x00b"})
	if err != nil {
		t.Fatalf("混淆组独立发行被错误拒绝: %v", err)
	}
	if res2.Replayed || res2.TxSeq == res1.TxSeq {
		t.Fatalf("混淆组不应回放甲的结果: %+v", res2)
	}
}
