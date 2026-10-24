package app

import (
	"context"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"vault/internal/domain"
)

// 孤儿磁盘文件 = 存储根 disks/ 目录下**未被数据库登记**的 .vhdx。
//
// 产生原因都是真实发生过的：建库/派生在"落盘之后、写库之前"失败、管理员手工删过记录、
// 数据库被还原到更早的时间点等。
//
// 策略是**对账自动清理**：reconcile.go 发现后直接删除，不再只打日志、也不再搬进
// meta/orphan。存储根之下由系统全权管理，未登记的东西就是垃圾；管理端不再提供
// 「孤儿磁盘」页面。

// 孤儿文件分类（与对账报告里的 kind 一致）。
const (
	OrphanKindDiff   = "diff"
	OrphanKindParent = "parent"
	OrphanKindOther  = "other"
)

// orphanScanSkipOtherPlatform 说明"为什么这台机器没有孤儿文件可扫"。
//
// 本扫描的前提是"磁盘 = storages.path 目录下的 .vhdx 文件"，只对 Windows 成立；
// Linux 上磁盘是 LVM thin LV（不在 storages.path 目录里）。
const orphanScanSkipOtherPlatform = "orphan_vhdx_scan: 仅适用于 Windows 的 VHDX 文件布局，当前平台已跳过"

// OrphanFile 是一个未被数据库登记的磁盘文件（仅供对账内部使用）。
type OrphanFile struct {
	// Path 绝对路径。
	Path string
	// Root 该文件所属的存储根。
	Root string
	// Kind diff / parent / other。
	Kind string
	// SizeBytes 文件长度（字节，逻辑大小）。
	SizeBytes int64
	// ModifiedAt 文件最后修改时间（毫秒）。
	ModifiedAt int64
}

// disksDirOf 解析某个根下的"磁盘目录"（默认 disks/）。
//
// 返回 ok=false 的两种情况都表示"这个根不适用文件式磁盘布局"：
//   - 配置里没有 disks 子目录名；
//   - 解析结果就是根本身（没配 disks_dir 或配成 "."）。
//
// 第二种必须挡住：那意味着"根下所有 .vhdx 都算磁盘文件"，而根里还有 meta/、staging/
// 等同样放 .vhdx 的地方。删除接口的白名单绝不能是"整个根"，宁可不扫/不删。
func (a *App) disksDirOf(guard domain.PathGuard) (string, bool) {
	rel := strings.TrimSpace(a.raw().Storage.DisksDir)
	if rel == "" {
		return "", false
	}
	dir, err := guard.Resolve(rel)
	if err != nil || strings.EqualFold(filepath.Clean(dir), filepath.Clean(guard.Root)) {
		return "", false
	}
	return dir, true
}

// registeredDiskPaths 返回数据库里全部磁盘的 VHDX 路径键（pathKeyLocal 归一，大小写不敏感）。
func (a *App) registeredDiskPaths(ctx context.Context) (map[string]bool, error) {
	disks, err := a.Store.ListAllDisks(ctx)
	if err != nil {
		return nil, err
	}
	out := make(map[string]bool, len(disks))
	for i := range disks {
		out[pathKeyLocal(disks[i].VHDXPath)] = true
	}
	return out, nil
}

// collectOrphanFiles 扫描各根 disks 目录下未被登记的文件（纯扫描，不做任何变更）。
//
// 互为父子的根只扫上游那一个，避免同一区域被重复报告；返回的 files 按路径排序。
// checked 是扫过的 .vhdx 总数（含已登记的），skipped 是未能扫描的位置说明。
func (a *App) collectOrphanFiles(ctx context.Context, guards domain.PathGuardSet,
	dbPaths map[string]bool, log *slog.Logger) (files []OrphanFile, checked int, skipped []string) {
	scanned := make([]string, 0, guards.Len())
	for _, guard := range guards.Guards() {
		if ctx.Err() != nil {
			break
		}
		if isUnderAny(guard.Root, scanned) {
			log.Debug("跳过被上游根覆盖的白名单根", "root", guard.Root)
			continue
		}
		scanned = append(scanned, guard.Root)

		disksRoot, ok := a.disksDirOf(guard)
		if !ok {
			skipped = append(skipped, "orphan_vhdx_scan: 根 "+guard.Root+" 未配置 disks 子目录，已跳过")
			continue
		}
		if _, statErr := os.Stat(disksRoot); statErr != nil {
			// 根下还没有 disks/（新配的存储）：不是问题，跳过即可，不必报。
			continue
		}
		_ = filepath.Walk(disksRoot, func(p string, fi os.FileInfo, walkErr error) error {
			if walkErr != nil || fi == nil || fi.IsDir() {
				return nil
			}
			if ctx.Err() != nil {
				return fs.SkipAll
			}
			if !strings.EqualFold(filepath.Ext(p), ".vhdx") {
				return nil
			}
			checked++
			if dbPaths[pathKeyLocal(p)] {
				return nil
			}
			files = append(files, OrphanFile{
				Path:       p,
				Root:       guard.Root,
				Kind:       orphanKindOf(p, disksRoot),
				SizeBytes:  fi.Size(),
				ModifiedAt: fi.ModTime().UnixMilli(),
			})
			return nil
		})
	}
	sort.Slice(files, func(i, j int) bool { return files[i].Path < files[j].Path })
	return files, checked, skipped
}

// orphanKindOf 按文件在 disks/ 下的位置分类：diffs/ 下是差异盘、parents/ 下是母盘，其余归 other。
func orphanKindOf(p, disksRoot string) string {
	rel := strings.ToLower(filepath.ToSlash(strings.TrimPrefix(p, disksRoot)))
	switch {
	case strings.Contains(rel, "/diffs/") || strings.HasPrefix(rel, "/diffs/"):
		return OrphanKindDiff
	case strings.Contains(rel, "/parents/") || strings.HasPrefix(rel, "/parents/"):
		return OrphanKindParent
	default:
		return OrphanKindOther
	}
}

// orphanKindLabel 返回分类的中文名（用于对账报告措辞）。
func orphanKindLabel(kind string) string {
	switch kind {
	case OrphanKindDiff:
		return "差异盘"
	case OrphanKindParent:
		return "母盘"
	default:
		return "VHDX"
	}
}
