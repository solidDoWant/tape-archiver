import { useState } from 'react'
import { sanitizeRedirectPath } from './route'
import { IconSpinner, IconWarning } from './icons'
import Footer from './Footer'

// LoginPage is the styled pre-redirect login screen (Login.dc.html — issue
// #272), replacing the previous behavior of an unauthenticated page request
// getting an immediate, unstyled 302 straight to the IdP (pkg/webauth's
// package doc comment). It has no username/password fields — the OIDC
// provider hosts the actual credential form; this is a landing/status
// screen only, matching Login.dc.html's `state` prop: default, redirecting,
// error-denied, error-expired.
//
// The design's per-provider button label ("Continue with Authentik") needs
// a `providerName` the app has no configured equivalent for today
// (DESIGN_ANALYSIS.md §4/§6 flags this as a possible future
// `OIDC_PROVIDER_LABEL`-style env var) — issue #272 does not ask for that
// config surface, so this uses a generic "Continue with SSO" label instead
// of inventing one.
type LoginState = 'default' | 'redirecting' | 'error-denied' | 'error-expired'

// readLoginState derives the page's state from the URL query string:
// pkg/webauth's OIDC callback handler redirects here with ?error=denied or
// ?error=expired on failure (see webauth.go's loginErrorRedirect), and
// AuthGate (App.tsx) redirects here with ?redirect=<original path> when an
// unauthenticated browser requests any other page.
function readLoginState(search: string): { state: LoginState; redirect: string } {
  const params = new URLSearchParams(search)
  const error = params.get('error')
  const redirect = sanitizeRedirectPath(params.get('redirect'))

  if (error === 'denied') {
    return { state: 'error-denied', redirect }
  }

  if (error === 'expired') {
    return { state: 'error-expired', redirect }
  }

  return { state: 'default', redirect }
}

function LoginPage() {
  const { state: initialState, redirect } = readLoginState(window.location.search)
  const [redirecting, setRedirecting] = useState(false)

  const state: LoginState = redirecting ? 'redirecting' : initialState
  const isError = state === 'error-denied' || state === 'error-expired'
  const hasHeading = state === 'redirecting' || isError

  function signIn() {
    setRedirecting(true)
    // A real browser navigation (not the SPA router's navigate()): this
    // leaves the app entirely for pkg/webauth's GET /auth/login, which
    // redirects on to the IdP.
    window.location.assign(`/auth/login?redirect=${encodeURIComponent(redirect)}`)
  }

  let headingText = ''
  let subText = ''

  if (state === 'redirecting') {
    headingText = 'Redirecting…'
    subText = 'Taking you to your identity provider to complete sign-in. This tab will return automatically.'
  } else if (isError) {
    headingText = 'Sign-in required'
    subText = 'You were signed out. Authenticate again to continue.'
  }

  let errTitle = ''
  let errBody = ''

  if (state === 'error-denied') {
    errTitle = 'Access denied'
    errBody =
      "Your account authenticated but isn't authorized for this archive. Ask the owner to grant access, then try again."
  } else if (state === 'error-expired') {
    errTitle = 'Session expired'
    errBody = 'Your previous session timed out for security. Sign in again to pick up where you left off.'
  }

  let buttonLabel = 'Continue with SSO'
  if (state === 'redirecting') {
    buttonLabel = 'Redirecting…'
  } else if (isError) {
    buttonLabel = 'Sign in'
  }

  return (
    <div className="relative flex min-h-screen items-center justify-center overflow-hidden bg-bg p-8 text-text">
      {/* ambient background: faint concentric reel rings, off to the sides */}
      <div aria-hidden="true" className="pointer-events-none absolute inset-0 overflow-hidden">
        <div className="absolute -top-40 -right-36 h-[520px] w-[520px] rounded-full border border-border-strong opacity-50" />
        <div className="absolute -top-[90px] -right-[70px] h-[380px] w-[380px] rounded-full border border-border opacity-50" />
        <div className="absolute -bottom-48 -left-36 h-[560px] w-[560px] rounded-full border border-border-strong opacity-45" />
        <div className="absolute -bottom-32 -left-20 h-[400px] w-[400px] rounded-full border border-border opacity-45" />
      </div>

      <div className="relative w-full max-w-[400px] rounded-2xl border border-border bg-surface p-9 pb-8 shadow-elevated">
        <div className="flex flex-col items-center gap-3.5 text-center">
          <div className="flex h-12 w-12 items-center justify-center rounded-[13px] bg-text shadow-card">
            <div className="h-[21px] w-[21px] rounded-full border-[3.5px] border-surface" />
          </div>
          <div className="text-xl font-bold tracking-tight">tape-archiver</div>
        </div>

        <div className="my-[22px] h-px bg-border" />

        {hasHeading && (
          <div className="mb-5 text-center">
            <h1 className="text-[15px] font-semibold tracking-tight">{headingText}</h1>
            <p className="mt-1.5 text-[12.5px] leading-relaxed text-text-dim">{subText}</p>
          </div>
        )}

        {isError && (
          <div
            role="alert"
            className="mb-4 flex items-start gap-2.5 rounded-[10px] border border-red-line bg-red-bg p-3"
          >
            <IconWarning className="mt-0.5 h-3.5 w-3.5 flex-none text-red" />
            <div>
              <div className="text-[12.5px] font-semibold text-red">{errTitle}</div>
              <div className="mt-0.5 text-[11.5px] leading-relaxed text-text-dim">{errBody}</div>
            </div>
          </div>
        )}

        <button
          type="button"
          onClick={signIn}
          disabled={state === 'redirecting'}
          className="flex w-full items-center justify-center gap-2.5 rounded-[10px] bg-text px-4 py-3 text-[13.5px] font-semibold text-bg shadow-card transition-opacity enabled:cursor-pointer enabled:hover:opacity-90 enabled:active:translate-y-px disabled:cursor-default disabled:opacity-70"
        >
          {state === 'redirecting' && <IconSpinner className="h-4 w-4 animate-spin" />}
          {buttonLabel}
        </button>

        {isError && (
          <div className="mt-3 text-center">
            <button
              type="button"
              onClick={signIn}
              className="cursor-pointer text-xs text-text-dim underline-offset-2 hover:underline"
            >
              Try a different account
            </button>
          </div>
        )}
      </div>

      <Footer className="absolute inset-x-0 bottom-5 text-center" />
    </div>
  )
}

export default LoginPage
