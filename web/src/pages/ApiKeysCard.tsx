import { useEffect, useState } from 'react'
import {
  ActionIcon, Alert, Badge, Button, Card, Code, CopyButton, Group, Modal, Select,
  Stack, Table, Text, TextInput, Tooltip,
} from '@mantine/core'
import { notifications } from '@mantine/notifications'
import { IconCheck, IconCopy, IconPlus, IconTrash } from '@tabler/icons-react'
import { api, type ApiKey, type ApiKeyScope } from '../api'
import { Field, useNarrow } from '../responsive'

/**
 * The keys other programs call this daemon with.
 *
 * On the settings page rather than beside the GitHub credentials, because they
 * are the other half of what is on this page: credentials prove who the fleet is
 * to GitHub, and these — like the password above them — are how something proves
 * itself to the fleet.
 *
 * A key is shown once, when it is made, and never again: the daemon keeps a hash
 * of it. That is the one thing this component has to get right, and it is why
 * creating a key opens a dialog that will not be mistaken for a form that saved.
 */
export function ApiKeysCard() {
  const [keys, setKeys] = useState<ApiKey[] | null>(null)
  const [creating, setCreating] = useState(false)
  const [minted, setMinted] = useState<{ key: ApiKey; secret: string } | null>(null)
  const narrow = useNarrow()

  const refresh = async () => {
    try {
      setKeys(await api.apiKeys())
    } catch {
      // An empty list rather than a card that never resolves. The password form
      // above works regardless, and that is what somebody is on this page for
      // when the daemon is struggling to answer at all.
      setKeys([])
    }
  }

  useEffect(() => {
    void refresh()
  }, [])

  if (!keys) return null

  return (
    <Card withBorder p={{ base: 'md', sm: 'lg' }}>
      <Stack gap="sm">
        <Group justify="space-between" gap="sm" wrap="wrap">
          <Text fw={500}>API keys</Text>
          <Button
            size="xs"
            variant="light"
            leftSection={<IconPlus size={14} />}
            onClick={() => setCreating(true)}
          >
            New key
          </Button>
        </Group>
        <Text size="sm" c="dimmed">
          How a script, a dashboard or a deployment calls this daemon without the password above.
          A key goes in the <Code>Authorization</Code> header as <Code>Bearer</Code>, and a{' '}
          <b>read</b> key can only ever read. No key can read or create another key, or change the
          password — those are yours alone, so revoking a key here is final.
        </Text>

        {keys.length === 0 ? (
          <Text size="sm" c="dimmed" fs="italic">
            No keys. Everything calling this daemon is doing so as you.
          </Text>
        ) : narrow ? (
          <Stack gap="sm">
            {keys.map((key) => (
              <Card key={key.id} withBorder padding="sm">
                <Stack gap="xs">
                  <Group justify="space-between" gap="xs" wrap="nowrap" align="flex-start">
                    <Text fw={500} style={{ wordBreak: 'break-word' }}>{key.name}</Text>
                    <RevokeButton apiKey={key} onRevoked={refresh} />
                  </Group>
                  <Field label="Scope"><ScopeBadge scope={key.scope} /></Field>
                  <Field label="Key"><Hint hint={key.hint} /></Field>
                  <Field label="Expires"><Text size="sm" c="dimmed">{when(key.expiresAt, 'never')}</Text></Field>
                  <Field label="Last used"><Text size="sm" c="dimmed">{when(key.lastUsedAt, 'never')}</Text></Field>
                </Stack>
              </Card>
            ))}
          </Stack>
        ) : (
          <Table verticalSpacing="sm" horizontalSpacing="md">
            <Table.Thead>
              <Table.Tr>
                <Table.Th>Name</Table.Th>
                <Table.Th>Scope</Table.Th>
                <Table.Th>Key</Table.Th>
                <Table.Th>Expires</Table.Th>
                <Table.Th>Last used</Table.Th>
                <Table.Th w={48} />
              </Table.Tr>
            </Table.Thead>
            <Table.Tbody>
              {keys.map((key) => (
                <Table.Tr key={key.id}>
                  <Table.Td><Text fw={500}>{key.name}</Text></Table.Td>
                  <Table.Td><ScopeBadge scope={key.scope} /></Table.Td>
                  <Table.Td><Hint hint={key.hint} /></Table.Td>
                  <Table.Td><Text size="sm" c="dimmed">{when(key.expiresAt, 'never')}</Text></Table.Td>
                  <Table.Td><Text size="sm" c="dimmed">{when(key.lastUsedAt, 'never')}</Text></Table.Td>
                  <Table.Td><RevokeButton apiKey={key} onRevoked={refresh} /></Table.Td>
                </Table.Tr>
              ))}
            </Table.Tbody>
          </Table>
        )}
      </Stack>

      <NewKeyModal
        opened={creating}
        onClose={() => setCreating(false)}
        onCreated={async (created) => {
          setCreating(false)
          setMinted(created)
          await refresh()
        }}
      />
      <MintedKeyModal minted={minted} onClose={() => setMinted(null)} />
    </Card>
  )
}

