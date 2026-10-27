<#
.SYNOPSIS
    移除 iSCSI 目标门户（连同它的「已发现目标」缓存）。幂等。

.NOTES
    由 Go 侧以 `-PortalAddress <ip> -PortalPort 3260` 调用。

    目的：本系统是「用一次删一次」——卸载后不留门户与发现缓存，避免 iSCSI 发起程序里
    越积越多没用的条目（真实反馈："iSCSI 发起程序里一大堆没用的死连接"）。
    门户上仍有活动会话时删除会失败：如实回报，由上层决定（别的挂载还在用同一门户时也不该删）。

    最后一行才是 JSON。
#>

param(
    [string]$PortalAddress,
    [int]$PortalPort = 3260
)

$ErrorActionPreference = 'Stop'
$ProgressPreference = 'SilentlyContinue'
# Remove-IscsiTargetPortal 是 ShouldProcess cmdlet：非交互进程里弹确认会把调用卡死。
$ConfirmPreference = 'None'
try { [Console]::OutputEncoding = [System.Text.Encoding]::UTF8 } catch { }

try {
    if (-not $PortalAddress) { throw 'PortalAddress 参数不能为空' }
    if ($PortalPort -le 0) { $PortalPort = 3260 }

    $command = Get-Command -Name Remove-IscsiTargetPortal -ErrorAction SilentlyContinue
    if (-not $command) {
        [pscustomobject]@{
            ok      = $true
            action  = 'unsupported'
            message = '当前系统未提供 Remove-IscsiTargetPortal，未清理门户（需实测确认）'
        } | ConvertTo-Json -Compress -Depth 5
        exit 0
    }

    $portals = @(Get-IscsiTargetPortal -ErrorAction SilentlyContinue | Where-Object {
        $_.TargetPortalAddress -eq $PortalAddress -and [int]$_.TargetPortalPortNumber -eq $PortalPort
    })
    if ($portals.Count -eq 0) {
        [pscustomobject]@{
            ok            = $true
            action        = 'none'
            portal        = ($PortalAddress + ':' + $PortalPort)
        } | ConvertTo-Json -Compress -Depth 5
        exit 0
    }

    $errors = @()
    $iscsicli = Join-Path $env:SystemRoot 'System32\iscsicli.exe'
    foreach ($portal in $portals) {
        # 用管道传入门户对象，而不是回传 -TargetPortalAddress/-TargetPortalPortNumber：
        # 后者在部分系统上会因 CIM 参数类型不匹配直接失败（真实现场：
        # "参数“TargetPortalPortNumber”的类型不匹配"，门户永远删不掉，
        # 于是在 iSCSI 发起程序里越积越多）。
        # 与新增门户同理：WMI 偶尔瞬时拒绝（文案随语言本地化，不按文案匹配），短退避重试 2 次。
        $lastError = $null
        for ($attempt = 1; $attempt -le 2; $attempt++) {
            try {
                $portal | Remove-IscsiTargetPortal -Confirm:$false -ErrorAction Stop | Out-Null
                $lastError = $null
                break
            } catch {
                $lastError = $_
                if ($attempt -lt 2) { Start-Sleep -Milliseconds 250 }
            }
        }
        if (-not $lastError) { continue }

        # cmdlet 彻底失败时用 iscsicli 兜底：上面那个"CIM 参数类型不匹配"是 cmdlet 自身
        # 的缺陷（管道传对象同样会撞上），而 iscsicli 直接调 MSiSCSI 服务，没有这层校验。
        # 删没删掉由下面的复核决定，这里不看 iscsicli 的输出（本地化文本不可解析）。
        if (Test-Path $iscsicli) {
            & $iscsicli DeleteTargetPortal $PortalAddress ([string]$PortalPort) 2>&1 | Out-Null
            Start-Sleep -Milliseconds 200
        }
        $errors += $lastError.Exception.Message
    }

    # 复核：门户必须真的没了（避免"报成功却没删掉"）。
    $remaining = @(Get-IscsiTargetPortal -ErrorAction SilentlyContinue | Where-Object {
        $_.TargetPortalAddress -eq $PortalAddress -and [int]$_.TargetPortalPortNumber -eq $PortalPort
    })
    if ($remaining.Count -gt 0) {
        [pscustomobject]@{
            ok      = $false
            reason  = 'remove_portal_failed'
            message = ('仍有门户未移除：' + ($errors -join ' ; '))
            portal  = ($PortalAddress + ':' + $PortalPort)
        } | ConvertTo-Json -Compress -Depth 5
        exit 1
    }

    [pscustomobject]@{
        ok     = $true
        action = 'removed'
        portal = ($PortalAddress + ':' + $PortalPort)
    } | ConvertTo-Json -Compress -Depth 5
} catch {
    [pscustomobject]@{
        ok      = $false
        reason  = 'remove_portal_failed'
        message = $_.Exception.Message
    } | ConvertTo-Json -Compress -Depth 5
    exit 1
}
