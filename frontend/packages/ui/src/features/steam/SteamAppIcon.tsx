import { AppstoreOutlined } from '@ant-design/icons'
import { useEffect, useState } from 'react'

import { fetchLocalSteamIcon, fetchLocalSteamImage } from '../../api/agentClient'
import { palette } from '../../tokens/palette'

export interface SteamAppIconProps {
  appID: number
  name?: string
  /** 服务端已知的封面 URL（可选）；本机图标/封面都读不到时作为最后回退。 */
  image?: string
  size?: number
  radius?: number
}

/**
 * Steam 应用图标。
 *
 * 列表行优先使用 Steam 本地缓存里的 32×32 应用图标（librarycache 中 40 位哈希 jpg），
 * 避免把宽高比完全不同的封面塞进小方块；本机没有图标时退回封面，再退回通用占位。
 */
export function SteamAppIcon({
  appID,
  name,
  image,
  size = 40,
  radius = 8
}: SteamAppIconProps): JSX.Element {
  const [src, setSrc] = useState<string | null>(image || null)

  useEffect(() => {
    let canceled = false
    setSrc(image || null)
    void (async () => {
      const icon = await fetchLocalSteamIcon(appID)
      if (canceled) return
      if (icon) {
        setSrc(icon)
        return
      }
      const cover = await fetchLocalSteamImage(appID)
      if (!canceled && cover) setSrc(cover)
    })()
    return () => {
      canceled = true
    }
  }, [appID, image])

  return (
    <div
      style={{
        width: size,
        height: size,
        flexShrink: 0,
        borderRadius: radius,
        overflow: 'hidden',
        background: palette.neutralFill,
        display: 'flex',
        alignItems: 'center',
        justifyContent: 'center'
      }}
    >
      {src ? (
        <img
          src={src}
          alt={name || `App ${appID}`}
          loading="lazy"
          decoding="async"
          onError={() => setSrc(null)}
          style={{ width: '100%', height: '100%', objectFit: 'cover', display: 'block' }}
        />
      ) : (
        <AppstoreOutlined style={{ fontSize: Math.round(size * 0.45), color: palette.textTertiary }} />
      )}
    </div>
  )
}
