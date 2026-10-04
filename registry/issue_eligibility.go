package registry

import "fmt"

// 发行（Issue 与 IssueBatch）共用的资格规则。
//
// 一件藏品只有在以下条件全部成立时才能发行：
//   - 操作者账户已登记且可用；
//   - 系列存在、未封存，且操作者就是系列创建账户；
//   - 藏品编号从未使用过；
//   - 初始持有人账户已登记且可用。
//
// 规则的每一步都由两个入口按各自的顺序编排：单件发行按"操作者 → 系列 →
// 藏品编号 → 初始持有人"的顺序；整批发行先查操作者与系列（不关联具体
// 藏品），再按清单顺序先查全部编号占用、后查各件初始持有人。参数合法性
// （必填内容、空清单、清单内编号重复）仍由两个入口各自的 validatePresent
// 在进入资格检查前完成。

// issueEligibility 汇集一件藏品发行资格判断所需的全部输入，与具体入口
// 无关。
type issueEligibility struct {
	operator string // 操作者，必须是系列创建账户且可用
	seriesID string // 所属系列
	itemID   string // 藏品编号
	holderID string // 初始持有人
}

// operatorIssueEligibility 判断操作者账户是否已登记且可用。
func (r *Registry) operatorIssueEligibility(operator string) error {
	op, ok := r.state.Accounts[operator]
	if !ok {
		return fmt.Errorf("%w: 操作者账户 %s", ErrNotFound, operator)
	}
	if !op.Active {
		return fmt.Errorf("%w: 操作者账户 %s", ErrAccountInactive, operator)
	}
	return nil
}

// seriesIssueEligibility 判断系列是否存在、未封存，且操作者就是系列创建
// 账户。两个入口的拒绝信息相同；整批由编排层决定不关联具体藏品编号。
func (r *Registry) seriesIssueEligibility(seriesID, operator string) error {
	s, ok := r.state.Series[seriesID]
	if !ok {
		return fmt.Errorf("%w: 系列 %s", ErrNotFound, seriesID)
	}
	if s.Sealed {
		return fmt.Errorf("%w: 系列 %s", ErrSeriesSealed, seriesID)
	}
	if s.CreatorID != operator {
		return fmt.Errorf("%w: 只有系列创建账户 %s 可以发行", ErrForbidden, s.CreatorID)
	}
	return nil
}

// itemIDIssueEligibility 判断藏品编号是否从未使用。
func (r *Registry) itemIDIssueEligibility(itemID string) error {
	if _, ok := r.state.Items[itemID]; ok {
		return fmt.Errorf("%w: 藏品编号 %s 已被使用", ErrAlreadyExists, itemID)
	}
	return nil
}

// holderIssueEligibility 判断初始持有人账户是否已登记且可用。两个入口的
// 拒绝信息相同；整批由编排层另行在结果中关联藏品编号。
func (r *Registry) holderIssueEligibility(holderID string) error {
	h, ok := r.state.Accounts[holderID]
	if !ok {
		return fmt.Errorf("%w: 初始持有人账户 %s", ErrNotFound, holderID)
	}
	if !h.Active {
		return fmt.Errorf("%w: 初始持有人账户 %s", ErrAccountInactive, holderID)
	}
	return nil
}

// checkSingleIssueEligibility 按单件发行的优先级编排共用规则：操作者未
// 登记或已停用最先报告；随后是系列问题——不存在优先于已封存，已封存优先
// 于操作者不是创建账户；藏品编号与初始持有人的问题不能提前覆盖这些拒绝，
// 编号已占用仍优先于初始持有人未登记或停用。
func (r *Registry) checkSingleIssueEligibility(q issueEligibility) error {
	if err := r.operatorIssueEligibility(q.operator); err != nil {
		return err
	}
	if err := r.seriesIssueEligibility(q.seriesID, q.operator); err != nil {
		return err
	}
	if err := r.itemIDIssueEligibility(q.itemID); err != nil {
		return err
	}
	return r.holderIssueEligibility(q.holderID)
}

// checkBatchIssueEligibility 按整批发行的优先级编排共用规则：操作者与
// 系列层面的问题最先报告且不关联任何藏品（itemID 返回空）；之后先按清单
// 顺序检查全部藏品编号占用，再按清单顺序检查各件初始持有人——编号占用
// 始终优先于初始持有人问题，同一类问题中报告清单顺序最前的那件。任何一
// 件不通过则整批拒绝。
func (r *Registry) checkBatchIssueEligibility(operator, seriesID string, entries []IssueBatchEntry) (string, error) {
	if err := r.operatorIssueEligibility(operator); err != nil {
		return "", err
	}
	if err := r.seriesIssueEligibility(seriesID, operator); err != nil {
		return "", err
	}
	for _, e := range entries {
		if err := r.itemIDIssueEligibility(e.ItemID); err != nil {
			return e.ItemID, err
		}
	}
	for _, e := range entries {
		if err := r.holderIssueEligibility(e.HolderID); err != nil {
			return e.ItemID, err
		}
	}
	return "", nil
}
