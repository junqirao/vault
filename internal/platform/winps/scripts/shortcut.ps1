<#
.SYNOPSIS
    创建或删除存储库挂载点的快捷方式（Windows .lnk）。

.NOTES
    由 Go 侧以 `-Action create|remove -Target <挂载点> -Name <库名> -Dir <目录> [-Path <lnk>]` 调用。
    Dir 为空表示系统桌面（与客户端设置页的“留空使用桌面”一致）。
    remove 优先使用 -Path（挂载记录里记下的完整路径），没有时才按 Name + Dir 重新拼；
    只删本代理创建并记录过的那一个文件，不会碰用户自己放的快捷方式。
#>

param(
    [Parameter(Mandatory = $true)]
    [ValidateSet('create', 'remove')]
    [string]$Action,
    [string]$Target,
    [string]$Name,
    [string]$Dir,
    [string]$Path
)

$ErrorActionPreference = 'Stop'
$ProgressPreference = 'SilentlyContinue'
try { [Console]::OutputEncoding = [System.Text.Encoding]::UTF8 } catch { }

function Resolve-ShortcutDir([string]$Directory) {
    if (-not [string]::IsNullOrWhiteSpace($Directory)) {
        return $Directory.Trim()
    }
    return [Environment]::GetFolderPath('Desktop')
}

function Resolve-ShortcutName([string]$ShortcutName, [string]$TargetPath) {
    $raw = $ShortcutName
    if ([string]::IsNullOrWhiteSpace($raw) -and -not [string]::IsNullOrWhiteSpace($TargetPath)) {
        $raw = Split-Path -Leaf ($TargetPath.TrimEnd([char[]]@('\', '/')))
    }
    if ([string]::IsNullOrWhiteSpace($raw)) {
        $raw = 'Vault'
    }
    # 与 Electron 侧原实现保持一致：Windows 文件名非法字符替换成下划线，名称最长 120 字符。
    $safe = $raw -replace '[\\/:*?"<>|]', '_'
    if ($safe.Length -gt 120) {
        $safe = $safe.Substring(0, 120)
    }
    if ([string]::IsNullOrWhiteSpace($safe)) {
        $safe = 'Vault'
    }
    return $safe
}

try {
    $shortcutPath = $Path
    if ([string]::IsNullOrWhiteSpace($shortcutPath)) {
        $directory = Resolve-ShortcutDir $Dir
        $safeName = Resolve-ShortcutName $Name $Target
        $shortcutPath = Join-Path $directory ($safeName + '.lnk')
    }

    if ($Action -eq 'remove') {
        $removed = $false
        if (Test-Path -LiteralPath $shortcutPath) {
            Remove-Item -LiteralPath $shortcutPath -Force -ErrorAction Stop
            $removed = $true
        }
        [pscustomobject]@{
            ok      = $true
            action  = 'removed'
            removed = $removed
            path    = $shortcutPath
        } | ConvertTo-Json -Compress -Depth 5
        exit 0
    }

    if ([string]::IsNullOrWhiteSpace($Target)) {
        [pscustomobject]@{
            ok      = $false
            action  = $Action
            reason  = 'empty_target'
            message = '快捷方式目标不能为空'
        } | ConvertTo-Json -Compress -Depth 5
        exit 1
    }

    $directory = Split-Path -Parent $shortcutPath
    if (-not [string]::IsNullOrWhiteSpace($directory) -and -not (Test-Path -LiteralPath $directory)) {
        New-Item -ItemType Directory -Path $directory -Force | Out-Null
    }

    $shell = New-Object -ComObject WScript.Shell
    $link = $shell.CreateShortcut($shortcutPath)
    $link.TargetPath = $Target
    $working = Split-Path -Parent ($Target.TrimEnd([char[]]@('\', '/')))
    if (-not [string]::IsNullOrWhiteSpace($working)) {
        $link.WorkingDirectory = $working
    }
    $link.Description = 'Vault repository'
    $link.Save()

    [pscustomobject]@{
        ok     = $true
        action = 'created'
        path   = $shortcutPath
    } | ConvertTo-Json -Compress -Depth 5
    exit 0
} catch {
    [pscustomobject]@{
        ok      = $false
        action  = $Action
        reason  = 'shortcut_failed'
        message = $_.Exception.Message
    } | ConvertTo-Json -Compress -Depth 5
    exit 1
}