/**
 * The scope, said in a colour.
 *
 * Read is the unremarkable one and admin is the one worth noticing in a list, so
 * only admin is coloured — a table where every row is a badge tells the eye
 * nothing.
 */
function ScopeBadge({ scope }: { scope: ApiKeyScope }) {
  return scope === 'admin' ? (
    <Badge color="orange" variant="light">admin</Badge>
  ) : (
    <Badge color="gray" variant="light">read</Badge>
  )
}

/** The front of the key, which is all there is: enough to match one against what
 * a script is configured with. */
function Hint({ hint }: { hint: string }) {
  return <Text ff="monospace" size="sm" c="dimmed">rf_{hint}…</Text>
}

/** A date, or the word that says there is not one. A blank cell reads as a value
 * that failed to load. */
function when(at: string | undefined, absent: string): string {
  if (!at) return absent
  return new Date(at).toLocaleDateString()
}

function RevokeButton({ apiKey, onRevoked }: { apiKey: ApiKey; onRevoked: () => Promise<void> }) {
  const [busy, setBusy] = useState(false)
  return (
    <Tooltip label="Revoke">
      <ActionIcon
        variant="subtle"
        color="red"
        loading={busy}
        aria-label={`Revoke ${apiKey.name}`}
        onClick={async () => {
          setBusy(true)
          try {
            await api.deleteApiKey(apiKey.id)
            notifications.show({
              color: 'green',
              title: 'Key revoked',
              message: `Anything still using ${apiKey.name} stops working now.`,
            })
            await onRevoked()
          } catch (error) {
            notifications.show({
              color: 'red',
              title: 'Could not revoke the key',
              message: error instanceof Error ? error.message : String(error),
            })
          } finally {
            setBusy(false)
          }
        }}
      >
        <IconTrash size={16} />
      </ActionIcon>
    </Tooltip>
  )
}

/** How long a new key lasts. Offered as a few choices rather than a date picker:
 * the answer is almost always "a while" or "indefinitely", and a date box invites
 * a typo that creates a key expiring in the year 202. */
const lifetimes = [
  { value: '30', label: '30 days' },
  { value: '90', label: '90 days' },
  { value: '365', label: 'A year' },
  { value: '0', label: 'Never' },
]

