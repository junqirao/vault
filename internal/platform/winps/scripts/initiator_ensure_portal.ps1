<#
.SYNOPSIS
    幂等地为目标门户（TargetPortal）添加到本地 iSCSI 发起端。

.NOTES
    由 Go 侧以 `-Address <ip> -Port 3260` 调用，参数不做字符串拼接。
    已存在同一「地址 + 端口」的门户时直接返回 action=exists，不重复添加。

    ⚠️ 需实测确认：New-IscsiTargetPortal / Get-IscsiTargetPortal 的参数名
    （TargetPortalAddress / TargetPortalPortNumber）与返回对象的属性名。
#>

param(
    [string]$Address,
    [int]$Port = 3260
)

$ErrorActionPreference = 'Stop'
$ProgressPreference = 'SilentlyContinue'
try { [Console]::OutputEncoding = [System.Text.Encoding]::UTF8 } catch { }

try {
    if (-not $Address) { throw 'Address 参数不能为空' }
    if ($Port -le 0) { $Port = 3260 }

    # WMI 提供程序会瞬时拒绝："The target does not have enough resources to process the
    # given request." —— 门户列表正被别的进程写入、或上一轮会话刚释放时都可能出现
    # （真实现场：并发挂载 4 个库，3 个在同一毫秒内失败在此）。
    # 该文案会随系统语言本地化，所以**不按错误文案匹配**：统一退避重试。
    #
    # 重试 5 次、退避 300/600/1200/2400ms（合计约 4.5s）：现场表明这类拒绝是"挤出式"的
    # —— 同时有多个挂载在动门户时，前面的失败会拖到下一个窗口才恢复。3 次（0.75s）不够。
    #
    # 注意 **Get 也在重试范围内**：原来它带 -ErrorAction SilentlyContinue，瞬时失败会被
    # 吞成"门户不存在"，接着 New 又失败（门户其实在），错误信息反而更误导。
    $delays = @(300, 600, 1200, 2400)
    $lastError = $null
    $action = 'created'
    for ($attempt = 1; $attempt -le ($delays.Count + 1); $attempt++) {
        try {
            $existing = @(Get-IscsiTargetPortal -ErrorAction Stop | Where-Object {
                $_.TargetPortalAddress -eq $Address -and [int]$_.TargetPortalPortNumber -eq $Port
            })
            if ($existing.Count -gt 0) {
                $action = 'exists'
            } else {
                New-IscsiTargetPortal -TargetPortalAddress $Address -TargetPortalPortNumber $Port -ErrorAction Stop | Out-Null
                $action = 'created'
            }
            $lastError = $null
            break
        } catch {
            $lastError = $_
            if ($attempt -le $delays.Count) {
                Start-Sleep -Milliseconds $delays[$attempt - 1]
            }
        }
    }
    if ($lastError) { throw $lastError }

    [pscustomobject]@{
        ok      = $true
        action  = $action
        address = $Address
        port    = $Port
    } | ConvertTo-Json -Compress -Depth 5
} catch {
    [pscustomobject]@{
        ok      = $false
        reason  = 'ensure_portal_failed'
        message = $_.Exception.Message
    } | ConvertTo-Json -Compress -Depth 5
    exit 1
}
