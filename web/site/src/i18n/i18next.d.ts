import "i18next";
import type { resources } from "./index";

// Type augmentation so useTranslation's t() knows the translation keys and
// interpolations from the English resource bundle (the reference locale).
declare module "i18next" {
  interface CustomTypeOptions {
    defaultNS: "translation";
    resources: (typeof resources)["en"];
  }
}
