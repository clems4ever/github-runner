import { useCallback, useEffect, useMemo, useState } from 'react'
import {
  ActionIcon,
  AppShell,
  Badge,
  Burger,
  Group,
  NavLink,
  ScrollArea,
  Text,
  Title,
  Tooltip,
  useMantineColorScheme,
} from '@mantine/core'
import { useDisclosure } from '@mantine/hooks'
import { notifications } from '@mantine/notifications'
import {
  IconGauge,
  IconMoon,
  IconRefresh,
  IconServer2,
  IconSettings,
  IconStack2,
  IconSun,
  IconKey,
} from '@tabler/icons-react'
import {
  api,
  type Credential,
  type Health,
  type Pool,
  type ResourceReport,
  type Runner,
  type Scale,
} from './api'
import { useCrampedHeader } from './responsive'
import { Logo } from './Logo'
import { FleetPage } from './pages/FleetPage'
import { PoolsPage } from './pages/PoolsPage'
import { CredentialsPage } from './pages/CredentialsPage'
import { ResourcesPage } from './pages/ResourcesPage'
import { SettingsPage } from './pages/SettingsPage'

type Page = 'fleet' | 'pools' | 'resources' | 'credentials' | 'settings'

const pages: { key: Page; label: string; icon: typeof IconServer2 }[] = [
  { key: 'fleet', label: 'Fleet', icon: IconServer2 },
  { key: 'pools', label: 'Pools', icon: IconStack2 },
  { key: 'resources', label: 'Resources', icon: IconGauge },
  { key: 'credentials', label: 'Credentials', icon: IconKey },
  { key: 'settings', label: 'Settings', icon: IconSettings },
]

