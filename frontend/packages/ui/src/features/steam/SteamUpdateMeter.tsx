import type { CSSProperties } from 'react'
import { Typography } from 'antd'

import { UsageBar } from '../../components/UsageBar'
import { fontSize, palette, spacing } from '../../tokens/palette'
import type { SteamUpdateProgress } from './steamUpdateProgress'

/**
 * Steam 更新进度条：与存储库列表的用量格同一套排版（左标签 + 右明细，下面一条进度条）。
 *
 * 游戏分组页与存储库列表复用这一个元素：同一个下载在哪儿看都长得一样，
 * 也保证"文字在上、进度条在下"的固定节奏，进度条不会因为下方多一行文字而上下错位。
 */
export function SteamUpdateMeter({
  progress,
  style
}: {
  progress: SteamUpdateProgress
  style?: CSSProperties
}): JSX.Element {
  return (
    <div style={{ ...style, minWidth: 0 }}>
      <div style={{ display: 'flex', justifyContent: 'space-between', alignItems: 'baseline', gap: spacing.xs }}>
        <Typography.Text
          type="secondary"
          style={{ fontSize: fontSize.sm, whiteSpace: 'nowrap', flex: '0 0 auto' }}
        >
          {progress.label}
        </Typography.Text>
        <Typography.Text
          style={{
            fontSize: fontSize.sm,
            whiteSpace: 'nowrap',
            overflow: 'hidden',
            textOverflow: 'ellipsis',
            textAlign: 'right',
            flex: '1 1 auto',
            minWidth: 0
          }}
        >
          {progress.value}
        </Typography.Text>
      </div>
      {/* 下载进度不是用量，不随占比变橙/红：一路主色蓝（与上传进度同一口径）。 */}
      <UsageBar percent={progress.percent} color={palette.accent} />
    </div>
  )
}
