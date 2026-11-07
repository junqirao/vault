import { useEffect, useMemo, useRef } from 'react'
import { Card, Typography } from 'antd'
import dayjs from 'dayjs'
import * as echarts from 'echarts/core'
import { LineChart } from 'echarts/charts'
import { GridComponent, TooltipComponent } from 'echarts/components'
import { CanvasRenderer } from 'echarts/renderers'
import type { EChartsOption } from 'echarts'

import type { AgentCacheSample } from '../../api/agentTypes'
import { useI18n } from '../../i18n'
import { fontSize, palette, spacing } from '../../tokens/palette'
import { formatBytes } from '../../utils/format'

// 只注册用到的图表/组件，走 echarts 的按需引入。
echarts.use([LineChart, GridComponent, TooltipComponent, CanvasRenderer])

/** 一条折线：name 用于 tooltip，data 与采样点对齐，null 表示该时刻无数据（断线）。 */
interface Series {
  name: string
  color: string
  data: (number | null)[]
}

/** ECharts 的薄封装：负责 init/resize/dispose，option 变化时整份替换。 */
function EChart({ option, height }: { option: EChartsOption; height: number }): JSX.Element {
  const hostRef = useRef<HTMLDivElement | null>(null)
  const chartRef = useRef<ReturnType<typeof echarts.init> | null>(null)

  useEffect(() => {
    if (!hostRef.current) return
    const chart = echarts.init(hostRef.current)
    chartRef.current = chart
    const onResize = (): void => chart.resize()
    window.addEventListener('resize', onResize)
    return () => {
      window.removeEventListener('resize', onResize)
      chart.dispose()
      chartRef.current = null
    }
  }, [])

  useEffect(() => {
    // notMerge=true：系列数量会变（例如 L2 未启用时少一条），必须整份替换而不是合并。
    chartRef.current?.setOption(option, true)
  }, [option])

  return <div ref={hostRef} style={{ width: '100%', height }} />
}

/**
 * 组装一张折线图的 option。
 *
 * percent 为真时 y 轴按 0..1 百分比显示；`unit` 是"这张图的数带单位"的格式化函数
 * （缓存占用这类字节数）：tooltip 与坐标轴都必须走它，否则图上只有标题栏那一处有人话单位，
 * 鼠标一碰就是裸字节数。
 */
function lineOption(
  times: string[],
  series: Series[],
  opts: { percent?: boolean; unit?: (v: number) => string } = {}
): EChartsOption {
  const valueText = opts.percent ? (v: number): string => `${(v * 100).toFixed(0)}%` : opts.unit
  const axisText = opts.percent ? (v: number): string => `${Math.round(v * 100)}%` : opts.unit
  return {
    animation: false,
    grid: { left: 56, right: 12, top: 12, bottom: 24 },
    tooltip: {
      trigger: 'axis',
      valueFormatter: (v) =>
        typeof v === 'number' ? (valueText ?? ((x: number) => x.toFixed(1)))(v) : '—'
    },
    xAxis: {
      type: 'category',
      data: times,
      boundaryGap: false,
      axisTick: { show: false },
      axisLine: { lineStyle: { color: palette.neutralFill } },
      axisLabel: { fontSize: 10, color: palette.textTertiary, hideOverlap: true }
    },
    yAxis: {
      type: 'value',
      min: 0,
      max: opts.percent ? 1 : undefined,
      splitLine: { lineStyle: { color: palette.neutralFill } },
      axisLabel: {
        fontSize: 10,
        color: palette.textTertiary,
        formatter: axisText ?? '{value}'
      }
    },
    series: series.map((s) => ({
      name: s.name,
      type: 'line',
      data: s.data,
      showSymbol: false,
      connectNulls: false,
      lineStyle: { width: 1.6, color: s.color },
      itemStyle: { color: s.color }
    }))
  }
}

/** 单张指标图：HTML 标题（含最新值）+ ECharts 绘图区。 */
function MetricChart({
  times,
  title,
  series,
  format,
  percent,
  unit
}: {
  times: string[]
  title: string
  series: Series[]
  /** 标题右侧"最新值"的渲染。 */
  format: (v: number) => string
  percent?: boolean
  /** 数值**带单位**（字节类）：坐标轴与 tooltip 也用 format 渲染。 */
  unit?: boolean
}): JSX.Element {
  const option = useMemo(
    () => lineOption(times, series, { percent, unit: unit ? format : undefined }),
    [times, series, percent, unit, format]
  )

  return (
    <div>
      <div style={{ display: 'flex', justifyContent: 'space-between', alignItems: 'baseline', gap: spacing.sm }}>
        <Typography.Text type="secondary" style={{ fontSize: fontSize.xs }}>
          {title}
        </Typography.Text>
        <span style={{ display: 'inline-flex', gap: spacing.sm }}>
          {series.map((s) => {
            const last = [...s.data].reverse().find((v) => v !== null && Number.isFinite(v))
            return (
              <Typography.Text key={s.name} type="secondary" style={{ fontSize: fontSize.xs }}>
                <span
                  aria-hidden
                  style={{
                    display: 'inline-block',
                    width: 8,
                    height: 8,
                    borderRadius: 4,
                    background: s.color,
                    marginRight: 4
                  }}
                />
                {s.name} {last !== undefined && last !== null ? format(last) : '—'}
              </Typography.Text>
            )
          })}
        </span>
      </div>
      <EChart option={option} height={132} />
    </div>
  )
}

