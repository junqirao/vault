import { Tag } from 'antd'

import { useI18n } from '../i18n'

export type MediaType = 'hdd' | 'ssd' | 'unknown'

export interface MediaTypeTagProps {
  mediaType: MediaType
  /** unknown 时是否也显示（默认隐藏）。 */
  showUnknown?: boolean
}

/** 实心介质标签：SSD 绿色、HDD 淡黄色。全站同一套颜色。 */
export function MediaTypeTag({ mediaType, showUnknown = false }: MediaTypeTagProps): JSX.Element | null {
  const { t } = useI18n()
  if (mediaType === 'unknown' && !showUnknown) return null
  const solid =
    mediaType === 'ssd'
      ? { background: '#16A34A', borderColor: '#16A34A', color: '#fff' }
      : { background: '#FDE68A', borderColor: '#FDE68A', color: '#6B4E00' }
  return (
    <Tag style={{ marginInlineEnd: 0, border: 'none', ...solid }}>
      {t(`storage.media.${mediaType}`)}
    </Tag>
  )
}
