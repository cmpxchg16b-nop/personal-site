"use client";

import { useEffect, useState } from "react";
import { I18nextProvider } from "react-i18next";
import i18n, {
  fallbackLng,
  LANGUAGE_STORAGE_KEY,
  supportedLngs,
  type SupportedLng,
} from "./index";

// detectLanguage picks the language to actually use: an explicit choice
// persisted in localStorage wins, otherwise the browser's preferred language
// (any zh-* tag maps to "zh"), otherwise the fallback.
function detectLanguage(): SupportedLng {
  const saved = window.localStorage.getItem(LANGUAGE_STORAGE_KEY);
  if ((supportedLngs as readonly string[]).includes(saved ?? "")) {
    return saved as SupportedLng;
  }
  return navigator.language.toLowerCase().startsWith("zh") ? "zh" : fallbackLng;
}

// I18nProvider gates the translated tree behind client-side mounting and
// feeds the shared i18next instance to it.
//
// Why the gate: the user's language is known only in the browser
// (localStorage / navigator), so the prerendered HTML cannot carry it.
// Rendering children on the server would either bake in the fallback
// language (hydration mismatch: with streaming SSR the layout's
// language-switch effect can fire before the page boundary hydrates) or
// flash the wrong language. Instead, SSR and the hydration render both
// produce nothing here; after mount we apply the detected language first and
// only then render the app, so the first paint is already correct. It also
// keeps <html lang> and the browser-tab title in sync with the active
// language.
export default function I18nProvider({
  children,
}: {
  children: React.ReactNode;
}) {
  const [ready, setReady] = useState(false);

  useEffect(() => {
    // Resources are bundled synchronously, so changeLanguage applies right
    // away; waiting for its resolution just orders setReady after it.
    void i18n.changeLanguage(detectLanguage()).then(() => setReady(true));

    // Follows the active language: assistive tech tracks <html lang>, and
    // the tab title shows the localized app name. t() reads the language
    // that changeLanguage just applied, so the explicit initial call covers
    // the no-change case where "languageChanged" never fires.
    const syncDocumentLanguage = (lng: string) => {
      document.documentElement.lang = lng;
      document.title = i18n.t("app.title");
    };
    syncDocumentLanguage(i18n.language);
    i18n.on("languageChanged", syncDocumentLanguage);
    return () => {
      i18n.off("languageChanged", syncDocumentLanguage);
    };
  }, []);

  if (!ready) return null;

  return <I18nextProvider i18n={i18n}>{children}</I18nextProvider>;
}
