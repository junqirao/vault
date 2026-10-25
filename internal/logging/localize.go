package logging

import (
	"context"
	"fmt"
	"log/slog"
	"sort"
	"strconv"
	"strings"
	"time"
)

// localizeHandler 把日志收敛到 logEvents 并输出英文正文 + msg_key。
//
// 为什么包在 JSONHandler 外面而不是改每个调用点：现有代码有数百处 slog 调用，
// 逐处改既容易漏、也让业务代码充斥日志文案。这里统一做：
//   - 命中 logEvents 的消息：msg 渲染为英文模板，并追加 msg_key；
//   - 未命中的 Info：默认丢弃（调试时把 log.level 设为 debug 即可原样看到）；
//   - 未命中的 Warn/Error：收敛为 system.warning / system.error，并附带英文 detail
//     （底层错误原因 / 稳定错误码 / 字段摘要；不参与多语言渲染）。
type localizeHandler struct {
	inner       slog.Handler
	attrs       map[string]string
	keepUnknown bool
}

func newLocalizeHandler(inner slog.Handler, keepUnknown bool) slog.Handler {
	return &localizeHandler{inner: inner, keepUnknown: keepUnknown}
}

func (h *localizeHandler) Enabled(ctx context.Context, level slog.Level) bool {
	return h.inner.Enabled(ctx, level)
}

func (h *localizeHandler) Handle(ctx context.Context, r slog.Record) error {
	if ev, ok := lookupEvent(r.Message); ok {
		values := h.values(r)
		translated := r.Clone()
		translated.Message = renderTemplate(ev.English, values)
		translated.AddAttrs(slog.String("msg_key", ev.ID))
		// detail 只放英文：优先底层错误原因/稳定错误码；分组事件（同一事件合并了多条旧文案）
		// 额外把原日志翻译成可读英文，方便看出具体是哪一个失败。
		detail := englishErrorDetail(values)
		if detail == "" && len(ev.Match) > 1 {
			detail = translateDetail(r.Message)
		}
		if detail != "" {
			translated.AddAttrs(slog.String("detail", detail))
		}
		return h.inner.Handle(ctx, translated)
	}

	// debug 级别保留全部原始日志（排查底层问题时的逃生口）。
	if h.keepUnknown {
		return h.inner.Handle(ctx, r)
	}

	values := h.values(r)
	switch {
	case r.Level >= slog.LevelError:
		return h.handleFallback(ctx, r, eventByID["system.error"], values)
	case r.Level >= slog.LevelWarn:
		return h.handleFallback(ctx, r, eventByID["system.warning"], values)
	default:
		// 未收录的 Info/Debug：默认静默，避免日志页被低价值记录淹没。
		return nil
	}
}

func (h *localizeHandler) handleFallback(ctx context.Context, r slog.Record, ev logEvent, values map[string]string) error {
	translated := r.Clone()
	translated.Message = ev.English
	translated.AddAttrs(
		slog.String("msg_key", ev.ID),
		// detail 固定英文：优先底层错误原因，其次把原日志做技术词翻译；都没有时用
		// 字段摘要。它不参与多语言渲染（日志页按原样展示）。
		slog.String("detail", fallbackDetail(values, r.Message)),
	)
	return h.inner.Handle(ctx, translated)
}

func (h *localizeHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	values := make(map[string]string, len(h.attrs)+len(attrs))
	for k, v := range h.attrs {
		values[k] = v
	}
	collectAttrValues(values, "", attrs)
	return &localizeHandler{
		inner:       h.inner.WithAttrs(attrs),
		attrs:       values,
		keepUnknown: h.keepUnknown,
	}
}

func (h *localizeHandler) WithGroup(name string) slog.Handler {
	return &localizeHandler{
		inner:       h.inner.WithGroup(name),
		attrs:       h.attrs,
		keepUnknown: h.keepUnknown,
	}
}

