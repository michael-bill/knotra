import { createContext, useContext, useMemo } from 'react';
import en from '../locales/en.json';
import ru from '../locales/ru.json';

export type Locale = 'en' | 'ru';
export type TranslationValues = Record<string, string | number>;
export type MessageKey = keyof typeof en;

export const LocaleContext = createContext<Locale>('en');
export const localeTag = (locale: Locale): string => (locale === 'ru' ? 'ru-RU' : 'en-US');

export function preferredLocale(
  language = typeof navigator === 'undefined' ? 'en' : navigator.language,
): Locale {
  return /^ru(?:-|$)/i.test(language) ? 'ru' : 'en';
}

export function normalizeLocale(value: unknown): Locale {
  return value === 'en' || value === 'ru' ? value : preferredLocale();
}

export const englishMessages: Record<string, string> = en;
export const russianMessages: Record<string, string> = ru;

export function translate(key: string, locale: Locale, values: TranslationValues = {}): string {
  if (!Object.hasOwn(englishMessages, key)) return key;
  const template =
    locale === 'ru' ? (russianMessages[key] ?? englishMessages[key]) : englishMessages[key];
  return template.replace(/\{(\w+)\}/g, (placeholder, key: string) =>
    Object.hasOwn(values, key) ? String(values[key]) : placeholder,
  );
}

const messageKeys = new Map(Object.entries(en).map(([key, value]) => [value, key]));
const messagePatterns = Object.entries(en)
  .filter(([key, value]) => key.startsWith('messages.') && /\{\w+\}/.test(value))
  .map(([key, value]) => {
    const names: string[] = [];
    const escape = (text: string) => text.replace(/[.*+?^${}()|[\]\\]/g, '\\$&');
    let offset = 0;
    let pattern = '^';
    for (const match of value.matchAll(/\{(\w+)\}/g)) {
      pattern += escape(value.slice(offset, match.index)) + '([\\s\\S]*?)';
      names.push(match[1]);
      offset = match.index! + match[0].length;
    }
    return { key, names, pattern: new RegExp(pattern + escape(value.slice(offset)) + '$') };
  });

// Only use this adapter for errors produced by frontend validators. Engine messages
// and authoring content stay verbatim; normal interface labels always use keys.
export function translateMessage(
  message: string,
  locale: Locale,
  values: TranslationValues = {},
): string {
  if (locale === 'ru' && message.startsWith('Error: ')) {
    const detail = message.slice('Error: '.length);
    const translated = translateMessage(detail, locale, values);
    return translated === detail ? message : translated;
  }
  const key = messageKeys.get(message);
  if (key) return translate(key, locale, values);
  if (Object.hasOwn(en, message)) return translate(message, locale, values);
  if (locale === 'ru' && message.length <= 4096) {
    for (const entry of messagePatterns) {
      const match = entry.pattern.exec(message);
      if (match) {
        const parameters = Object.fromEntries(entry.names.map((name, i) => [name, match[i + 1]]));
        return translate(entry.key, locale, parameters);
      }
    }
  }
  return message;
}

export function useTranslation(locale: Locale) {
  return useMemo(
    () => (message: string, values?: TranslationValues) => translate(message, locale, values),
    [locale],
  );
}

export function useI18n() {
  const locale = useContext(LocaleContext);
  const message = useMemo(
    () => (text: string, values?: TranslationValues) => translateMessage(text, locale, values),
    [locale],
  );
  return { locale, t: useTranslation(locale), message };
}
