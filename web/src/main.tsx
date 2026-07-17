import { StrictMode } from 'react'
import { createRoot } from 'react-dom/client'
import './index.css'
import App from './App.tsx'
import ErrorBoundary from './ErrorBoundary.tsx'
import { applyTheme, resolveInitialTheme } from './theme.ts'

// Applied synchronously here, before the first render (not from inside a
// React effect), so a dark-mode operator never sees a flash of the light
// theme while React mounts.
applyTheme(resolveInitialTheme())

createRoot(document.getElementById('root')!).render(
  <StrictMode>
    <ErrorBoundary label="the app">
      <App />
    </ErrorBoundary>
  </StrictMode>,
)
