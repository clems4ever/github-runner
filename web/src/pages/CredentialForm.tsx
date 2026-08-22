import { useState } from 'react'
import {
  Alert,
  Anchor,
  Button,
  Group,
  NumberInput,
  PasswordInput,
  SegmentedControl,
  Stack,
  Text,
  Textarea,
  TextInput,
} from '@mantine/core'
import { notifications } from '@mantine/notifications'
import { IconInfoCircle } from '@tabler/icons-react'
import { api, type Credential, type CredentialKind } from '../api'

/**
 * Adding a credential, or replacing the secret of one that exists.
 *
 * The two kinds are genuinely different things — a token is a string belonging
 * to a person, an app is an identity with a key — so the form changes shape
 * rather than pretending they are the same field with a different label.
 */
export function CredentialForm({
  rotating,
  onSaved,
  onCancel,
}: {
  rotating: Credential | null
  onSaved: () => Promise<void>
  onCancel: () => void
}) {
  const [kind, setKind] = useState<CredentialKind>(rotating?.kind ?? 'app')
  const [name, setName] = useState('')
  const [secret, setSecret] = useState('')
  const [appId, setAppId] = useState<number | string>('')
  const [installationId, setInstallationId] = useState<number | string>('')
  const [saving, setSaving] = useState(false)

  // When replacing a secret, the kind and the app are already settled: only
  // the secret itself is in question.
  const effectiveKind = rotating?.kind ?? kind
  const ready = secret.trim() !== '' && (rotating !== null || name.trim() !== '') &&
    (effectiveKind === 'pat' || rotating !== null || Number(appId) > 0)

  const submit = async () => {
    setSaving(true)
    try {
      if (rotating) {
        await api.rotateCredential(rotating.id, secret)
        notifications.show({
          color: 'green',
          title: 'Secret replaced',
          message: 'Runners using it are replaced gracefully, as they finish their jobs.',
        })
      } else {
        await api.createCredential({
          name,
          kind: effectiveKind,
          secret,
          appId: effectiveKind === 'app' ? Number(appId) : undefined,
          installationId:
            effectiveKind === 'app' && Number(installationId) > 0 ? Number(installationId) : undefined,
        })
      }
      await onSaved()
    } catch (error) {
      notifications.show({
        color: 'red',
        title: 'Could not save the credential',
        message: error instanceof Error ? error.message : String(error),
      })
    } finally {
      setSaving(false)
    }
  }

  return (
    <Stack>
      {!rotating && (
        <>
          <SegmentedControl
            fullWidth
            value={kind}
            onChange={(value) => setKind(value as CredentialKind)}
            data={[
              { value: 'app', label: 'GitHub App' },
              { value: 'pat', label: 'Personal access token' },
            ]}
          />
          <TextInput
            label="Name"
            placeholder={kind === 'app' ? 'runner fleet app' : 'personal access token'}
            withAsterisk
            value={name}
            onChange={(event) => setName(event.currentTarget.value)}
          />
        </>
      )}

      {effectiveKind === 'app' ? (
        <>
          {!rotating && (
            <Alert variant="light" color="blue" icon={<IconInfoCircle size={18} />}>
              <Text size="sm">
                Register an App under your account, set <b>Where can this GitHub App be installed</b>{' '}
                to <b>Only on this account</b>, turn webhooks off, and give it{' '}
                <b>Repository permissions → Administration: Read and write</b>. Then install it and
                pick the repositories it may use.
              </Text>
              <Text size="xs" c="dimmed" mt={6}>
                Nothing here needs to be reachable from the internet — the daemon only makes
                outbound calls.{' '}
                <Anchor
                  size="xs"
                  href="https://docs.github.com/en/apps/creating-github-apps/setting-up-a-github-app/creating-a-github-app"
                  target="_blank"
                  rel="noreferrer"
                >
                  GitHub's instructions
                </Anchor>
              </Text>
            </Alert>
          )}

          {!rotating && (
            <Group grow align="flex-start">
              <NumberInput
                label="App ID"
                description="On the app's settings page"
                withAsterisk
                min={1}
                value={appId}
                onChange={setAppId}
              />
              <NumberInput
                label="Installation ID"
                description="Optional — found automatically"
                min={1}
                value={installationId}
                onChange={setInstallationId}
              />
            </Group>
          )}

          <Textarea
            label="Private key"
            description="The .pem GitHub gave you when you generated it"
            placeholder={'-----BEGIN RSA PRIVATE KEY-----\n…'}
            withAsterisk
            rows={6}
            styles={{ input: { fontFamily: 'var(--mantine-font-family-monospace)', fontSize: 12 } }}
            value={secret}
            onChange={(event) => setSecret(event.currentTarget.value)}
          />
        </>
      ) : (
        <PasswordInput
          label="Token"
          description="Administration: Read and write on the repositories it covers"
          placeholder="github_pat_..."
          withAsterisk
          value={secret}
          onChange={(event) => setSecret(event.currentTarget.value)}
        />
      )}

      {rotating && (
        <Text size="xs" c="dimmed">
          Pools using this credential will replace their runners gracefully, as each finishes the
          job it is on.
        </Text>
      )}

      <Group justify="flex-end">
        <Button variant="default" onClick={onCancel}>
          Cancel
        </Button>
        <Button onClick={submit} loading={saving} disabled={!ready}>
          Save
        </Button>
      </Group>
    </Stack>
  )
}
