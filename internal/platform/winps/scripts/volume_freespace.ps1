<#
.SYNOPSIS
    查询指定路径所在卷的可用空间与总空间。

.NOTES
    优先用 Storage 模块的 Get-Volume -FilePath 定位所在卷（该参数在部分版本上需实测确认）；
    失败时回退到 .NET DriveInfo（要求路径位于某个盘符卷上）。
#>

param(
    [string]$Path
)

$ErrorActionPreference = 'Stop'
$ProgressPreference = 'SilentlyContinue'
try { [Console]::OutputEncoding = [System.Text.Encoding]::UTF8 } catch { }

# 识别承载该卷的物理磁盘是 HDD 还是 SSD。
#
# Get-Disk.MediaType 在新版 Windows 上直接给出 "SSD" / "HDD"，旧版可能只有 BusType，
# 因此再做一次 NVMe 兜底。识别不出来返回 unknown —— 调用方按"未知"展示，不猜测。
function Get-MediaType {
    param(
        [string]$DriveLetter,
        [string]$Root
    )
    try {
        $letter = $DriveLetter
        if (-not $letter -and $Root) {
            $letter = $Root.TrimEnd('\').TrimEnd(':')
        }
        if (-not $letter) { return 'unknown' }
        $partition = Get-Partition -DriveLetter $letter -ErrorAction Stop | Select-Object -First 1
        if (-not $partition) { return 'unknown' }
        $disk = Get-Disk -Number $partition.DiskNumber -ErrorAction Stop
        $media = [string]$disk.MediaType
        if ($media -match 'SSD') { return 'ssd' }
        if ($media -match 'HDD') { return 'hdd' }
        if ([string]$disk.BusType -match 'NVMe') { return 'ssd' }
    } catch { }
    return 'unknown'
}

try {
    if (-not $Path) { throw 'Path 参数不能为空' }

    $volume = $null
    try { $volume = Get-Volume -FilePath $Path -ErrorAction Stop } catch { $volume = $null }

    if ($volume -and $volume.Size) {
        $mediaType = Get-MediaType -DriveLetter ([string]$volume.DriveLetter) -Root ''
        [pscustomobject]@{
            ok            = $true
            free          = [long]$volume.SizeRemaining
            total         = [long]$volume.Size
            file_system   = [string]$volume.FileSystem
            drive_letter  = [string]$volume.DriveLetter
            media_type    = $mediaType
            source        = 'Get-Volume'
        } | ConvertTo-Json -Compress -Depth 5
    } else {
        $resolved = Resolve-Path -LiteralPath $Path -ErrorAction Stop
        $root = [System.IO.Path]::GetPathRoot($resolved.ProviderPath)
        if (-not $root) { throw "无法确定 $Path 所在的卷根目录" }
        $drive = New-Object System.IO.DriveInfo($root)
        if (-not $drive.IsReady) { throw "卷不可用：$root" }
        $mediaType = Get-MediaType -DriveLetter '' -Root $root
        [pscustomobject]@{
            ok           = $true
            free         = [long]$drive.AvailableFreeSpace
            total        = [long]$drive.TotalSize
            file_system  = [string]$drive.DriveFormat
            drive_letter = $root.TrimEnd('\')
            media_type   = $mediaType
            source       = 'DriveInfo'
        } | ConvertTo-Json -Compress -Depth 5
    }
} catch {
    [pscustomobject]@{
        ok      = $false
        reason  = 'freespace_failed'
        message = $_.Exception.Message
    } | ConvertTo-Json -Compress -Depth 5
    exit 1
}
