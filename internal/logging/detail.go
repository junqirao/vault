package logging

import (
	"sort"
	"strings"
)

// detailPhrase 是日志 detail 的轻量技术词对照。
//
// 为什么不维护 600 条完整译文：日志原文大量是"动词 + 领域名词 + 失败"的固定结构，
// 用一张小词表按最长匹配替换，就能得到可读的英文 detail（如"写入本地状态失败" →
// "write local state failed"）。它只服务排障，不参与多语言渲染，也不需要完美语法。
type detailPhrase struct {
	zh string
	en string
}

var detailPhrases = []detailPhrase{
	// 较长、需要优先匹配的固定说法
	{"对账超出时间预算，已截断", "reconciliation exceeded its time budget and was truncated"},
	{"本地状态文件无法解析，已忽略其内容", "failed to parse the local state file; its contents were ignored"},
	{"创建本地配置文件失败（将仅使用内存配置）", "failed to create the local config file; memory-only config will be used"},
	{"写入本地配置失败（仅内存生效）", "failed to write the local config; changes kept in memory only"},
	{"写入本地状态失败（将仅使用内存状态）", "failed to write local state; using in-memory state only"},
	{"写入本地状态失败", "failed to write local state"},
	{"磁盘缺失", "disk is missing"},
	{"自动挂载失败", "auto-mount failed"},
	{"租约心跳失败", "lease heartbeat failed"},
	{"按实际占用重算用户用量失败", "failed to recalculate user usage from actual disk usage"},
	{"按实际占用重算存储库用量失败", "failed to recalculate repository usage from actual disk usage"},
	{"按实际占用重算已用量失败", "failed to recalculate used bytes from actual disk usage"},
	{"重算用户用量失败", "failed to recalculate user usage"},
	{"重算已用量失败", "failed to recalculate used bytes"},
	{"本地 iSCSI 代理不可用，回退直连服务端目标", "local iSCSI proxy unavailable; falling back to a direct server target"},
	{"本地 iSCSI 代理取不到监听地址，回退直连服务端目标", "local iSCSI proxy has no listen address; falling back to a direct server target"},
	{"本机 iSCSI 发起端未就绪", "local iSCSI initiator is not ready"},

	// 动词
	{"清理", "clean up "},
	{"删除", "delete "},
	{"移除", "remove "},
	{"写入", "write "},
	{"读取", "read "},
	{"查询", "query "},
	{"更新", "update "},
	{"刷新", "refresh "},
	{"保存", "save "},
	{"创建", "create "},
	{"初始化", "initialize "},
	{"启动", "start "},
	{"停止", "stop "},
	{"关闭", "close "},
	{"打开", "open "},
	{"导出", "export "},
	{"导入", "import "},
	{"计算", "calculate "},
	{"重算", "recalculate "},
	{"标记", "mark "},
	{"设置", "set "},
	{"获取", "get "},
	{"解析", "parse "},
	{"解密", "decrypt "},
	{"加密", "encrypt "},
	{"生成", "generate "},
	{"登记", "register "},
	{"注销", "unregister "},
	{"回写", "write back "},
	{"恢复", "restore "},
	{"回滚", "roll back "},
	{"迁移", "migrate "},
	{"复制", "copy "},
	{"扩容", "resize "},
	{"挂载", "mount "},
	{"卸载", "unmount "},
	{"断开", "disconnect "},
	{"连接", "connect "},
	{"重连", "reconnect "},
	{"中止", "abort "},
	{"取消", "cancel "},
	{"等待", "wait for "},
	{"跳过", "skip "},
	{"继续", "continue "},
	{"尝试", "try "},
	{"重试", "retry "},
	{"执行", "execute "},
	{"处理", "process "},
	{"接受", "accept "},
	{"注入", "inject "},
	{"落盘", "persist "},
	{"同步", "sync "},
	{"校验", "verify "},
	{"验证", "verify "},
	{"探测", "probe "},
	{"检查", "check "},
	{"清理", "clean up "},
	{"回收", "reclaim "},
	{"释放", "release "},
	{"撤销", "revoke "},
	{"停用", "disable "},
	{"启用", "enable "},
	{"选择", "select "},
	{"分配", "allocate "},

	// 领域名词
	{"虚拟盘", "virtual disk "},
	{"差异盘", "diff disk "},
	{"母盘", "parent disk "},
	{"存储库", "repository "},
	{"存储池", "storage pool "},
	{"存储卷", "storage volume "},
	{"存储", "storage "},
	{"磁盘", "disk "},
	{"缓存", "cache "},
	{"会话", "session "},
	{"租约", "lease "},
	{"目标", "target "},
	{"分配", "allocation "},
	{"用户", "user "},
	{"配额", "quota "},
	{"任务", "job "},
	{"文件", "file "},
	{"目录", "directory "},
	{"路径", "path "},
	{"空间", "space "},
	{"容量", "capacity "},
	{"元数据", "metadata "},
	{"指纹", "fingerprint "},
	{"状态", "state "},
	{"配置", "config "},
	{"证书", "certificate "},
	{"身份", "identity "},
	{"密钥", "key "},
	{"令牌", "token "},
	{"服务端", "server "},
	{"客户端", "client "},
	{"代理", "proxy "},
	{"门户", "portal "},
	{"端口", "port "},
	{"地址", "address "},
	{"版本", "version "},
	{"时间", "time "},
	{"预算", "budget "},
	{"数据", "data "},
	{"记录", "record "},
	{"结果", "result "},
	{"原因", "reason "},
	{"进程", "process "},
	{"依赖", "dependency "},
	{"工具", "tool "},
	{"模块", "module "},
	{"服务", "service "},
	{"系统", "system "},
	{"数据库", "database "},
	{"表", "table "},
	{"字段", "field "},
	{"参数", "parameter "},
	{"权限", "permission "},
	{"队列", "queue "},
	{"事件", "event "},
	{"通道", "channel "},
	{"心跳", "heartbeat "},
	{"快照", "snapshot "},
	{"备份", "backup "},
	{"源", "source "},
	{"本地", "local "},
	{"远程", "remote "},
	{"实际", "actual "},
	{"物理", "physical "},
	{"逻辑", "logical "},
	{"可用", "available "},
	{"占用", "usage "},
	{"用量", "usage "},
	{"安装", "install "},
	{"卸载", "uninstall "},
	{"线程", "thread "},
	{"任务", "job "},

	// 连接词 / 状态词
	{"重建", "rebuild "},
	{"孤儿", "orphan "},
	{"残骸", "leftover "},
	{"预检", "precheck "},
	{"记账", "accounting "},
	{"下发", "dispatch "},
	{"派生", "derive "},
	{"维护", "maintenance "},
	{"只读", "read-only "},
	{"卷", "volume "},
	{"故障", "failure "},
	{"恢复", "restore "},
	{"异常", "error "},
	{"失败", " failed"},
	{"成功", " succeeded"},
	{"不可用", " unavailable"},
	{"无法", "cannot "},
	{"未完成", "not completed"},
	{"未就绪", "not ready"},
	{"不一致", "mismatch"},
	{"不存在", "does not exist"},
	{"不支持", "unsupported"},
	{"超时", "timed out"},
	{"已取消", "canceled"},
	{"被取消", "canceled"},
	{"不完整", "incomplete"},
	{"已存在", "already exists"},
	{"重复", "duplicate"},
	{"异常", "error"},
	{"非法", "invalid"},
	{"有效", "valid"},
	{"默认", "default"},
	{"当前", "current"},
	{"目标", "target "},
	{"所有", "all "},
	{"部分", "partial "},
	{"临时", "temporary "},
	{"自动", "automatic "},
	{"手动", "manual "},
	{"重新", "re-"},
	{"没有", "no "},
	{"不能", "cannot "},
	{"不会", "will not "},
	{"已经", "already "},
	{"正在", "in progress: "},
	{"完成", "completed"},
	{"有效", "valid"},
}

