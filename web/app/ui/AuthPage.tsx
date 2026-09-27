'use client';

import Link from 'next/link';
import { FormEvent, useEffect, useState } from 'react';
import { api, jsonBody } from '../lib/api';
import { authPageState } from '../lib/auth-status.mjs';
import { postLoginDestination } from '../lib/auth-redirect.mjs';
import { BrandMark } from './BrandMark';
import { Icon } from './icons';

export function AuthPage({ mode }: { mode: 'login' | 'signup' }) {
  const [error, setError] = useState('');
  const [busy, setBusy] = useState(false);
  const [next, setNext] = useState('');
  const [host, setHost] = useState('');
  const [setupRequired, setSetupRequired] = useState<boolean | null>(null);
  const [setupToken, setSetupToken] = useState('');
  const signup = mode === 'signup';
  const pageState = authPageState(mode, setupRequired);

  useEffect(() => {
    setNext(new URLSearchParams(window.location.search).get('next') ?? '');
    setSetupToken(new URLSearchParams(window.location.search).get('setup') ?? '');
    setHost(window.location.host);
    let active = true;
    api<{ setupRequired: boolean }>('/auth/status').then(({ setupRequired: required }) => {
      if (!active) return;
      setSetupRequired(required);
      if (authPageState(mode, required).redirectSignupToLogin) {
        const requested = new URLSearchParams(window.location.search).get('next');
        window.location.replace(`/login/${requested ? `?next=${encodeURIComponent(requested)}` : ''}`);
      }
    }).catch(() => {});
    return () => { active = false; };
  }, [mode]);

  async function submit(event: FormEvent<HTMLFormElement>) {
    event.preventDefault();
    setError('');
    setBusy(true);
    const form = new FormData(event.currentTarget);
    try {
      await api(signup ? '/auth/signup' : '/auth/login', {
        method: 'POST',
        body: jsonBody({ email: form.get('email'), password: form.get('password'), ...(signup && setupToken ? { setupToken } : {}) }),
      });
      window.location.assign(postLoginDestination(window.location.search, window.location.origin));
    } catch (cause) {
      setError(cause instanceof Error ? cause.message : 'Unable to sign in. Try again.');
    } finally {
      setBusy(false);
    }
  }

  return (
    <main className="auth-page">
      <section className="auth-form-wrap">
        <Link className="brand auth-brand" href="/"><BrandMark size={20} animated={busy} /><span>boxes</span></Link>
        <h1>{signup ? 'Create the owner account' : 'Sign in'}</h1>
        <p className="auth-host">Server: <code>{host}</code></p>
        {signup && setupRequired && !setupToken && <p className="form-error" role="alert">Use the owner setup link printed by the installer.</p>}
        <form className="form-stack" onSubmit={submit}>
          <label>Email address<input name="email" type="email" autoComplete="email" required /></label>
          <label>Password<input name="password" type="password" autoComplete={signup ? 'new-password' : 'current-password'} minLength={signup ? 12 : undefined} required />{signup && <small className="help-text">At least 12 characters</small>}</label>
          {error && <p className="form-error" role="alert">{error}</p>}
          <button className="button button-primary button-wide" disabled={busy} type="submit">{busy ? 'Working...' : signup ? <><Icon name="plus" size={14} />Create account</> : 'Sign in'}</button>
        </form>
        {(signup || pageState.showCreateOwnerLink) && <p className="auth-switch">{signup ? 'Already have an account?' : 'Need to create the owner account?'} <Link href={`${signup ? '/login/' : '/signup/'}${next ? `?next=${encodeURIComponent(next)}` : ''}`}>{signup ? 'Sign in' : 'Create account'}</Link></p>}
      </section>
    </main>
  );
}
