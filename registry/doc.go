// Package registry 是本地数字藏品登记与流转的实现。
//
// 一个 Registry 对应本机上的一个数据目录：用 Create 新建登记册，用 Open
// 打开已有登记册。账户、系列、藏品、持有关系、历史、版税规则与应付记录、
// 请求结果在每次成功操作返回前原子落盘，正常关闭或进程被直接终止后重开
// 都能完整保留；已有数据无法读取时 Open 会明确报错，不会被当成空登记册。
package registry

// Ready 表示基线可以运行。新增能力不改变其语义。
func Ready() bool { return true }
