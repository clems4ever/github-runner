import { useEffect, useState } from 'react'
import {
  Card,
  Center,
  Group,
  Loader,
  SegmentedControl,
  Select,
  Stack,
  Text,
  useComputedColorScheme,
} from '@mantine/core'
import { CompositeChart } from '@mantine/charts'
import { api, type ActivityPoint, type Pool } from '../api'

/**
 * What the fleet has been doing.
 *
 * Two encodings on one axis, both counting runners, because the interesting
 * thing is the relationship between them: the filled area is work actually
 * running, the line above it is how many runners existed to run it. When a
 * pool scales, the line steps up to meet the area and steps back down when the
 * work stops — which is the autoscaler's behaviour, drawn.
 *
 * Not two y-scales: same unit, same axis. A second scale would let the two
 * lines be positioned to imply any relationship at all.
 */

/** Series colours, from the validated categorical palette (slots 1 and 2). */
const colours = {
  light: { busy: '#2a78d6', runners: '#eb6834', grid: '#e1e0d9', muted: '#898781' },
  dark: { busy: '#3987e5', runners: '#d95926', grid: '#2c2c2a', muted: '#898781' },
}

const ranges = [
  { value: '1', label: '1h' },
  { value: '6', label: '6h' },
  { value: '24', label: '24h' },
]

export function ActivityChart({ pools }: { pools: Pool[] }) {
  const scheme = useComputedColorScheme('light')
  const palette = colours[scheme]

  const [hours, setHours] = useState('6')
  // Empty is the whole fleet. Narrowing to one pool is how someone looks at a
  // single repository without a chart per pool crowding the page.
  const [pool, setPool] = useState<string | null>(null)
  const [points, setPoints] = useState<ActivityPoint[] | null>(null)
  const [failed, setFailed] = useState(false)

  useEffect(() => {
    let cancelled = false
    const load = async () => {
      try {
        const activity = await api.activity(Number(hours), pool ?? undefined)
        if (!cancelled) {
          setPoints(activity.points ?? [])
          setFailed(false)
        }
      } catch {
        if (!cancelled) setFailed(true)
      }
    }
    void load()
    // The window keeps moving, so the chart has to as well — but far less
    // often than the runner table, which is about right now.
    const timer = setInterval(() => void load(), 30_000)
    return () => {
      cancelled = true
      clearInterval(timer)
    }
  }, [hours, pool])

  const data = (points ?? []).map((point) => ({
    at: point.at,
    // Labels here are what the legend and the tooltip show, so they are
    // written for a reader rather than named after fields.
    'Running a job': point.busy,
    Runners: point.running,
  }))

  const peak = Math.max(1, ...(points ?? []).map((p) => p.running))

  return (
    <Card withBorder padding="md">
      <Group justify="space-between" mb="sm">
        <div>
          <Text fw={500}>Activity</Text>
          <Text size="xs" c="dimmed">
            Peak per interval, so a short burst is not averaged away
          </Text>
        </div>
        {/* Filters in one row, above the chart. */}
        <Group gap="xs">
          {pools.length > 1 && (
            <Select
              size="xs"
              w={180}
              aria-label="Pool"
              placeholder="All pools"
              clearable
              value={pool}
              onChange={setPool}
              data={pools.map((p) => ({ value: p.name, label: p.name }))}
            />
          )}
          <SegmentedControl size="xs" data={ranges} value={hours} onChange={setHours} />
        </Group>
      </Group>

      {failed ? (
        <Center h={220}>
          <Stack align="center" gap={4}>
            <Text size="sm" fw={500}>
              Could not read the history.
            </Text>
            <Text size="xs" c="dimmed">
              The fleet itself is unaffected — this is only what it has been doing.
            </Text>
          </Stack>
        </Center>
      ) : points === null ? (
        <Center h={220}>
          <Loader size="sm" />
        </Center>
      ) : data.length === 0 ? (
        <Center h={220}>
          <Stack align="center" gap={4}>
            <Text size="sm" fw={500}>
              No history yet
            </Text>
            <Text size="xs" c="dimmed">
              {pool
                ? `Nothing recorded for ${pool} in this window.`
                : 'The daemon records what the fleet is doing on every pass; come back in a minute.'}
            </Text>
          </Stack>
        </Center>
      ) : (
        <CompositeChart
          h={220}
          data={data}
          dataKey="at"
          withLegend
          legendProps={{ verticalAlign: 'bottom', height: 32 }}
          curveType="stepAfter"
          // No marker on every point: at a couple of hundred points per window
          // they stop being marks and become texture. The crosshair tooltip is
          // what reads a single value.
          withDots={false}
          strokeWidth={2}
          areaProps={{ fillOpacity: 0.18 }}
          // One axis: both series count runners. Whole numbers only — half a
          // runner is not a thing.
          yAxisProps={{ domain: [0, peak], allowDecimals: false, width: 32 }}
          xAxisProps={{
            tickFormatter: (value: string) =>
              new Date(value).toLocaleTimeString([], { hour: '2-digit', minute: '2-digit' }),
            minTickGap: 48,
          }}
          // The colours go in as CSS variables rather than through the
          // `gridColor` and `textColor` props. Those are still in the type,
          // but @mantine/charts 9.5.1 stopped taking them out of the props it
          // forwards, so they reach the root div and React reports two unknown
          // attributes per render. These are the same two variables the props
          // would have set.
          vars={() => ({
            root: { '--chart-grid-color': palette.grid, '--chart-text-color': palette.muted },
          })}
          tooltipAnimationDuration={120}
          tooltipProps={{
            // Without this the crosshair label is the raw timestamp, which is
            // the one place a reader is looking for a time they can read.
            labelFormatter: (value: unknown) =>
              new Date(String(value)).toLocaleString([], {
                hour: '2-digit',
                minute: '2-digit',
                day: 'numeric',
                month: 'short',
              }),
          }}
          series={[
            // The fill is the work; the line above it is the capacity that was
            // there to do it. Different mark shapes as well as different
            // colours, so the two are told apart without relying on hue.
            { name: 'Running a job', color: palette.busy, type: 'area' },
            { name: 'Runners', color: palette.runners, type: 'line' },
          ]}
        />
      )}
    </Card>
  )
}