// values 合并 With(...) 上的字段与本次调用的字段（本次调用优先）。
func (h *localizeHandler) values(r slog.Record) map[string]string {
	values := make(map[string]string, len(h.attrs)+r.NumAttrs())
	for k, v := range h.attrs {
		values[k] = v
	}
	r.Attrs(func(a slog.Attr) bool {
		collectAttrValues(values, "", []slog.Attr{a})
		return true
	})
	return values
}

// collectAttrValues 把 attrs 展开成"字段名 → 文本"，供英文模板使用。
//
// 组字段（如 apperr.Error 的 LogValue）展开为 error.code、error.field 这种点分名字；
// 顶层字段同时保留裸名，方便模板直接写 {type} / {version}。
func collectAttrValues(dst map[string]string, prefix string, attrs []slog.Attr) {
	for _, a := range attrs {
		a.Value = a.Value.Resolve()
		key := a.Key
		if prefix != "" {
			key = prefix + "." + key
		}
		if a.Value.Kind() == slog.KindGroup {
			collectAttrValues(dst, key, a.Value.Group())
			continue
		}
		text := attrText(a.Value)
		dst[key] = text
		if prefix == "" {
			dst[a.Key] = text
		}
	}
}

func attrText(v slog.Value) string {
	v = v.Resolve()
	switch v.Kind() {
	case slog.KindString:
		return v.String()
	case slog.KindInt64:
		return strconv.FormatInt(v.Int64(), 10)
	case slog.KindUint64:
		return strconv.FormatUint(v.Uint64(), 10)
	case slog.KindFloat64:
		return strconv.FormatFloat(v.Float64(), 'f', -1, 64)
	case slog.KindBool:
		return strconv.FormatBool(v.Bool())
	case slog.KindDuration:
		return v.Duration().String()
	case slog.KindTime:
		return v.Time().Format(time.RFC3339Nano)
	default:
		if any := v.Any(); any != nil {
			return fmt.Sprint(any)
		}
		return ""
	}
}

// fallbackDetail 为未收录事件生成一段**英文** detail（不参与多语言渲染）。
//
// 优先级：英文底层错误原因 > 原文本身已是英文 > 技术词翻译 > 稳定错误码 > 字段摘要。
// 这样 "Unexpected warning" 后面至少能看出"哪一步失败了"，而不是一句空泛的兜底。
func fallbackDetail(values map[string]string, msg string) string {
	if cause := strings.TrimSpace(values["error.cause"]); cause != "" && isASCII(cause) {
		return cause
	}
	if isASCII(msg) && strings.TrimSpace(msg) != "" {
		return strings.TrimSpace(msg)
	}
	if translated := translateDetail(msg); translated != "" {
		return translated
	}
	if code := strings.TrimSpace(values["error.code"]); code != "" {
		return code
	}
	keys := make([]string, 0, len(values))
	for key, value := range values {
		if strings.TrimSpace(value) == "" || strings.HasPrefix(key, "error.") {
			continue
		}
		keys = append(keys, key)
	}
	sort.Strings(keys)
	if len(keys) == 0 {
		return "unmapped log event"
	}
	parts := make([]string, 0, len(keys))
	for _, key := range keys {
		parts = append(parts, key+"="+values[key])
	}
	return strings.Join(parts, " ")
}

// englishErrorDetail 返回已知事件可用的英文 detail：优先 ASCII 底层原因，其次稳定错误码。
//
// 底层原因来自操作系统/平台时可能是本地化文本；这类不放进 detail（避免日志页出现
// 另一种语言），而是退回英文错误码。原始 cause 仍保留在 error 字段里供排查。
func englishErrorDetail(values map[string]string) string {
	if cause := strings.TrimSpace(values["error.cause"]); cause != "" && isASCII(cause) {
		return cause
	}
	return strings.TrimSpace(values["error.code"])
}

// isASCII 判断字符串是否只含 ASCII 字符（日志 detail 要求英文）。
func isASCII(s string) bool {
	for _, r := range s {
		if r > 127 {
			return false
		}
	}
	return true
}