/** 相邻采样点差分成速率（每秒）；目标重建导致计数归零时该区间为 null（断线而不是画假值）。 */
function rateOf(samples: AgentCacheSample[], pick: (s: AgentCacheSample) => number): (number | null)[] {
  return samples.map((s, i) => {
    if (i === 0) return null
    const prev = samples[i - 1]
    const dt = s.t - prev.t
    const dv = pick(s) - pick(prev)
    if (dt <= 0 || dv < 0) return null
    return dv / dt
  })
}

export interface RepoPerformanceViewProps {
  repoName: string
  /** 挂载点/状态说明（可选，显示在标题右侧）。 */
  detail?: string
  /** 本机挂载记录里的分配 ID；未挂载时为空。 */
  allocationId?: string
  /** 该库是否开启了缓存：开缓存展示命中率/占用，未开只展示读写 IOPS。 */
  cacheEnabled: boolean
  /** 直连模式不经过代理，没有可展示的指标。 */
  directMode?: boolean
  /** 当前库的采样点（由页面统一拉取后按分配 ID 分发，避免每个库各轮询一次）。 */
  samples: AgentCacheSample[]
}

/**
 * 单个存储库的性能视图（已挂载 / 未挂载共用）。
 *
 * 未挂载、直连模式、采样不足都以一句可执行说明替代空图：用户能直接看出"为什么没有数据"，
 * 而不是盯着一张没有曲线的坐标轴猜。
 */
export function RepoPerformanceView({
  repoName,
  detail,
  allocationId,
  cacheEnabled,
  directMode,
  samples
}: RepoPerformanceViewProps): JSX.Element {
  const { t } = useI18n()
  const fmtIops = (v: number): string => (v >= 100 ? v.toFixed(0) : v.toFixed(1))
  const fmtPct = (v: number): string => `${(v * 100).toFixed(0)}%`

  const times = useMemo(() => samples.map((s) => dayjs(s.t * 1000).format('HH:mm:ss')), [samples])

  const hitRate: (number | null)[] = samples.map((s, i) => {
    if (i === 0) return null
    const prev = samples[i - 1]
    const dr = s.reads - prev.reads
    const dh = s.hits - prev.hits
    if (dr <= 0 || dh < 0) return null
    return dh / dr
  })

  // 4K 随机读占比：Δ4K随机读 / Δ读命令。代理即使没开缓存也会统计，所以这条曲线
  // 不跟 cacheEnabled 走；旧代理没有 random_4k 字段时按 0 处理。
  const random4kRate: (number | null)[] = samples.map((s, i) => {
    if (i === 0) return null
    const prev = samples[i - 1]
    const dr = s.reads - prev.reads
    const d4k = (s.random_4k ?? 0) - (prev.random_4k ?? 0)
    if (dr <= 0 || d4k < 0) return null
    return d4k / dr
  })

  let body: JSX.Element
  if (!allocationId) {
    body = (
      <Typography.Text type="secondary" style={{ fontSize: fontSize.xs }}>
        {t('repo.monitor.unmountedHint')}
      </Typography.Text>
    )
  } else if (directMode) {
    body = (
      <Typography.Text type="secondary" style={{ fontSize: fontSize.xs }}>
        {t('repo.monitor.directHint')}
      </Typography.Text>
    )
  } else if (samples.length < 2) {
    body = (
      <Typography.Text type="secondary" style={{ fontSize: fontSize.xs }}>
        {t('repo.performance.detail.empty')}
      </Typography.Text>
    )
  } else {
    body = (
      <div
        style={{
          display: 'grid',
          gridTemplateColumns: 'repeat(auto-fit, minmax(220px, 1fr))',
          gap: spacing.md
        }}
      >
        {cacheEnabled ? (
          <MetricChart
            times={times}
            title={t('repo.cache.chart.hitRate')}
            series={[{ name: '', color: palette.success, data: hitRate }]}
            format={fmtPct}
            percent
          />
        ) : null}
        <MetricChart
          times={times}
          title={t('repo.performance.random4k')}
          series={[{ name: '', color: palette.danger, data: random4kRate }]}
          format={fmtPct}
          percent
        />
        <MetricChart
          times={times}
          title={t('repo.cache.chart.read')}
          series={
            cacheEnabled
              ? [
                  { name: t('repo.cache.chart.read'), color: palette.accent, data: rateOf(samples, (s) => s.reads) },
                  { name: t('repo.cache.chart.readHit'), color: palette.success, data: rateOf(samples, (s) => s.hits) }
                ]
              : [{ name: t('repo.cache.chart.read'), color: palette.accent, data: rateOf(samples, (s) => s.reads) }]
          }
          format={fmtIops}
        />
        <MetricChart
          times={times}
          title={t('repo.cache.chart.write')}
          series={[{ name: t('repo.cache.chart.write'), color: palette.warning, data: rateOf(samples, (s) => s.writes) }]}
          format={fmtIops}
        />
        {cacheEnabled ? (
          <MetricChart
            times={times}
            title={t('repo.cache.chart.usage')}
            series={[
              { name: 'L1', color: palette.accent, data: samples.map((s) => s.l1_used_bytes) },
              { name: 'L2', color: palette.warning, data: samples.map((s) => s.l2_used_bytes) }
            ]}
            format={formatBytes}
            unit
          />
        ) : null}
      </div>
    )
  }

  return (
    <Card
      size="small"
      title={repoName}
      extra={
        detail ? (
          <Typography.Text type="secondary" style={{ fontSize: fontSize.xs }}>
            {detail}
          </Typography.Text>
        ) : null
      }
    >
      {body}
    </Card>
  )
}