function NewKeyModal({
  opened, onClose, onCreated,
}: {
  opened: boolean
  onClose: () => void
  onCreated: (created: { key: ApiKey; secret: string }) => Promise<void>
}) {
  const narrow = useNarrow()
  const [name, setName] = useState('')
  const [scope, setScope] = useState<ApiKeyScope>('read')
  const [days, setDays] = useState('90')
  const [saving, setSaving] = useState(false)

  // Back to the defaults, so the next key starts from read rather than from
  // whatever the last one happened to be. Reopening a form still set to admin is
  // how somebody creates an admin key without meaning to.
  const reset = () => {
    setName('')
    setScope('read')
    setDays('90')
  }

  const close = () => {
    reset()
    onClose()
  }

  return (
    <Modal opened={opened} onClose={close} title="New API key" fullScreen={narrow}>
      <Stack gap="sm">
        <TextInput
          label="Name"
          description="What it is for. It is how you will recognise it later."
          placeholder="monitoring"
          value={name}
          onChange={(event) => setName(event.currentTarget.value)}
        />
        <Select
          label="Scope"
          description="Read keys can only read. Give admin only to something that has to change the fleet."
          data={[
            { value: 'read', label: 'Read — see the fleet, change nothing' },
            { value: 'admin', label: 'Admin — create pools, rotate credentials, reconcile' },
          ]}
          value={scope}
          onChange={(value) => setScope((value as ApiKeyScope) ?? 'read')}
          allowDeselect={false}
        />
        <Select
          label="Expires"
          description="A key that outlives the job it was made for is the one nobody remembers to revoke."
          data={lifetimes}
          value={days}
          onChange={(value) => setDays(value ?? '90')}
          allowDeselect={false}
        />
        <Group justify="flex-end">
          <Button variant="default" onClick={close}>Cancel</Button>
          <Button
            loading={saving}
            disabled={!name.trim()}
            onClick={async () => {
              setSaving(true)
              try {
                const lifetime = Number(days)
                const created = await api.createApiKey({
                  name: name.trim(),
                  scope,
                  expiresAt: lifetime
                    ? new Date(Date.now() + lifetime * 86400_000).toISOString()
                    : undefined,
                })
                await onCreated({ key: created.apiKey, secret: created.secret })
                reset()
              } catch (error) {
                notifications.show({
                  color: 'red',
                  title: 'Could not create the key',
                  message: error instanceof Error ? error.message : String(error),
                })
              } finally {
                setSaving(false)
              }
            }}
          >
            Create key
          </Button>
        </Group>
      </Stack>
    </Modal>
  )
}

/**
 * The key, the one time anybody will see it.
 *
 * Everything else in this UI only takes secrets in; this is the only one that
 * gives one out, and it has one job: make it obvious that closing this dialog
 * destroys the key. Hence the warning above rather than below it, a copy button
 * rather than an invitation to select forty characters of base64 by hand, and a
 * close button that says what it does.
 */
function MintedKeyModal({
  minted, onClose,
}: {
  minted: { key: ApiKey; secret: string } | null
  onClose: () => void
}) {
  const narrow = useNarrow()
  return (
    <Modal
      opened={minted !== null}
      onClose={onClose}
      title={minted ? `API key for ${minted.key.name}` : 'API key'}
      size="lg"
      fullScreen={narrow}
    >
      {minted && (
        <Stack gap="sm">
          <Alert color="yellow" variant="light">
            Copy it now. The daemon keeps only a hash of this key, so it cannot be shown again —
            if you lose it, revoke this key and make another.
          </Alert>
          <Group gap="xs" wrap="nowrap" align="flex-start">
            {/* It scrolls rather than widening the dialog past the phone it is
                on, and it is a Code block because it is copied, not read. */}
            <Code block style={{ flex: 1, overflowX: 'auto', wordBreak: 'break-all' }}>
              {minted.secret}
            </Code>
            <CopyButton value={minted.secret}>
              {({ copied, copy }) => (
                <Tooltip label={copied ? 'Copied' : 'Copy'}>
                  <ActionIcon
                    variant="light"
                    color={copied ? 'green' : 'blue'}
                    onClick={copy}
                    aria-label="Copy the key"
                  >
                    {copied ? <IconCheck size={16} /> : <IconCopy size={16} />}
                  </ActionIcon>
                </Tooltip>
              )}
            </CopyButton>
          </Group>
          <Text size="sm" c="dimmed">
            Send it as <Code>Authorization: Bearer …</Code>. This is {' '}
            {minted.key.scope === 'admin'
              ? 'an admin key: it can change the fleet.'
              : 'a read key: it can see the fleet and change nothing.'}
          </Text>
          <Group justify="flex-end">
            <Button onClick={onClose}>I have copied it</Button>
          </Group>
        </Stack>
      )}
    </Modal>
  )
}
