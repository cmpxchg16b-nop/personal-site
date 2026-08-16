import i18n from "i18next";
import { initReactI18next } from "react-i18next";
import { enUS, zhCN } from "date-fns/locale";
import type { Locale } from "date-fns";
import en from "./locales/en.json";
import zh from "./locales/zh.json";

// localStorage key under which the user's explicit language choice is kept.
export const LANGUAGE_STORAGE_KEY = "personal-site-lang";

export const supportedLngs = ["en", "zh"] as const;
export type SupportedLng = (typeof supportedLngs)[number];

export const fallbackLng: SupportedLng = "en";

// Per-language helpers for locale-aware formatting outside the translation
// bundles: date-fns needs its own Locale objects (relative times like
// "3 hours ago" / "3 小时前"), and Intl-based APIs (Date#toLocaleString)
// take a BCP 47 tag.
const dateFnsLocales: Record<SupportedLng, Locale> = { en: enUS, zh: zhCN };
const localeTags: Record<SupportedLng, string> = { en: "en-US", zh: "zh-CN" };

// Both helpers accept any i18next language string and fall back to English
// for languages without an explicit mapping.
export function dateFnsLocaleFor(lng: string): Locale {
  return dateFnsLocales[lng as SupportedLng] ?? dateFnsLocales[fallbackLng];
}

export function localeTagFor(lng: string): string {
  return localeTags[lng as SupportedLng] ?? localeTags[fallbackLng];
}

export const resources = {
  en: { translation: en },
  zh: { translation: zh },
};

// Initialize with the fallback language as a sane default; I18nProvider
// applies the user's persisted/browser-preferred language on mount, before
// the translated tree is revealed (see its comment for why).
i18n.use(initReactI18next).init({
  resources,
  lng: fallbackLng,
  fallbackLng,
  interpolation: {
    // React already escapes interpolated values.
    escapeValue: false,
  },
  react: {
    // Resources are bundled synchronously via JSON imports, so there is
    // nothing to suspend on; disabling suspense keeps static prerendering
    // straightforward.
    useSuspense: false,
  },
});

export default i18n;
