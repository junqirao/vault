import type { ReactNode } from 'react'
import { Button } from 'antd'
import { RightOutlined } from '@ant-design/icons'

import { fontSize } from '../tokens/palette'

export interface SectionTitleLinkProps {
  children: ReactNode
  onClick?: () => void
  /** 悬浮提示 / 无障碍名称（通常是"进入详情/管理"）。 */
  title?: string
  disabled?: boolean
}

/**
 * 一级模块标题的可点击入口：小号加粗文字 + 尾置右箭头。
 *
 * 所有首页模块（存储 / 配额 / 缓存 / 性能）共用这一颗组件，保证：
 *   - 字号比卡片标题小（13px），不抢主标题；
 *   - 文字与 `>` 图标用 inline-flex 垂直居中（手动拼 span/箭头容易基线偏移）；
 *   - paddingInline 为 0，标题左边缘与相邻模块对齐。
 */
export function SectionTitleLink({ children, onClick, title, disabled }: SectionTitleLinkProps): JSX.Element {
  return (
    <Button
      type="text"
      size="small"
      aria-label={title}
      disabled={disabled}
      icon={<RightOutlined style={{ fontSize: 9, lineHeight: 1, display: 'inline-flex' }} />}
      iconPosition="end"
      onClick={onClick}
      style={{
        paddingInline: 0,
        height: 'auto',
        fontWeight: 600,
        fontSize: fontSize.sm,
        lineHeight: 1.2,
        display: 'inline-flex',
        alignItems: 'center'
      }}
    >
      {children}
    </Button>
  )
}
