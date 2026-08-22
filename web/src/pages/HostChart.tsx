import { useEffect, useState } from 'react'
import {
  Card,
  Center,
  Group,
  Loader,
  SegmentedControl,
  Stack,
  Text,
  useComputedColorScheme,
} from '@mantine/core'
import { CompositeChart } from '@mantine/charts'
import { api, type HostPoint } from '../api'

/**
 * What the host has been using.
 *
 * Three quantities measured in three different units — processor time, memory,
 * disk — drawn as percentages of themselves so that one axis can carry all of
 * them. That is the only honest way to put them on one chart: a second y-scale
 * would let any two of these lines be positioned to imply any relationship at
 * all, and the relationship is exactly what a reader is here for.
 *
 * The peak per interval, not the mean, for the same reason the activity chart
 * uses it: an hour-long bucket averages a two-minute build storm into nothing,
 * and the storm is the event worth seeing.
 */

/**
 * Series colours, from the validated categorical palette (slots 1, 2 and 3).
 *
 * The same three appear on the meters above the chart, so a colour means one
 * resource on this page rather than one series in this component. Aqua sits
 * below 3:1 against the light surface, which the palette's relief rule allows
 * only where the value is also written down — the meters do exactly that.
 */
const colours = {
  light: { cpu: '#2a78d6', memory: '#eb6834', disk: '#1baf7a', grid: '#e1e0d9', muted: '#898781' },
  dark: { cpu: '#3987e5', memory: '#d95926', disk: '#199e70', grid: '#2c2c2a', muted: '#898781' },
}

const ranges = [
  { value: '1', label: '1h' },
  { value: '6', label: '6h' },
  { value: '24', label: '24h' },
]

export function HostChart() {
  const scheme = useComputedColorScheme('light')
  const palette = colours[scheme]

  const [hours, setHours] = useState('6')
  const [points, setPoints] = useState<HostPoint[] | null>(null)
  const [failed, setFailed] = useState(false)

  useEffect(() => {
    let cancelled = false
    const load = async () => {
      try {
        const history = await api.resourceHistory(Number(hours))
        if (!cancelled) {
          setPoints(history.points ?? [])
          setFailed(false)
        }
      } catch {
        if (!cancelled) setFailed(true)
      }
    }
    void load()
    // The window keeps moving, so the chart has to as well — but far less
    // often than the meters, which are about right now.
    const timer = setInterval(() => void load(), 30_000)
    return () => {
      cancelled = true
      clearInterval(timer)
    }
  }, [hours])

  const data = (points ?? []).map((point) => ({
    at: point.at,
    // The labels here are what the legend and the tooltip show, so they are
    // written for a reader rather than named after fields.
    CPU: point.cpuPercent,
    Memory: point.memoryPercent,
    Disk: point.diskPercent,
  }))

  return (
    <Card withBorder padding="md">
      <Group justify="space-between" mb="sm">
        <div>
          <Text fw={500}>History</Text>
          <Text size="xs" c="dimmed">
            Peak per interval, as a share of what the host has
          </Text>
        </div>
        <SegmentedControl size="xs" data={ranges} value={hours} onChange={setHours} />
      </Group>

      {failed ? (
        <Center h={220}>
          <Stack align="center" gap={4}>
            <Text size="sm" fw={500}>
              Could not read the history.
            </Text>
            <Text size="xs" c="dimmed">
              The fleet itself is unaffected — this is only what the host has been doing.
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
              The daemon measures the host every few seconds; come back in a minute.
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
          curveType="monotone"
          // No marker on every point: at a couple of hundred points per window
          // they stop being marks and become texture. The crosshair tooltip is
          // what reads a single value.
          withDots={false}
          strokeWidth={2}
          areaProps={{ fillOpacity: 0.18 }}
          // Fixed to the full scale rather than to the peak. These are shares
          // of a fixed budget, and a host at 4% should look nearly empty
          // instead of being stretched to fill the card.
          yAxisProps={{ domain: [0, 100], width: 40, tickFormatter: (v: number) => `${v}%` }}
          xAxisProps={{
            tickFormatter: (value: string) =>
              new Date(value).toLocaleTimeString([], { hour: '2-digit', minute: '2-digit' }),
            minTickGap: 48,
          }}
          valueFormatter={(value: number) => `${value.toFixed(1)}%`}
          gridColor={palette.grid}
          textColor={palette.muted}
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
            // Processor time is the one that moves, so it gets the fill and the
            // two slower-moving lines are drawn over it. Different mark shapes
            // as well as different colours, so they are told apart without
            // relying on hue.
            { name: 'CPU', color: palette.cpu, type: 'area' },
            { name: 'Memory', color: palette.memory, type: 'line' },
            { name: 'Disk', color: palette.disk, type: 'line' },
          ]}
        />
      )}
    </Card>
  )
}
