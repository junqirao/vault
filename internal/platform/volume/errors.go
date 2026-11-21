package volume

import (
	"errors"
	"net/http"

	"vault/internal/apperr"
	"vault/internal/platform/winps"
)

// 客户端挂载相关错误码。
const (
	// CodeIscsiDiskNotFound 未找到符合容量条件的 iSCSI 磁盘。
	CodeIscsiDiskNotFound = "platform.iscsi_disk_not_found"
	// CodeNoPartition 磁盘上没有可挂载的分区。
	CodeNoPartition = "platform.no_partition"
	// CodeMountFailed 挂载点分配失败。
	CodeMountFailed = "platform.mount_failed"
	// CodeMountDirNotFound 目录挂载点不存在（或不是目录）。
	CodeMountDirNotFound = "platform.mount_dir_not_found"
	// CodeMountDirNotEmpty 目录挂载点非空。
	CodeMountDirNotEmpty = "platform.mount_dir_not_empty"
	// CodeMountDirNotNTFS 目录挂载点所在卷不是 NTFS。
	CodeMountDirNotNTFS = "platform.mount_dir_not_ntfs"
)

// ErrIscsiDiskNotFound 未找到匹配的 iSCSI 磁盘。
func ErrIscsiDiskNotFound() *apperr.Error {
	return apperr.New(CodeIscsiDiskNotFound, http.StatusNotFound)
}

// ErrNoPartition 磁盘上没有可挂载的分区。
func ErrNoPartition() *apperr.Error {
	return apperr.New(CodeNoPartition, http.StatusConflict)
}

// ErrMountDirNotFound 目录挂载点不存在或不是目录。
func ErrMountDirNotFound(dir string) *apperr.Error {
	return apperr.New(CodeMountDirNotFound, http.StatusBadRequest).WithArg("path", dir)
}

// ErrMountDirNotEmpty 目录挂载点非空（Add-PartitionAccessPath 要求空目录）。
func ErrMountDirNotEmpty(dir string) *apperr.Error {
	return apperr.New(CodeMountDirNotEmpty, http.StatusConflict).WithArg("path", dir)
}

// ErrMountDirNotNTFS 目录挂载点所在卷不是 NTFS。
func ErrMountDirNotNTFS(dir string) *apperr.Error {
	return apperr.New(CodeMountDirNotNTFS, http.StatusBadRequest).WithArg("path", dir)
}

// mappedReasonError 把脚本通过 {"ok":false,"reason":"..."} 上报的失败原因映射为更精确的业务错误。
//
// 未命中映射时返回 nil，调用方应返回原始错误（保留 platform.ps_failed 语义）。
//
// 映射时保留原始脚本错误为 Cause：ScriptError.Error() 只含稳定的 reason/step（不含原始
// 报错文本），所以不影响对外文案；而 agent 的 mountErrorDetailOf 能通过 errors.As
// 取到脚本自报的 Message（例如"现场 iSCSI 磁盘编号:容量"），挂载失败时才有得诊断。
func mappedReasonError(err error, mappings map[string]*apperr.Error) error {
	var scriptErr *winps.ScriptError
	if !errors.As(err, &scriptErr) {
		return nil
	}
	if mapped, ok := mappings[scriptErr.Reason]; ok {
		return mapped.WithCause(err)
	}
	return nil
}
