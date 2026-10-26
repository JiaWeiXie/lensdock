import { useState, type FormEvent, type ReactNode } from 'react';
import { Button, Field, Notice } from '@/components/ui';
import { describeError } from '@/errors';
import { useI18n, type Lang } from '@/i18n';
import { useSession } from '@/state/session';

/** Local re-entry after sign-out, with token pairing available when required. */
export function Login(): ReactNode {
  const { t, lang, setLang } = useI18n();
  const { login, enterLocal, localAvailable, pairing, loginError, status, retry } = useSession();
  const [token, setToken] = useState('');

  const submit = async (e: FormEvent): Promise<void> => {
    e.preventDefault();
    const ok = await login(token.trim());
    if (ok) setToken('');
  };

  return (
    <main className="login" id="main">
      <div className="login__card">
        <div className="login__brand">
          <svg width="40" height="40" viewBox="0 0 32 32" aria-hidden="true">
            <circle cx="16" cy="16" r="10" fill="none" stroke="var(--teal-bright)" strokeWidth="3" />
            <rect x="13.5" y="13.5" width="5" height="5" fill="var(--amber-bright)" />
          </svg>
          <div>
            <h1>LensDock</h1>
            <p>{t('login.tagline')}</p>
          </div>
        </div>

        {status === 'unreachable' ? (
          <Notice tone="danger" title={t('login.unreachable')} actions={<Button small icon="refresh" onClick={retry}>{t('common.retry')}</Button>}>
            {t('login.unreachableBody')}
          </Notice>
        ) : (
          <>
            {localAvailable && (
              <Notice
                tone="neutral"
                title={t('login.localTitle')}
                actions={
                  <Button small variant="primary" icon="lock" busy={pairing} onClick={() => void enterLocal()}>
                    {t('login.localEnter')}
                  </Button>
                }
              >
                {t('login.localNote')}
              </Notice>
            )}
          <form onSubmit={(e) => void submit(e)} className="login__form">
            <Field label={t('login.token')} hint={t('login.tokenHint')} htmlFor="login-token" error={loginError ? describeError(loginError, t) : null}>
              <input
                id="login-token"
                className="input input--mono"
                type="password"
                name="lensdock-token"
                autoComplete="off"
                autoCapitalize="off"
                spellCheck={false}
                autoFocus
                value={token}
                onChange={(e) => setToken(e.target.value)}
                aria-invalid={loginError ? true : undefined}
              />
            </Field>
            <Button type="submit" variant="primary" icon="lock" busy={pairing} disabled={token.trim() === ''}>
              {t('login.pair')}
            </Button>
          </form>
          </>
        )}

        <p className="login__note">{t('login.note')}</p>
        <label className="login__lang">
          <span className="sr-only">{t('settings.language')}</span>
          <select className="input input--sm" value={lang} onChange={(e) => setLang(e.target.value as Lang)}>
            <option value="zh-TW">繁體中文</option>
            <option value="en-US">English</option>
          </select>
        </label>
      </div>
    </main>
  );
}
