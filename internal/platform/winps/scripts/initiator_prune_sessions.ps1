<#
.SYNOPSIS
    清理 iSCSI 发起端里属于本系统、但已经没有对应挂载的残留会话与持久目标。

.NOTES
    由 Go 侧以 `-KeepJson '["<iqn>", ...]'` 调用（保留清单 = 当前仍在挂载的目标 IQN）。

    为什么需要：进程中途被杀 / 卸载失败会把会话留在发起端，它会一直重连（界面上就是一堆
    "死连接"），越积越多（真实反馈："iSCSI 发起程序里一大堆没用的死连接，用一次删一次"）。
    本脚本**只动本系统命名空间内的目标**（见 Test-IsVaultTarget），且不在保留清单里；绝不碰
    用户自己配的 iSCSI 目标或门户。

    断开失败（如仍有在线设备）只跳过、不报错；持久目标（收藏）用 iscsicli RemoveTarget
    尽力清除。除了会话，"已发现目标"缓存里本系统、且当前没连线的条目也会一并删除 ——
    这些 IsConnected=False 的历史目标是 Windows 打开资源管理器时挨个超时的根源。
    最后一行才是 JSON。
#>

param(
    [string]$KeepJson = '[]'
)

$ErrorActionPreference = 'Stop'
$ProgressPreference = 'SilentlyContinue'
$ConfirmPreference = 'None'
try { [Console]::OutputEncoding = [System.Text.Encoding]::UTF8 } catch { }

# 与 initiator_connect.ps1 / initiator_disconnect.ps1 中的同名函数保持一致：
# 平台会改写目标名，只能用短名做模糊匹配。
function Get-TargetShortName {
    param([string]$Iqn)
    $text = [string]$Iqn
    $idx = $text.LastIndexOf(':')
    if ($idx -ge 0 -and $idx -lt ($text.Length - 1)) { return $text.Substring($idx + 1) }
    return $text
}

function Test-SameTarget {
    param([string]$Left, [string]$Right)
    if (-not $Left -or -not $Right) { return $false }
    if ($Left -eq $Right) { return $true }
    $ls = Get-TargetShortName -Iqn $Left
    if ($ls -and $Right.IndexOf($ls, [System.StringComparison]::OrdinalIgnoreCase) -ge 0) { return $true }
    $rs = Get-TargetShortName -Iqn $Right
    if ($rs -and $Left.IndexOf($rs, [System.StringComparison]::OrdinalIgnoreCase) -ge 0) { return $true }
    return $false
}

# 只认本系统的目标：**按 IQN 命名空间判定**，短名前缀只作补充。
#
# 为什么不能只看短名前缀（真实工单）：历史版本的本机缓存代理用过
# iqn.2024-01.local.vault:cached-disk0 这类短名（既不是 vault- 也不是 vcache-），
# 只认前缀的写法会把它当成"用户自己的目标"，于是这条死连接每次启动都被跳过、
# 永远清不掉——用户的表现就是"每次开资源管理器都要卡几分钟"。
#
# 本系统的目标全部落在 *.vault 命名空间下：
#   iqn.2024-01.local.vault:vcache-<alloc>          本机缓存代理（v1，已废弃）
#   iqn.2024-01.local.vault:cached-disk0            本机缓存代理（更早的命名）
#   iqn.2024-01.com.vault:<repo>-<alloc>-target     服务端目标
#   iqn.1991-05.com.microsoft:win-xxx-iqn...com.vault:vault-...   平台改写后的服务端目标
# 用户自己配的目标不会用 .vault 这个域名，因此按命名空间识别既准确又不会误伤。
function Test-IsVaultTarget {
    param([string]$Iqn)
    $text = [string]$Iqn
    if (-not $text) { return $false }
    if ($text -match '(?i)\.vault:') { return $true }
    $short = Get-TargetShortName -Iqn $text
    if (-not $short) { return $false }
    $lower = $short.ToLowerInvariant()
    return $lower.StartsWith('vault-') -or $lower.StartsWith('vcache-') -or $lower.StartsWith('cached-disk')
}

