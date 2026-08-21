import { useState } from 'react'
import {
  Button, Card, Code, Group, PasswordInput, Stack, Text, TextInput, Title,
} from '@mantine/core'
import { notifications } from '@mantine/notifications'
import { api, type Health } from '../api'

export function SettingsPage({ health }: { health: Health | null }) {
  const [user, setUser] = useState('')
  const [password, setPassword] = useState('')
  const [saving, setSaving] = useState(false)

  return (
    <Stack gap="lg" maw={640}>
      <Title order={3}>Settings</Title>

      <Card withBorder padding="lg">
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

      <Card withBorder padding="lg">
        <Stack gap="xs">
          <Text fw={500}>Upgrading</Text>
          <Text size="sm" c="dimmed">
            Replacing the binary and restarting the daemon does not touch the runners: they are
            systemd units and containers of their own, and jobs carry on through it.
          </Text>
          <Code block>sudo systemctl restart runner-fleetd</Code>
          <Text size="sm" c="dimmed">Version {health?.version ?? 'unknown'}</Text>
        </Stack>
      </Card>
    </Stack>
  )
}
