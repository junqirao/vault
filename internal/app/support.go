package app

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"vault/internal/config"
	"vault/internal/domain"
	"vault/internal/store"
)

// auditWriteTimeout 是审计落库的独立超时。
//
// 审计是旁路记录，不应该被业务操作的 context 牵连：SteamCMD 超时、客户端断开、
// 长任务到达 deadline 时，业务 ctx 已经结束，但这条审计仍然应该写进去。
const auditWriteTimeout = 5 * time.Second

// writeAudit 落一条审计；失败仅告警，不影响主流程（避免审计异常导致业务失败）。
func (d Deps) writeAudit(ctx context.Context, entry *domain.AuditLog) {
	if d.Store == nil {
		return
	}
	// 关键：审计写入必须脱离调用方 ctx 的取消/超时。
	// context.WithoutCancel 保留 ctx 里的值（trace/principal 等），但不再继承取消信号；
	// 再套一个独立超时，避免数据库异常时把调用方永久挂住。
	base := context.Background()
	if ctx != nil {
		base = context.WithoutCancel(ctx)
	}
	auditCtx, cancel := context.WithTimeout(base, auditWriteTimeout)
	defer cancel()
	if err := d.Store.AppendAudit(auditCtx, entry); err != nil {
		if d.Log == nil {
			return
		}
		// 审计超时/取消不是业务错误，降级为 debug，避免每次长任务超时都多一条
		// "写入审计失败: context canceled" 的伴生日志。
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			d.Log.Debug("写入审计超时（已忽略）", "action", entry.Action,
				"resource", entry.Resource, "error", err)
			return
		}
		d.Log.Warn("写入审计失败", "action", entry.Action, "resource", entry.Resource, "error", err)
	}
}

// audit 写入一条域级审计记录。
func (d Deps) audit(ctx context.Context, userID, action, resource, detail, result string) {
	d.writeAudit(ctx, &domain.AuditLog{
		UserID:   userID,
		Action:   action,
		Resource: resource,
		Detail:   detail,
		Result:   result,
	})
}

// AuditWithIP 写入带来源 IP 的审计记录（HTTP 层使用）。
func (d Deps) AuditWithIP(ctx context.Context, userID, action, resource, detail, ip, result string) {
	d.writeAudit(ctx, &domain.AuditLog{
		UserID:   userID,
		Action:   action,
		Resource: resource,
		Detail:   detail,
		IP:       ip,
		Result:   result,
	})
}

// RawConfig 返回当前生效的原始配置快照（供 HTTP 层做只读展示）。
func (d Deps) RawConfig() config.Config { return d.raw() }

// Audit 写入审计记录（导出给 HTTP 层复用，语义与内部 audit 一致）。
func (d Deps) Audit(ctx context.Context, userID, action, resource, detail, result string) {
	d.audit(ctx, userID, action, resource, detail, result)
}

// sizing 依据当前配置构造 VHDX 容量计算参数（见 5.2）。
func (d Deps) sizing() domain.Sizing {
	raw := d.raw()
	s := domain.DefaultSizing()
	// 粒度按 **MB** 计量（原 GB 字段已废弃，见 config.Storage.SizeGranularityMB）。
	if raw.Storage.SizeGranularityMB > 0 {
		s.GranularityBytes = raw.Storage.SizeGranularityMB << 20
	}
	if raw.Storage.SizeReservePermille > 0 {
		s.ReservePermille = raw.Storage.SizeReservePermille
	}
	return s
}

// decodePayload 解析任务 payload；解析失败返回不可重试的语义错误由调用方决定。
func decodePayload(raw string, v any) error {
	if raw == "" {
		return nil
	}
	return json.Unmarshal([]byte(raw), v)
}

// deref 安全解引用字符串指针。
func deref(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}

// deleteAllocationIfExists 删除分配记录；记录不存在视为成功（幂等）。
func deleteAllocationIfExists(ctx context.Context, st *store.Store, id string) error {
	if id == "" {
		return nil
	}
	if err := st.DeleteAllocation(ctx, id); err != nil && !isNotFound(err) {
		return err
	}
	return nil
}

// PermAtLeast 判断是否具备不低于 min 的权限等级。
func PermAtLeast(have, min domain.Permission) bool {
	rank := func(p domain.Permission) int {
		switch p {
		case domain.PermManage:
			return 3
		case domain.PermMount:
			return 2
		case domain.PermRead:
			return 1
		default:
			return 0
		}
	}
	return rank(have) >= rank(min)
}
