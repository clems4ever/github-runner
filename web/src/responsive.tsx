/**
 * Telling a narrow screen from a wide one, and what to draw on it.
 *
 * The fleet and pool tables carry six and eight columns. There is no width on a
 * phone at which they are anything but clipped — and clipped silently, with the
 * row menu that edits a pool off the right-hand edge and no scrollbar to say
 * so. Below this breakpoint each row is drawn as a card instead, which is the
 * same facts in a column rather than fewer facts in a row.
 *
 * The cut is Mantine's `md` rather than `sm` because the navigation reappears
 * at `sm` and takes 220px back out of the content: a 768px tablet has less room
 * for a table than a 768px phone would.
 */
import type { ReactNode } from 'react'
import { Group, Text } from '@mantine/core'
import { useMediaQuery } from '@mantine/hooks'

/** Below this width a table becomes a list of cards. */
export const narrowQuery = '(max-width: 61.9375em)'

/**
 * Below this the header holds the wordmark, the logo and the two buttons, and
 * nothing else. The busy count is the one thing there that is repeated
 * elsewhere — the fleet page counts runners four ways — so it is what goes.
 * Left in, it does not shrink gracefully: it collapses to an empty blue pill.
 */
export const crampedHeaderQuery = '(max-width: 22.9375em)'

export function useNarrow(): boolean {
  // Resolved on the first render rather than in an effect: getting this wrong
  // for one frame means a phone paints the clipped table and then jumps.
  return useMediaQuery(narrowQuery, false, { getInitialValueInEffect: false })
}

export function useCrampedHeader(): boolean {
  return useMediaQuery(crampedHeaderQuery, false, { getInitialValueInEffect: false })
}

/**
 * One fact on a card: the column heading the table would have used, and the
 * cell that was under it.
 *
 * Label left, value right, so a column of them lines up down both edges and
 * can be read by scanning either side.
 */
export function Field({ label, children }: { label: string; children: ReactNode }) {
  return (
    <Group justify="space-between" gap="md" wrap="nowrap" align="flex-start">
      <Text size="xs" c="dimmed" tt="uppercase" fw={600} style={{ flexShrink: 0 }}>
        {label}
      </Text>
      <div style={{ minWidth: 0, textAlign: 'right' }}>{children}</div>
    </Group>
  )
}