# 用 iscsicli 登出一条会话（最后兜底）。
#
# 为什么需要：残留会话的目标往往已经不存在（本机缓存代理早就没了），
# Disconnect-IscsiTarget 会直接抛错，而 iscsicli LogoutTarget 只按 SessionId 登出、
# 不要求目标可达 —— 死连接只能靠它清掉（真实反馈：发起程序里一堆 IsConnected=False 的
# 会话，Disconnect 删不动，只能去控制面板手工移除）。
function Invoke-IscsiCliLogout {
    param([string]$SessionIdentifier)
    if (-not $SessionIdentifier) { return $false }
    if (-not (Get-Command -Name iscsicli -ErrorAction SilentlyContinue)) { return $false }
    $ids = @()
    foreach ($part in ($SessionIdentifier -split '-')) {
        $value = $part.Trim()
        if (-not $value) { continue }
        if ($value -match '(?i)^0x') { $ids += $value } else { $ids += ('0x' + $value) }
    }
    if ($ids.Count -eq 0) { return $false }
    try {
        & iscsicli LogoutTarget ($ids -join '-') 2>$null | Out-Null
        return $true
    } catch {
        return $false
    }
}

try {
    $keep = @()
    if ($KeepJson) {
        $decoded = ConvertFrom-Json -InputObject $KeepJson
        foreach ($item in @($decoded)) {
            if ($item) { $keep += [string]$item }
        }
    }

    $removed = 0
    $skipped = 0
    $sessions = @(Get-IscsiSession -ErrorAction SilentlyContinue)
    foreach ($session in $sessions) {
        $name = [string]$session.TargetNodeAddress
        if (-not (Test-IsVaultTarget -Iqn $name)) { continue }

        $kept = $false
        foreach ($k in $keep) {
            if (Test-SameTarget -Left $name -Right $k) { $kept = $true; break }
        }
        if ($kept) { continue }

        # 优先按 SessionIdentifier 断：同名的残留会话可能不止一条（同一个目标被登录过多次），
        # 按 NodeAddress 只会断开其中一条，剩下的继续重连。
        $sessionId = ''
        try { $sessionId = [string]$session.SessionIdentifier } catch { }

        $attempts = @()
        if ($sessionId) { $attempts += @{ SessionIdentifier = $sessionId } }
        $attempts += @{ NodeAddress = $name }

        $done = $false
        foreach ($splat in $attempts) {
            try {
                Disconnect-IscsiTarget @splat -Confirm:$false -ErrorAction Stop | Out-Null
                $done = $true
                break
            } catch { }
        }
        if (-not $done) { $done = Invoke-IscsiCliLogout -SessionIdentifier $sessionId }
        if ($done) { $removed++ } else { $skipped++ }
    }

    # 已发现目标缓存：Windows 枚举 iSCSI 设备时会挨个探测这些条目，**IsConnected=False 的历史目标
    # 每多一条就多一次超时**（真实反馈："没开客户端时打开资源管理器要卡几分钟"）。
    # 会话断了但目标还留在发现缓存里才是卡顿主因，所以这里单独再清一遍：只删本系统、当前没连线、
    # 且不在保留清单里的目标。仍在挂载的目标 IsConnected=True，不会被误删。
    $targetsRemoved = 0
    $hasGet = Get-Command -Name Get-IscsiTarget -ErrorAction SilentlyContinue
    $hasRemove = Get-Command -Name Remove-IscsiTarget -ErrorAction SilentlyContinue
    if ($hasGet -and $hasRemove) {
        foreach ($target in @(Get-IscsiTarget -ErrorAction SilentlyContinue)) {
            $node = ''
            try { $node = [string]$target.NodeAddress } catch { }
            if (-not $node) { continue }
            if (-not (Test-IsVaultTarget -Iqn $node)) { continue }

            $connected = $false
            try { $connected = ($target.IsConnected -eq $true) } catch { }
            if ($connected) { continue }

            $kept = $false
            foreach ($k in $keep) { if (Test-SameTarget -Left $node -Right $k) { $kept = $true; break } }
            if ($kept) { continue }

            try {
                Remove-IscsiTarget -NodeAddress $node -ErrorAction Stop | Out-Null
                $targetsRemoved++
            } catch { }
        }
    }

    # 门户清理：删掉**没有任何会话在用**的门户 —— 门户是「已发现目标」缓存的容器，
    # 不删它，Get-IscsiTarget 里就会一直堆着 IsConnected=False 的历史目标；Windows 枚举
    # iSCSI 设备时因此变慢（真实反馈：没开客户端时打开资源管理器要很久）。
    #
    # 只删"没有会话"的门户：仍在挂载的目标其会话会保留门户。若读不出会话所属门户
    # （老系统属性缺失），保守起见整段跳过，避免误删正在使用的门户。
    $portalsRemoved = 0
    $liveSessions = @(Get-IscsiSession -ErrorAction SilentlyContinue)
    $activePortals = @{}
    $mappedSessions = 0
    foreach ($session in $liveSessions) {
        $addr = ''
        try { $addr = [string]$session.TargetPortalAddress } catch { }
        if ($addr) {
            $port = 0
            try { $port = [int]$session.TargetPortalPortNumber } catch { }
            $activePortals[($addr.ToLowerInvariant() + ':' + $port)] = $true
            $mappedSessions++
        }
    }
    if ($mappedSessions -eq $liveSessions.Count -and (Get-Command -Name Get-IscsiTargetPortal -ErrorAction SilentlyContinue)) {
        foreach ($portal in @(Get-IscsiTargetPortal -ErrorAction SilentlyContinue)) {
            $addr = ''
            try { $addr = [string]$portal.TargetPortalAddress } catch { }
            if (-not $addr) { continue }
            $port = 0
            try { $port = [int]$portal.TargetPortalPortNumber } catch { }
            $key = $addr.ToLowerInvariant() + ':' + $port
            if ($activePortals.ContainsKey($key)) { continue }
            try {
                Remove-IscsiTargetPortal -TargetPortalAddress $addr -TargetPortalPortNumber $port -Confirm:$false -ErrorAction Stop | Out-Null
                $portalsRemoved++
            } catch { }
        }
    }

    # 持久目标（收藏）：本系统不使用，但历史版本可能留下，尽力清掉本系统的那些。
    $persistentRemoved = 0
    if (Get-Command -Name iscsicli -ErrorAction SilentlyContinue) {
        try {
            $listOutput = & iscsicli ListPersistentTargets 2>$null | Out-String
            foreach ($line in ($listOutput -split "`r?`n")) {
                $trimmed = $line.Trim()
                if (-not $trimmed) { continue }
                # 持久目标行里会带完整目标名（iqn...）；只取本系统的那些。
                $match = [regex]::Match($trimmed, 'iqn\.[^\s]+')
                if (-not $match.Success) { continue }
                $iqn = $match.Value
                if (-not (Test-IsVaultTarget -Iqn $iqn)) { continue }
                $kept = $false
                foreach ($k in $keep) { if (Test-SameTarget -Left $iqn -Right $k) { $kept = $true; break } }
                if ($kept) { continue }
                try {
                    & iscsicli RemoveTarget $iqn 2>$null | Out-Null
                    $persistentRemoved++
                } catch { }
            }
        } catch { }
    }

    [pscustomobject]@{
        ok                 = $true
        action             = 'pruned'
        removed            = $removed
        skipped            = $skipped
        persistent_removed = $persistentRemoved
        portals_removed    = $portalsRemoved
        targets_removed    = $targetsRemoved
        keep_count         = $keep.Count
    } | ConvertTo-Json -Compress -Depth 5
} catch {
    [pscustomobject]@{
        ok      = $false
        reason  = 'prune_failed'
        message = $_.Exception.Message
    } | ConvertTo-Json -Compress -Depth 5
    exit 1
}
