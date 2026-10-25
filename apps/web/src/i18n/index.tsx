import { createContext, useCallback, useContext, useEffect, useMemo, useState, type ReactNode } from 'react';
import { enUS } from './en-US';
import { zhTW, type MessageKey } from './zh-TW';

export type Lang = 'zh-TW' | 'en-US';
export type { MessageKey };

const STORAGE_KEY = 'lensdock.lang';
const DICTS: Record<Lang, Record<MessageKey, string>> = { 'zh-TW': zhTW, 'en-US': enUS };

export type MessageParams = Record<string, string | number>;

/** Replaces `{name}` placeholders. Unknown placeholders are left visible so gaps are noticed. */
export function format(template: string, params?: MessageParams): string {
  if (!params) return template;
  return template.replace(/\{(\w+)\}/g, (m, name: string) => {
    const v = params[name];
    return v === undefined ? m : String(v);
  });
}

export function translate(lang: Lang, key: MessageKey, params?: MessageParams): string {
  return format(DICTS[lang][key] ?? key, params);
}

function initialLang(): Lang {
  const saved = localStorage.getItem(STORAGE_KEY);
  if (saved === 'zh-TW' || saved === 'en-US') return saved;
  return 'zh-TW';
}

interface I18n {
  lang: Lang;
  /** BCP 47 locale for Intl formatting. */
  locale: string;
  setLang: (lang: Lang) => void;
  t: (key: MessageKey, params?: MessageParams) => string;
}

const Ctx = createContext<I18n | null>(null);

export function I18nProvider({ children }: { children: ReactNode }): ReactNode {
  const [lang, setLangState] = useState<Lang>(initialLang);

  useEffect(() => {
    document.documentElement.lang = lang;
    document.title = translate(lang, 'app.title');
  }, [lang]);

  const setLang = useCallback((next: Lang) => {
    localStorage.setItem(STORAGE_KEY, next);
    setLangState(next);
  }, []);

  const t = useCallback((key: MessageKey, params?: MessageParams) => translate(lang, key, params), [lang]);
  const value = useMemo<I18n>(() => ({ lang, locale: lang, setLang, t }), [lang, setLang, t]);
  return <Ctx.Provider value={value}>{children}</Ctx.Provider>;
}

export function useI18n(): I18n {
  const v = useContext(Ctx);
  if (!v) throw new Error('useI18n outside I18nProvider');
  return v;
}
