import { useEffect, useState } from 'react'
import {
  Button, Card, Code, Group, NumberInput, PasswordInput, Stack, Switch, Text, TextInput, Title,
} from '@mantine/core'
import { notifications } from '@mantine/notifications'
import { api, type Budget, type Health } from '../api'

export function SettingsPage({ health }: { health: Health | null }) {
  const [user, setUser] = useState('')
  const [password, setPassword] = useState('')
  const [saving, setSaving] = useState(false)

  return (
    <Stack gap="lg" maw={640}>
      <Title order={3}>Settings</Title>

      <Card withBorder p={{ base: 'md', sm: 'lg' }}>
        <Stack gap="sm">
          <Text fw={500}>Web access</Text>
          <Text size="sm" c="dimmed">
            The daemon listens on the loopback address and asks for these credentials. It can create
            machines and holds a token that administers repositories, so nothing else on this host
            should be able to reach it.
          </Text>
          <TextInput
            label="User" placeholder="admin"
            value={user} onChange={(event) => setUser(event.currentTarget.value)}
          />
          <PasswordInput
            label="New password" description="At least 8 characters"
            value={password} onChange={(event) => setPassword(event.currentTarget.value)}
          />
          <Group justify="flex-end">
            <Button
              loading={saving}
              disabled={!user || password.length < 8}
              onClick={async () => {
                setSaving(true)
                try {
                  await api.setPassword(user, password)
                  notifications.show({
                    color: 'green',
                    title: 'Password changed',
                    message: 'The browser will ask for the new one on the next request.',
                  })
                  setPassword('')
                } catch (error) {
                  notifications.show({
                    color: 'red',
                    title: 'Could not change the password',
                    message: error instanceof Error ? error.message : String(error),
                  })
                } finally {
                  setSaving(false)
                }
              }}
            >
              Change password
            </Button>
          </Group>
        </Stack>
      </Card>

      <FleetBudget />

      <Card withBorder p={{ base: 'md', sm: 'lg' }}>
        <Stack gap="xs">
          <Text fw={500}>Upgrading</Text>
          <Text size="sm" c="dimmed">
            Replacing the binary and restarting the daemon does not touch the runners: they are
            systemd units and containers of their own, and jobs carry on through it.
          </Text>
          {/* A command is copied, not read across: it scrolls rather than
              widening the page past the phone it is on. */}
          <Code block style={{ overflowX: 'auto' }}>
            sudo systemctl restart runner-fleetd
          </Code>
          <Text size="sm" c="dimmed">Version {health?.version ?? 'unknown'}</Text>
        </Stack>
      </Card>
    </Stack>
  )
}

/**
 * What the whole fleet may take from this host, as opposed to what any one pool
 * was promised.
 *
 * Zero in a field is that dimension uncapped, and the fields say so rather than
 * being left empty: "0" next to "no cap" is unambiguous where an empty box is a
 * question about whether the form loaded.
 */
function FleetBudget() {
  const [budget, setBudget] = useState<Budget | null>(null)
  const [saving, setSaving] = useState(false)

  useEffect(() => {
    let cancelled = false
    api
      .settings()
      .then((settings) => {
        if (!cancelled) setBudget(settings.budget)
      })
      .catch(() => {
        // The page has a password form on it that works regardless, and the
        // budget is not what somebody came here for when the daemon is
        // struggling to answer at all.
        if (!cancelled) setBudget({ cpus: 0, cpuWeight: 0, memoryMb: 0, hardMemory: false })
      })
    return () => {
      cancelled = true
    }
  }, [])

  if (!budget) return null

  const set = (patch: Partial<Budget>) => setBudget({ ...budget, ...patch })

  return (
    <Card withBorder p={{ base: 'md', sm: 'lg' }}>
      <Stack gap="sm">
        <Text fw={500}>Fleet budget</Text>
        <Text size="sm" c="dimmed">
          A pool's size is per runner and says nothing about what happens when every pool is busy
          at once. This is the ceiling over all of them together: the machines run in one control
          group, and these are its limits. Pools stop growing when the budget is spent, and never
          shrink below their own minimums. Container pools are not covered.
        </Text>
        <Group grow align="flex-start">
          <NumberInput
            label="CPU" description="Cores across every machine. 0 for no cap."
            min={0} max={1024} allowDecimal={false}
            value={budget.cpus} onChange={(value) => set({ cpus: Number(value) || 0 })}
          />
          <NumberInput
            label="Memory (MiB)" description="Across every machine. 0 for no cap."
            min={0} step={1024} allowDecimal={false}
            value={budget.memoryMb} onChange={(value) => set({ memoryMb: Number(value) || 0 })}
          />
        </Group>
        <NumberInput
          label="Share when the host is contended"
          description={
            "What the fleet gets when something else on this host wants the machine too." +
            " systemd's default is 100, so below that is a fleet that yields. 0 leaves the default."
          }
          min={0} max={10000} allowDecimal={false}
          value={budget.cpuWeight} onChange={(value) => set({ cpuWeight: Number(value) || 0 })}
        />
        <Switch
          checked={budget.hardMemory}
          disabled={!budget.memoryMb}
          onChange={(event) => set({ hardMemory: event.currentTarget.checked })}
          label="Kill a machine rather than let the fleet exceed its memory"
          description={
            'Off by default. Without it the kernel reclaims harder as the fleet approaches the' +
            ' ceiling and the fleet gets slower. With it, the kernel kills the largest machine in' +
            ' the group — which is not necessarily the one that overspent, so it costs somebody' +
            ' their job. Turn it on if you would rather lose a job than have the host crawl.'
          }
        />
        <Group justify="flex-end">
          <Button
            loading={saving}
            onClick={async () => {
              setSaving(true)
              try {
                const saved = await api.setBudget(budget)
                setBudget(saved)
                notifications.show({
                  color: 'green',
                  title: 'Budget saved',
                  message:
                    saved.cpus || saved.memoryMb
                      ? 'It applies to the machines that are already running.'
                      : 'The fleet is no longer capped.',
                })
              } catch (error) {
                notifications.show({
                  color: 'red',
                  title: 'Could not save the budget',
                  message: error instanceof Error ? error.message : String(error),
                })
              } finally {
                setSaving(false)
              }
            }}
          >
            Save budget
          </Button>
        </Group>
      </Stack>
    </Card>
  )
}
