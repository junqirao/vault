<#
.SYNOPSIS
    按容量（或唯一未占用候选）定位客户端侧 iSCSI 磁盘，返回磁盘号。

.NOTES
    由 Go 侧以 `-SizeBytes <n> -UsedDiskNumbers <json 数组>` 调用；参数只做字符串拼接。
    已占用磁盘（本代理根据挂载记录排除）会被排除，避免同一尺寸的多块盘互相抢占。

    ⚠️ 真实环境确认：仅凭「同尺寸 + 同 BusType=iSCSI」的磁盘无法精确区分，
    真实环境需要用 unique id（Get-Disk 的 UniqueId / SerialNumber / Path）来精确定位。
    本脚本在候选数 > 1 时先返回编号最小的候选，并通过 candidate_count 告知上层（上层会记 WARN）。

    ⚠️ 容量兜底（真实事故）：导入已有磁盘时，服务端早期版本会把**文件大小**当成标称容量。
    VHDX 是稀疏文件，这个值通常不是合法的扇区对齐值（真实日志：198870176638），
    与 Get-Disk 报告的磁盘大小永远不相等，库就会永久挂载失败。
    因此精确匹配不到时，如果**恰好只有一块未被占用的 iSCSI 磁盘**，就直接选它：
    这次挂载前刚登录的目标只可能是它。多块候选时绝不猜，把实际容量写进错误信息，
    让上层日志能直接看出"预期多少、现场是什么"。

    刚登录的 iSCSI 磁盘可能还不在存储缓存里，首次查不到时会刷新缓存再查一次。
#>

param(
    [long]$SizeBytes = 0,
    [string]$UsedDiskNumbers = '[]',
    [string]$RefreshCache = 'true'
)

$ErrorActionPreference = 'Stop'
$ProgressPreference = 'SilentlyContinue'
try { [Console]::OutputEncoding = [System.Text.Encoding]::UTF8 } catch { }

# 当前所有 iSCSI 磁盘（不做容量筛选），按磁盘号排序。
function Get-IscsiDisks {
    @(Get-Disk -ErrorAction SilentlyContinue | Where-Object { $_.BusType -eq 'iSCSI' } | Sort-Object Number)
}

# 在候选里按容量精确匹配；已占用的磁盘号必须排除。
function Find-IscsiDiskBySize {
    param(
        [object[]]$Disks,
        [long]$Size,
        [int[]]$Used
    )
    @($Disks | Where-Object {
        [long]$_.Size -eq $Size -and ($Used -notcontains [int]$_.Number)
    })
}

# 把磁盘列表拼成 "编号:容量"，只用于错误信息（本机诊断）。
function Format-Disks {
    param([object[]]$Disks)
    if (-not $Disks -or @($Disks).Count -eq 0) { return '无' }
    return (@($Disks | ForEach-Object { ('#{0}:{1}' -f [int]$_.Number, [long]$_.Size) }) -join ', ')
}

try {
    if ($SizeBytes -le 0) { throw 'SizeBytes 必须为正数' }

    # ⚠️ Windows PowerShell 5.1 的 ConvertFrom-Json 把 JSON 数组**整体当成一个对象**输出
    # （不展开元素），于是 `@(ConvertFrom-Json ... | Where-Object {...})` 里的 `$_`
    # 是**整个数组**，`[int]` 转换直接抛
    # "无法将 System.Object[] 转换为 System.Int32"。
    # 因此这里必须先把数组**展开**（逐个 @() 强制展开元素）。
    $used = @()
    if ($UsedDiskNumbers -and $UsedDiskNumbers -ne '') {
        $decoded = ConvertFrom-Json -InputObject $UsedDiskNumbers
        foreach ($item in @($decoded)) {
            if ($null -ne $item) { $used += [int]$item }
        }
    }

    $refreshNote = ''
    $disks = Get-IscsiDisks
    $candidates = @(Find-IscsiDiskBySize -Disks $disks -Size $SizeBytes -Used $used)
    if ($candidates.Count -eq 0 -and $RefreshCache -eq 'true') {
        # 刷新存储缓存需要管理员权限；刷新失败不应改变「是否找到磁盘」的判断。
        try { Update-HostStorageCache -ErrorAction Stop } catch { $refreshNote = '（刷新存储缓存失败：' + $_.Exception.Message + '）' }
        $disks = Get-IscsiDisks
        $candidates = @(Find-IscsiDiskBySize -Disks $disks -Size $SizeBytes -Used $used)
    }

    $disk = $null
    $sizeMatched = $false
    if ($candidates.Count -gt 0) {
        $disk = $candidates[0]
        $sizeMatched = $true
    } else {
        # 容量兜底：唯一未被占用的 iSCSI 磁盘。刚登录的目标只会是它；
        # 多于一块时宁可失败，也不要把别的库的盘挂上来。
        $unused = @($disks | Where-Object { $used -notcontains [int]$_.Number })
        if ($unused.Count -eq 1) { $disk = $unused[0] }
    }

    if ($null -eq $disk) {
        [pscustomobject]@{
            ok            = $false
            reason        = 'disk_not_found'
            expected_size = $SizeBytes
            message       = ("未找到容量为 $SizeBytes 字节的 iSCSI 磁盘" +
                $refreshNote +
                '；当前 iSCSI 磁盘（编号:容量）= ' + (Format-Disks -Disks $disks) +
                '；已占用编号 = ' + $(if ($used.Count -gt 0) { ($used -join ',') } else { '无' }))
        } | ConvertTo-Json -Compress -Depth 5
        exit 1
    }

    [pscustomobject]@{
        ok              = $true
        number          = [int]$disk.Number
        size            = [long]$disk.Size
        expected_size   = $SizeBytes
        size_mismatch   = (-not $sizeMatched)
        bus_type        = [string]$disk.BusType
        partition_style = [string]$disk.PartitionStyle
        is_offline      = [bool]$disk.IsOffline
        is_read_only    = [bool]$disk.IsReadOnly
        candidate_count = $(if ($sizeMatched) { $candidates.Count } else { 1 })
        location        = [string]$disk.Location
    } | ConvertTo-Json -Compress -Depth 5
} catch {
    [pscustomobject]@{
        ok      = $false
        reason  = 'find_iscsi_disk_failed'
        message = $_.Exception.Message
    } | ConvertTo-Json -Compress -Depth 5
    exit 1
}