var detailExact = map[string]string{
	"磁盘缺失":         "disk is missing",
	"自动挂载失败":       "auto-mount failed",
	"租约心跳失败":       "lease heartbeat failed",
	"对账超出时间预算，已截断": "reconciliation exceeded its time budget and was truncated",
}

func init() {
	// 最长匹配优先：先替换"存储库"，再替换"存储"，避免出现 "storage repository" 之类碎片。
	sort.SliceStable(detailPhrases, func(i, j int) bool {
		return len([]rune(detailPhrases[i].zh)) > len([]rune(detailPhrases[j].zh))
	})
}

// translateDetail 把中文日志原文转成尽量可读的英文 detail；失败返回空串。
func translateDetail(msg string) string {
	msg = strings.TrimSpace(msg)
	if msg == "" {
		return ""
	}
	if exact, ok := detailExact[msg]; ok {
		return exact
	}
	out := msg
	for _, p := range detailPhrases {
		out = strings.ReplaceAll(out, p.zh, p.en)
	}
	// 去掉仍未被词表覆盖的中文字符，避免 detail 里混入中文；英文残句仍比
	// "unmapped log event" 更有信息量。
	var b strings.Builder
	for _, r := range out {
		if r < 128 {
			b.WriteRune(r)
		} else {
			b.WriteByte(' ')
		}
	}
	clean := strings.Join(strings.Fields(b.String()), " ")
	if !strings.ContainsAny(clean, "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ") {
		return ""
	}
	return clean
}
