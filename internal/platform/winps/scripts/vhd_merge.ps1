<#
.SYNOPSIS
    把差异盘合并进母盘（离线操作）。

.DESCRIPTION
    走 Hyper-V 模块的 Merge-VHD：把 Path（子差异盘）的改动写进 DestinationPath（母盘）。
    合并会改写母盘内容，调用方必须先把同一母盘下的其它差异盘清理掉，并确保两张盘
    都没有被挂载或被 iSCSI 打开。

.NOTES
    Merge-VHD 是离线操作；合并完成后子盘不再有效，这里会尽力删除子盘文件。
    脚本最后一行才是 JSON，因此即使有 verbose 输出也不会影响上层解析。
#>

param(
    [string]$Path,
    [string]$DestinationPath
)

$ErrorActionPreference = 'Stop'
$ProgressPreference = 'SilentlyContinue'
try { [Console]::OutputEncoding = [System.Text.Encoding]::UTF8 } catch { }

try {
    if (-not $Path) { throw 'Path 参数不能为空' }
    if (-not $DestinationPath) { throw 'DestinationPath 参数不能为空' }
    if (-not (Test-Path -LiteralPath $Path)) { throw "差异盘不存在：$Path" }
    if (-not (Test-Path -LiteralPath $DestinationPath)) { throw "母盘不存在：$DestinationPath" }
    if (-not (Get-Command -Name Merge-VHD -ErrorAction SilentlyContinue)) {
        throw '缺少 Hyper-V 模块（Merge-VHD），无法合并差异盘'
    }

    Merge-VHD -Path $Path -DestinationPath $DestinationPath -Force -ErrorAction Stop

    # 合并成功后子盘已不再有效；删除失败不影响合并结果（上层还会再兜一次 Delete）。
    if (Test-Path -LiteralPath $Path) {
        Remove-Item -LiteralPath $Path -Force -ErrorAction SilentlyContinue
    }

    [pscustomobject]@{
        ok              = $true
        path            = $Path
        destination     = $DestinationPath
        child_removed   = -not (Test-Path -LiteralPath $Path)
    } | ConvertTo-Json -Compress -Depth 5
} catch {
    [pscustomobject]@{
        ok      = $false
        reason  = 'merge_failed'
        message = $_.Exception.Message
    } | ConvertTo-Json -Compress -Depth 5
    exit 1
}