export function App() {
  const crampedHeader = useCrampedHeader()
  const [opened, { toggle }] = useDisclosure()
  const [page, setPage] = useState<Page>('fleet')
  const [pools, setPools] = useState<Pool[]>([])
  const [runners, setRunners] = useState<Runner[]>([])
  const [warnings, setWarnings] = useState<string[]>([])
  const [scaling, setScaling] = useState<Record<string, Scale>>({})
  const [credentials, setCredentials] = useState<Credential[]>([])
  const [health, setHealth] = useState<Health | null>(null)
  const [resources, setResources] = useState<ResourceReport | null>(null)
  const [loading, setLoading] = useState(true)

  const refresh = useCallback(async () => {
    try {
      const [poolList, runnerList, credentialList, healthInfo, resourceReport] = await Promise.all([
        api.pools(),
        api.runners(),
        api.credentials(),
        api.health(),
        api.resources(),
      ])
      setPools(poolList)
      setRunners(runnerList.runners ?? [])
      setWarnings(runnerList.warnings ?? [])
      setScaling(runnerList.scaling ?? {})
      setCredentials(credentialList)
      setHealth(healthInfo)
      setResources(resourceReport)
    } catch (error) {
      notifications.show({
        color: 'red',
        title: 'Cannot reach the daemon',
        message: error instanceof Error ? error.message : String(error),
      })
    } finally {
      setLoading(false)
    }
  }, [])

  useEffect(() => {
    void refresh()
    // The fleet changes without anyone touching this page — a job starts, a
    // runner is replaced — so it polls rather than pretending it is static.
    const timer = setInterval(() => void refresh(), 5000)
    return () => clearInterval(timer)
  }, [refresh])

  const busy = useMemo(() => runners.filter((r) => r.job === 'busy').length, [runners])

  return (
    <AppShell
      header={{ height: 56 }}
      navbar={{ width: 220, breakpoint: 'sm', collapsed: { mobile: !opened } }}
      padding={{ base: 'sm', sm: 'lg' }}
    >
      <AppShell.Header>
        <Group h="100%" px={{ base: 'sm', sm: 'md' }} justify="space-between" wrap="nowrap">
          <Group gap="sm" wrap="nowrap" style={{ minWidth: 0 }}>
            <Burger opened={opened} onClick={toggle} hiddenFrom="sm" size="sm" />
            <Title order={4} style={{ whiteSpace: 'nowrap' }}>
              runner-fleet
            </Title>
            {runners.length > 0 && !crampedHeader && (
              // The same count twice, because "2 of 5 busy" is what it means and
              // "2/5" is what fits next to the title on a phone. Neither may
              // shrink: a squeezed badge is an empty pill, not a shorter one.
              <>
                <Badge
                  variant="light"
                  color={busy > 0 ? 'blue' : 'gray'}
                  visibleFrom="xs"
                  style={{ flexShrink: 0 }}
                >
                  {busy} of {runners.length} busy
                </Badge>
                <Tooltip label={`${busy} of ${runners.length} runners busy`}>
                  <Badge
                    variant="light"
                    color={busy > 0 ? 'blue' : 'gray'}
                    hiddenFrom="xs"
                    style={{ flexShrink: 0 }}
                  >
                    {busy}/{runners.length}
                  </Badge>
                </Tooltip>
              </>
            )}
          </Group>
          <Group gap="xs" wrap="nowrap">
            <ReconcileButton onDone={refresh} />
            <ColorSchemeToggle />
            {/* The mark anchors the corner opposite the wordmark, so the header
                is bracketed by the product rather than by two grey buttons.
                The extra margin keeps it from reading as a third one. */}
            <Logo size={30} title="" style={{ marginInlineStart: 6 }} />
          </Group>
        </Group>
      </AppShell.Header>

      <AppShell.Navbar p="xs">
        <AppShell.Section grow component={ScrollArea}>
          {pages.map(({ key, label, icon: Icon }) => (
            <NavLink
              key={key}
              active={page === key}
              label={label}
              leftSection={<Icon size={18} stroke={1.5} />}
              onClick={() => {
                setPage(key)
                if (opened) toggle()
              }}
            />
          ))}
        </AppShell.Section>
        <AppShell.Section>
          <Text size="xs" c="dimmed" p="xs">
            {health?.version ?? ''}
          </Text>
        </AppShell.Section>
      </AppShell.Navbar>

      <AppShell.Main>
        {page === 'fleet' && (
          <FleetPage
            runners={runners}
            pools={pools}
            credentials={credentials}
            scaling={scaling}
            warnings={warnings}
            loading={loading}
            onChange={refresh}
          />
        )}
        {page === 'pools' && (
          <PoolsPage
            pools={pools}
            credentials={credentials}
            runners={runners}
            scaling={scaling}
            onChange={refresh}
          />
        )}
        {page === 'resources' && <ResourcesPage report={resources} />}
        {page === 'credentials' && (
          <CredentialsPage credentials={credentials} pools={pools} onChange={refresh} />
        )}
        {page === 'settings' && <SettingsPage health={health} />}
      </AppShell.Main>
    </AppShell>
  )
}

function ReconcileButton({ onDone }: { onDone: () => Promise<void> }) {
  const [running, setRunning] = useState(false)

  return (
    <Tooltip label="Reconcile now">
      <ActionIcon
        variant="default"
        size="lg"
        loading={running}
        aria-label="Reconcile now"
        onClick={async () => {
          setRunning(true)
          try {
            const result = await api.reconcile()
            const count = result.actions?.length ?? 0
            notifications.show({
              color: result.errors?.length ? 'yellow' : 'green',
              title: count === 0 ? 'Nothing to do' : `${count} action${count === 1 ? '' : 's'}`,
              message: result.errors?.length ? result.errors.join('\n') : 'The fleet matches its configuration.',
            })
            await onDone()
          } catch (error) {
            notifications.show({
              color: 'red',
              title: 'Reconcile failed',
              message: error instanceof Error ? error.message : String(error),
            })
          } finally {
            setRunning(false)
          }
        }}
      >
        <IconRefresh size={18} stroke={1.5} />
      </ActionIcon>
    </Tooltip>
  )
}

function ColorSchemeToggle() {
  const { colorScheme, toggleColorScheme } = useMantineColorScheme()
  return (
    <Tooltip label={colorScheme === 'dark' ? 'Light theme' : 'Dark theme'}>
      <ActionIcon variant="default" size="lg" onClick={toggleColorScheme} aria-label="Toggle theme">
        {colorScheme === 'dark' ? <IconSun size={18} stroke={1.5} /> : <IconMoon size={18} stroke={1.5} />}
      </ActionIcon>
    </Tooltip>
  )
}
