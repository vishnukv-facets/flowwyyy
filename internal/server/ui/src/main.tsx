import { createRoot } from 'react-dom/client'
import { QueryClientProvider } from '@tanstack/react-query'

import '@fontsource/plus-jakarta-sans/latin-400.css'
import '@fontsource/plus-jakarta-sans/latin-500.css'
import '@fontsource/plus-jakarta-sans/latin-600.css'
import '@fontsource/plus-jakarta-sans/latin-700.css'
import '@fontsource/jetbrains-mono/latin-400.css'
import '@fontsource/jetbrains-mono/latin-500.css'

import './styles/tokens.css'
import './styles/base.css'
import './styles/hljs.css'
import './styles/app.css'

import { queryClient } from './lib/query'
import { App } from './app'
import { BootGate } from './components/BootSplash'

createRoot(document.getElementById('root')!).render(
  <QueryClientProvider client={queryClient}>
    <BootGate>
      <App />
    </BootGate>
  </QueryClientProvider>,
)
