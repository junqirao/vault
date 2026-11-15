<#
.SYNOPSIS
    Extend the largest data partition inside a VHDX to the disk's maximum size.

.DESCRIPTION
    Resize-VHD only changes the virtual size of the VHDX; it does not grow the
    NTFS partition inside the guest. After an automatic parent-disk expansion,
    the guest volume would otherwise stay at the old size and the next diff copy
    fails with "There is not enough space on the disk."

    This script attaches the VHDX, finds the largest Basic partition, extends it
    to Get-PartitionSupportedSize().SizeMax, then detaches it. It is idempotent.
#>

param(
    [string]$Path
)

$ErrorActionPreference = 'Stop'
$ProgressPreference = 'SilentlyContinue'
try { [Console]::OutputEncoding = [System.Text.Encoding]::UTF8 } catch { }

$mounted = $false
try {
    if (-not $Path) { throw 'Path is required' }
    if (-not (Test-Path -LiteralPath $Path)) { throw "VHDX not found: $Path" }
    if (-not (Get-Command -Name Mount-VHD -ErrorAction SilentlyContinue)) {
        throw 'Hyper-V module (Mount-VHD) is unavailable'
    }

    $vhd = Mount-VHD -Path $Path -Passthru -ErrorAction Stop
    $mounted = $true

    # Give the storage stack a moment to expose the disk/partition.
    Start-Sleep -Milliseconds 500

    $diskNumber = $vhd.DiskNumber
    if (-not $diskNumber) {
        $disk = Get-Disk | Where-Object { $_.Location -and $_.Location -like "*$Path*" } | Select-Object -First 1
    } else {
        $disk = Get-Disk -Number $diskNumber -ErrorAction Stop
    }
    if (-not $disk) { throw 'Mounted VHDX disk not found' }

    $partition = Get-Partition -DiskNumber $disk.Number -ErrorAction SilentlyContinue |
        Where-Object { $_.Type -eq 'Basic' } |
        Sort-Object Size -Descending |
        Select-Object -First 1
    if (-not $partition) { throw 'No Basic partition found in VHDX' }

    $supported = Get-PartitionSupportedSize -DiskNumber $disk.Number -PartitionNumber $partition.PartitionNumber -ErrorAction Stop
    $extended = $false
    if ($supported.SizeMax -gt ($partition.Size + 1MB)) {
        Resize-Partition -DiskNumber $disk.Number -PartitionNumber $partition.PartitionNumber -Size $supported.SizeMax -ErrorAction Stop
        $extended = $true
        # Resize-Partition normally grows NTFS too; Resize-Volume is a safe fallback
        # for builds where the filesystem stays at the old size.
        try {
            $updated = Get-Partition -DiskNumber $disk.Number -PartitionNumber $partition.PartitionNumber -ErrorAction Stop
            $volume = Get-Volume -Partition $updated -ErrorAction SilentlyContinue
            if ($volume -and $volume.Size -lt $supported.SizeMax) {
                Resize-Volume -InputObject $volume -Size $supported.SizeMax -ErrorAction SilentlyContinue
            }
        } catch { }
    }

    [pscustomobject]@{
        ok         = $true
        extended   = $extended
        size_bytes = [int64]$supported.SizeMax
    } | ConvertTo-Json -Compress -Depth 5
} catch {
    [pscustomobject]@{
        ok      = $false
        reason  = 'extend_partition_failed'
        message = $_.Exception.Message
    } | ConvertTo-Json -Compress -Depth 5
    exit 1
} finally {
    if ($mounted) {
        try { Dismount-VHD -Path $Path -ErrorAction SilentlyContinue } catch { }
    }
}
