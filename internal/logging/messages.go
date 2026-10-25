package logging

import (
	"regexp"
	"strings"
)

// logEvent 是一条**可多语言渲染**的日志事件。
//
// 设计目标（轻量）：
//   - 代码里仍然用普通 slog（不必为了日志引入新 API）；
//   - 只有本表里列出的消息才会被写入日志文件，其余 Info 级日志默认静默丢弃
//     （排查时把 log.level 调到 debug 可看到全部原始日志）；
//   - 写入文件的 msg 是英文（便于直接看文件/贴工单），同时附带 msg_key；
//   - 前端日志页再用 msg_key + 同一套字段把消息渲染成用户当前语言。
//
// Match 是历史中文文案（精确匹配），用于在不改几百处调用点的前提下收敛与翻译；
// 新代码也可以直接把 ID 当 msg 传（如 log.Info("job.failed", ...)）。
type logEvent struct {
	ID      string
	English string
	Match   []string
}

var logEvents = []logEvent{
	// ---- 服务端生命周期 / 配置 ----
	{ID: "server.started", English: "Server started: version {version}", Match: []string{
		"服务端启动中",
	}},
	{ID: "server.stopped", English: "Server stopped", Match: []string{
		"服务端已停止",
	}},
	{ID: "server.shutdown", English: "Graceful shutdown started", Match: []string{
		"收到停止请求，开始优雅停机",
		"收到退出信号，开始优雅停机",
	}},
	{ID: "server.http.started", English: "HTTP server started on {listen}", Match: []string{
		"HTTP 服务已启动（TLS）",
		"HTTP 服务已启动（明文）",
	}},
	{ID: "server.http.failed", English: "HTTP server exited unexpectedly", Match: []string{
		"HTTP 服务异常退出",
	}},
	{ID: "server.tls.disabled", English: "TLS is disabled for {listen}; the management API is exposed in plaintext", Match: []string{
		"HTTP 未启用 TLS：管理接口将以明文暴露，仅建议在受控内网使用",
	}},
	{ID: "server.instance.conflict", English: "Another server instance is already running", Match: []string{
		"服务端已在运行，无法启动（单实例锁获取失败）",
	}},
	{ID: "server.config.reload_failed", English: "Configuration reload failed; keeping the previous configuration", Match: []string{
		"配置热重载失败，继续使用旧配置（请检查配置文件语法）",
	}},
	{ID: "server.initialized", English: "System initialized", Match: []string{
		"系统已初始化",
	}},
	{ID: "server.not_initialized", English: "System is not initialized yet", Match: []string{
		"系统尚未初始化，请在客户端连接后按向导创建超级管理员",
	}},
	{ID: "server.start_failed", English: "Server failed to start", Match: []string{
		"装配平台后端失败",
		"初始化主密钥失败",
		"初始化 CA 失败",
		"初始化数据保险箱失败",
		"数据库配置非法",
		"读取初始化状态失败",
		"记录首次启动时间失败",
		"写回首次启动生成的值失败",
		"应用超级管理员开关失败",
		"启动配置监听失败",
	}},
	{ID: "db.failed", English: "Database operation failed", Match: []string{
		"打开数据库失败",
		"数据库迁移失败",
	}},
	{ID: "db.migrated", English: "Database migration completed", Match: []string{
		"数据库迁移完成（-migrate-only）",
	}},
	{ID: "background.panic", English: "Background task panicked", Match: []string{
		"后台 goroutine panic，已捕获并继续运行",
		"后台任务 panic，已捕获并继续运行",
	}},

	// ---- 存储 ----
	{ID: "storage.cache.refresh_failed", English: "Failed to refresh the storage volume cache", Match: []string{
		"刷新存储卷信息缓存失败",
		"存储卷信息缓存预热失败",
	}},
	{ID: "storage.volume.not_ready", English: "Storage volume is not mounted and was excluded from placement", Match: []string{
		"存储底层卷未就绪，已从落盘集合剔除",
		"存在未就绪的存储底层卷，已从落盘集合剔除",
	}},
	{ID: "storage.path.changed", English: "Storage path changed, but disks remain under the old path", Match: []string{
		"存储路径变更，但该存储下仍有磁盘（磁盘文件不会自动搬迁）",
	}},
	{ID: "storage.rollback_failed", English: "Failed to roll back storage changes", Match: []string{
		"回滚存储卷失败（可能留下无主 LV，请检查）",
		"回滚存储卷登记失败",
		"回滚存储记录失败",
		"回滚时卸载已登记的 LV 失败",
	}},
	{ID: "storage.seed_failed", English: "Failed to seed storages from configuration", Match: []string{
		"从配置初始化存储种子失败（将在 DB 无存储时回退配置根）",
	}},

	// ---- 存储库 / 磁盘 ----
	{ID: "repo.moved", English: "Repository migration completed", Match: []string{
		"存储库迁移完成",
		"存储库迁移已在之前的尝试中完成，补齐收尾",
	}},
	{ID: "repo.finalize_failed", English: "Failed to finalize repository migration", Match: []string{
		"迁移收尾后恢复存储库状态失败",
		"迁移收尾后读取存储库失败",
		"迁移收尾时读取存储库失败",
		"恢复存储库迁移状态失败",
	}},
	{ID: "repo.parent_fingerprint_mismatch", English: "Parent disk fingerprint mismatch; all child targets will be disabled", Match: []string{
		"母盘指纹不一致：母盘可能被绕过改写，将停用全部子盘目标",
	}},
	{ID: "repo.parent_fingerprint_refreshed", English: "Parent disk fingerprint refreshed after a system move", Match: []string{
		"母盘指纹仅 mtime 变化且与迁移记录吻合，判定为系统搬迁并已刷新指纹",
	}},
	{ID: "disk.import.failed", English: "Disk import failed", Match: []string{
		"导入磁盘失败",
		"导入时计算磁盘指纹失败（不影响导入，对账会补算）",
	}},
	{ID: "disk.orphan.cleaned", English: "Orphan disk resources cleaned up", Match: []string{
		"已自动清理孤儿磁盘文件",
		"自动清理孤儿磁盘文件完成（部分失败，下轮重试）",
		"本轮自动清理孤儿 iSCSI 目标完成",
		"已删除孤儿 iSCSI 目标",
	}},
	{ID: "disk.orphan.cleanup_failed", English: "Orphan cleanup failed; will retry next round", Match: []string{
		"自动删除孤儿磁盘文件失败，下轮重试",
		"删除孤儿 iSCSI 目标失败",
		"清理历史孤儿目录文件失败",
	}},
	{ID: "disk.create.failed", English: "Disk creation failed", Match: []string{
		"建盘任务彻底失败，磁盘已置为异常并按实际占用重算用量",
		"建盘任务已失败，磁盘置为异常",
	}},

	// ---- iSCSI ----
	{ID: "iscsi.target.unavailable", English: "iSCSI target service unavailable; disk publishing and mounting will fail", Match: []string{
		"iSCSI 目标服务器不可用：磁盘发布与挂载将失败",
	}},
	{ID: "iscsi.target.reconcile_failed", English: "Failed to reconcile the iSCSI target", Match: []string{
		"收敛 iSCSI 目标失败",
		"停用子盘目标失败",
		"停用目标失败",
		"删除目标失败",
		"删除目标记录失败",
		"解除目标映射失败",
		"清理目标记录失败",
		"绑定池位盘的 iSCSI 目标失败（挂载时会再次认领）",
		"预创建 iSCSI 目标失败，用户挂载时将现场下发",
		"预创建 iSCSI 目标失败：读取存储库出错",
		"入队 iSCSI 目标修复任务失败",
		"该目标的修复任务刚失败过，进入冷却期，暂不重复入队",
	}},
	{ID: "iscsi.session.force_logout", English: "Forced logout of the iSCSI session", Match: []string{
		"已对该会话执行强制登出（平台可能为降级动作）",
		"强制登出会话失败，退回停用目标",
		"踢下线失败，仍标记租约为撤销",
	}},
	{ID: "iscsi.temp_share.failed", English: "Failed to reconcile temporary parent sharing", Match: []string{
		"收敛临时共享失败",
		"未装配 iSCSI 服务，跳过临时共享目标拆除",
		"未装配 iSCSI 服务，跳过平台侧目标拆除",
		"撤销母盘临时挂载租约失败（继续拆除目标）",
	}},

	// ---- 上传 ----
	{ID: "upload.verify.completed", English: "Upload verification completed", Match: []string{
		"上传核对完成",
	}},
	{ID: "upload.rollback_failed", English: "Upload failed and rollback was incomplete", Match: []string{
		"上传建库失败，已回滚存储库与母盘状态",
		"回滚上传建库状态失败",
	}},
	{ID: "upload.staging.cleanup_failed", English: "Failed to clean upload staging", Match: []string{
		"清理上传暂存目录失败",
		"清理上传暂存目录失败（已交给 GC 兜底）",
		"清理旧上传暂存目录失败",
		"回收已结束会话的暂存目录失败",
		"清空上传暂存路径失败",
		"查询待回收的上传暂存目录失败",
	}},
	{ID: "upload.manifest.mismatch", English: "Uploaded file tree does not match the manifest; affected chunks were invalidated", Match: []string{
		"文件树与清单不符，已作废对应分块等待重传",
	}},

	// ---- 任务 ----
	{ID: "job.failed", English: "Job failed: {type}", Match: []string{
		"任务失败且已达最大尝试次数",
	}},
	{ID: "job.retry", English: "Job failed and will be retried: {type}", Match: []string{
		"任务失败，将重试",
	}},
	{ID: "job.canceled", English: "Job canceled: {type}", Match: []string{
		"任务被取消",
		"取消排队中的任务",
		"取消正在执行的任务",
		"任务在等待锁时被取消",
		"任务在拿到锁后被取消",
		"任务在执行前已被取消",
		"任务未在执行器内，直接置为已取消",
	}},
	{ID: "job.panic", English: "Job panicked", Match: []string{
		"任务执行 panic",
	}},
	{ID: "job.unregistered", English: "Unregistered job type: {type}", Match: []string{
		"未注册的任务类型",
	}},

	// ---- 认证 / API ----
	{ID: "auth.login.succeeded", English: "User logged in", Match: []string{
		"用户登录成功",
	}},
	{ID: "auth.cert.issued", English: "Client certificate issued", Match: []string{
		"客户端证书注册成功",
		"已为用户签发客户端证书（客户端提交 CSR，私钥不外传）",
		"已为用户签发客户端证书（服务端生成密钥对）",
	}},
	{ID: "auth.cert.issue_failed", English: "User created, but automatic client certificate issuance failed", Match: []string{
		"用户已创建，但自动签发客户端证书失败",
	}},
	{ID: "auth.admin_changed", English: "Built-in super admin state changed by configuration", Match: []string{
		"内置超级管理员已按配置停用：该角色账号将无法登录",
		"内置超级管理员已按配置恢复启用",
	}},
	{ID: "api.request.failed", English: "Request failed", Match: []string{
		"请求处理失败",
	}},
	{ID: "api.request.panic", English: "Request handler panicked", Match: []string{
		"请求处理发生 panic",
		"SSE 事件流处理 panic，已终止该连接",
	}},
	{ID: "api.response.write_failed", English: "Failed to write HTTP response", Match: []string{
		"写响应失败",
		"写更新清单响应失败",
		"写更新签名响应失败",
		"写本地响应失败",
	}},
	{ID: "api.file.open_failed", English: "Failed to open file for download", Match: []string{
		"打开更新产物失败",
		"打开母盘失败",
	}},

	// ---- 对账 ----
	{ID: "reconcile.failed", English: "Reconciliation failed", Match: []string{
		"启动对账失败",
		"核对存储底层卷失败",
		"查询分配列表失败",
		"查询磁盘列表失败",
		"查询 iSCSI 目标列表失败",
		"重置运行中任务失败",
		"标记过期租约失败",
		"读取存储失败，跳过孤儿文件扫描",
		"删除孤儿文件前刷新磁盘登记失败，跳过本轮",
		"查询磁盘登记失败，本轮跳过孤儿虚拟盘清理",
	}},

	// ---- 客户端代理 ----
	{ID: "agent.started", English: "Vault Agent started", Match: []string{
		"Vault-Agent 已启动",
	}},
	{ID: "agent.stopped", English: "Vault Agent stopped", Match: []string{
		"Vault-Agent 已停止",
	}},
	{ID: "agent.mount.failed", English: "Mount failed", Match: []string{
		"挂载失败",
	}},
	{ID: "agent.unmount.failed", English: "Unmount failed; record kept for retry", Match: []string{
		"卸载未完成，保留记录供重试",
		"退出时卸载失败",
		"响应踢下线时卸载失败",
	}},
	{ID: "agent.server.unreachable", English: "Server connection failed; retry scheduled", Match: []string{
		"自动重连服务端失败",
		"手动重连服务端失败",
		"服务端事件流中断，稍后重试",
	}},
	{ID: "agent.session.expired", English: "Server session expired; re-authenticated with local certificate", Match: []string{
		"服务端会话已失效（可能由服务端重启导致），已用本地证书自动重新登录",
	}},
	{ID: "agent.iscsi.not_ready", English: "iSCSI initiator is not ready; mount operations will fail", Match: []string{
		"本机 iSCSI 发起端未就绪：挂载类操作将失败",
	}},

	// ---- 兜底：未收录的 Warn / Error ----
	// 输出英文兜底消息 + 英文 detail（底层原因/错误码/字段摘要），不写中文原句；
	// 需要看原文时把 log.level 调到 debug（此时全部原始日志原样输出）。
	{ID: "system.warning", English: "Unexpected warning"},
	{ID: "system.error", English: "Unexpected error"},
}

var (
	eventByID     = make(map[string]logEvent, len(logEvents))
	eventBySource = make(map[string]logEvent, len(logEvents)*4)
)

func init() {
	for _, ev := range logEvents {
		eventByID[ev.ID] = ev
		for _, match := range ev.Match {
			eventBySource[match] = ev
		}
	}
}

// lookupEvent 先按旧文案精确匹配，再按事件 ID 匹配（兼容新代码直接传 ID）。
func lookupEvent(msg string) (logEvent, bool) {
	if ev, ok := eventBySource[msg]; ok {
		return ev, true
	}
	ev, ok := eventByID[msg]
	return ev, ok
}

var templateVar = regexp.MustCompile(`\{([A-Za-z0-9_]+)\}`)

// renderTemplate 用日志字段填充英文模板；字段缺失时保留占位符，便于发现模板与字段不匹配。
func renderTemplate(template string, values map[string]string) string {
	if len(values) == 0 || !strings.Contains(template, "{") {
		return template
	}
	return templateVar.ReplaceAllStringFunc(template, func(match string) string {
		name := match[1 : len(match)-1]
		if v, ok := values[name]; ok {
			return v
		}
		return match
	})
}
