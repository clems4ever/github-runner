import { useState } from 'react'
import {
  Alert,
  Anchor,
  Badge,
  Button,
  Checkbox,
  FileButton,
  Group,
  SegmentedControl,
  Select,
  Stack,
  Table,
  Text,
  Textarea,
  TextInput,
} from '@mantine/core'
import { notifications } from '@mantine/notifications'
import { IconAlertTriangle, IconExternalLink, IconFileImport } from '@tabler/icons-react'
import {
  api,
  ApiError,
  type Credential,
  type ImportReport,
  type ScopeKind,
} from '../api'

/**
 * Importing a template.
 *
 * The preview is not a guess. It is the import itself, run against the database
 * and rolled back, so a line saying "create" is a pool that was created a
 * moment ago and then undone. That is why it is worth reading before pressing
 * the button underneath it.
 */
export function ImportPools({
  credentials,
  onImported,
  onCancel,
}: {
  credentials: Credential[]
  onImported: () => Promise<void>
  onCancel: () => void
}) {
  const [text, setText] = useState('')
  const [credentialId, setCredentialId] = useState<number>(credentials[0]?.id ?? 0)
  const [scope, setScope] = useState('')
  const [scopeKind, setScopeKind] = useState<ScopeKind>('repository')
  const [replaceExisting, setReplaceExisting] = useState(false)
  const [report, setReport] = useState<ImportReport | null>(null)
  const [problem, setProblem] = useState<{ message: string; grantUrl?: string } | null>(null)
  const [busy, setBusy] = useState(false)

  // Anything typed invalidates a preview taken before it, so the button under
  // the table always belongs to the table above it.
  const change = (value: string) => {
    setText(value)
    setReport(null)
    setProblem(null)
  }

  const send = async (dryRun: boolean) => {
    let parsed: unknown
    try {
      parsed = JSON.parse(text)
    } catch (error) {
      // The only check made here. Everything about what the document *means*
      // is the daemon's answer, so there is one set of rules rather than two.
      setProblem({ message: `This is not valid JSON: ${(error as Error).message}` })
      return
    }

    setBusy(true)
    setProblem(null)
    try {
      const result = await api.importPools({
        document: parsed,
        credentialId,
        scope: scope.trim() || undefined,
        scopeKind: scope.trim() ? scopeKind : undefined,
        replaceExisting,
        dryRun,
      })
      if (dryRun) {
        setReport(result)
        return
      }
      notifications.show({
        color: 'green',
        title: `Imported ${result.pools.length} ${result.pools.length === 1 ? 'pool' : 'pools'}`,
        message: 'Their runners are started on the next pass, which is now.',
      })
      await onImported()
    } catch (error) {
      if (error instanceof ApiError) {
        setProblem({ message: error.message, grantUrl: error.grantUrl })
      } else {
        setProblem({ message: error instanceof Error ? error.message : String(error) })
      }
      setReport(null)
    } finally {
      setBusy(false)
    }
  }

  const ready = text.trim() !== '' && credentialId > 0

  return (
    <Stack>
      <Group grow align="flex-start">
        <Select
          label="Credential"
          description="Imported pools register with it"
          withAsterisk
          data={credentials.map((c) => ({ value: String(c.id), label: `${c.name} (${c.hint})` }))}
          value={credentialId ? String(credentialId) : null}
          onChange={(value) => {
            setCredentialId(Number(value))
            setReport(null)
          }}
        />
        <TextInput
          label="Scope"
          description="Optional — replaces the scope of every pool in the template"
          placeholder="owner/repository"
          value={scope}
          onChange={(event) => {
            setScope(event.currentTarget.value)
            setReport(null)
          }}
        />
      </Group>

      {scope.trim() !== '' && (
        <SegmentedControl
          size="xs"
          value={scopeKind}
          onChange={(value) => setScopeKind(value as ScopeKind)}
          data={[
            { value: 'repository', label: 'Repository' },
            { value: 'organization', label: 'Organisation' },
          ]}
        />
      )}

      <Textarea
        label="Template"
        description="Paste a template, or choose the file"
        withAsterisk
        rows={10}
        autosize={false}
        placeholder={'{\n  "version": 1,\n  "pools": [ … ]\n}'}
        styles={{ input: { fontFamily: 'var(--mantine-font-family-monospace)', fontSize: 12 } }}
        value={text}
        onChange={(event) => change(event.currentTarget.value)}
      />

      <Group justify="space-between">
        <FileButton
          accept="application/json"
          onChange={async (file) => {
            if (file) change(await file.text())
          }}
        >
          {(props) => (
            <Button {...props} variant="default" size="xs" leftSection={<IconFileImport size={14} />}>
              Choose a file
            </Button>
          )}
        </FileButton>
        <Checkbox
          label="Import over pools that already exist"
          checked={replaceExisting}
          onChange={(event) => {
            setReplaceExisting(event.currentTarget.checked)
            setReport(null)
          }}
        />
      </Group>

      {problem && (
        <Alert color="red" variant="light" icon={<IconAlertTriangle size={18} />}>
          <Text size="sm">{problem.message}</Text>
          {problem.grantUrl && (
            <Anchor size="sm" href={problem.grantUrl} target="_blank" rel="noreferrer" mt={6} display="block">
              Grant access on GitHub <IconExternalLink size={12} />
            </Anchor>
          )}
        </Alert>
      )}

      {report && (
        <Stack gap="xs">
          {report.name && (
            <Text size="sm" fw={500}>
              {report.name}
            </Text>
          )}
          {report.description && (
            <Text size="xs" c="dimmed">
              {report.description}
            </Text>
          )}
          <Table withTableBorder verticalSpacing="xs" horizontalSpacing="sm">
            <Table.Thead>
              <Table.Tr>
                <Table.Th>Pool</Table.Th>
                <Table.Th>Scope</Table.Th>
                <Table.Th>Runtime</Table.Th>
                <Table.Th>Size</Table.Th>
                <Table.Th>Runners</Table.Th>
                <Table.Th />
              </Table.Tr>
            </Table.Thead>
            <Table.Tbody>
              {report.pools.map(({ name, action, pool }) => (
                <Table.Tr key={name}>
                  <Table.Td>
                    <Text size="sm" fw={500}>
                      {name}
                    </Text>
                  </Table.Td>
                  <Table.Td>
                    <Text size="sm">{pool.scope}</Text>
                  </Table.Td>
                  <Table.Td>
                    <Badge size="sm" variant="default">
                      {pool.runtime}
                    </Badge>
                  </Table.Td>
                  <Table.Td>
                    <Text size="sm" c="dimmed">
                      {pool.cpus} vCPU · {Math.round(pool.memoryMb / 1024)} GiB
                      {pool.runtime === 'vm' ? ` · ${pool.diskGb} GiB` : ''}
                    </Text>
                  </Table.Td>
                  <Table.Td>
                    <Text size="sm" c="dimmed">
                      {pool.minReplicas === pool.maxReplicas
                        ? pool.maxReplicas
                        : `${pool.minReplicas}–${pool.maxReplicas}`}
                    </Text>
                  </Table.Td>
                  <Table.Td>
                    <Badge size="sm" color={action === 'update' ? 'yellow' : 'blue'} variant="light">
                      {action === 'update' ? 'replaces the pool of this name' : 'new'}
                    </Badge>
                  </Table.Td>
                </Table.Tr>
              ))}
            </Table.Tbody>
          </Table>
          {report.pools.some((outcome) => outcome.action === 'update') && (
            <Text size="xs" c="dimmed">
              Pools written over keep their identity and their runners: each runner is replaced
              gracefully, as it finishes the job it is on.
            </Text>
          )}
        </Stack>
      )}

      <Group justify="flex-end">
        <Button variant="default" onClick={onCancel}>
          Cancel
        </Button>
        {report ? (
          <Button loading={busy} onClick={() => send(false)}>
            Import {report.pools.length} {report.pools.length === 1 ? 'pool' : 'pools'}
          </Button>
        ) : (
          <Button loading={busy} disabled={!ready} onClick={() => send(true)}>
            Preview
          </Button>
        )}
      </Group>
    </Stack>
  )
}
