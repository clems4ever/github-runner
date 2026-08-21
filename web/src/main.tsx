import React from 'react'
import ReactDOM from 'react-dom/client'
import { createTheme, MantineProvider } from '@mantine/core'
import { Notifications } from '@mantine/notifications'
import '@mantine/core/styles.css'
import '@mantine/notifications/styles.css'
import { App } from './App'

const theme = createTheme({
  primaryColor: 'indigo',
  defaultRadius: 'md',
  fontFamilyMonospace: 'ui-monospace, SFMono-Regular, Menlo, monospace',
})

ReactDOM.createRoot(document.getElementById('root')!).render(
  <React.StrictMode>
    <MantineProvider theme={theme} defaultColorScheme="auto">
      <Notifications position="top-right" />
      <App />
    </MantineProvider>
  </React.StrictMode>,
)
