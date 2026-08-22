"use client";

import {
  Dialog,
  DialogContent,
  Box,
  Button,
  CircularProgress,
  SvgIcon,
  useColorScheme,
  useMediaQuery,
} from "@mui/material";
import PersonOutlineIcon from "@mui/icons-material/Person";
import MicrosoftIcon from "@mui/icons-material/Microsoft";
import kioubitLoginSVGDark from "./kioubit-login-dark.svg";
import kioubitLoginSVGLight from "./kioubit-login-light.svg";
import iedonLoginSVG from "./iedon-login.svg";
import hexpAuthJpeg from "./hexpauth.jpeg";
import hexpAuthDarkPng from "./hexpauth-dark.png";
import Image from "next/image";
import { useRouter, useSearchParams } from "next/navigation";
import { useTranslation } from "react-i18next";
import { useLoginOptions } from "@/hooks/useLoginOptions";
import { Fragment, ReactNode } from "react";

const defaultIconLen = 20;

function GitHubIcon(props: React.ComponentProps<typeof SvgIcon>) {
  return (
    <SvgIcon {...props} viewBox="0 0 24 24">
      <path d="M12 1.27a11 11 0 00-3.48 21.46c.55.1.75-.23.75-.53v-1.85c-3.03.65-3.67-1.45-3.67-1.45-.5-1.27-1.21-1.6-1.21-1.6-.98-.67.08-.65.08-.65 1.09.08 1.66 1.12 1.66 1.12.96 1.65 2.52 1.18 3.14.9.1-.7.38-1.18.68-1.45-2.42-.27-4.96-1.21-4.96-5.37 0-1.19.42-2.16 1.12-2.92-.11-.28-.49-1.38.11-2.88 0 0 .91-.29 2.97 1.11a10.32 10.32 0 015.42 0c2.06-1.4 2.97-1.11 2.97-1.11.59 1.5.21 2.6.11 2.88.69.76 1.12 1.73 1.12 2.92 0 4.17-2.55 5.1-4.97 5.37.39.33.74.99.74 1.99v2.95c0 .3.2.64.75.53A11 11 0 0012 1.27z" />
    </SvgIcon>
  );
}

function GoogleIcon(props: React.ComponentProps<typeof SvgIcon>) {
  return (
    <SvgIcon {...props} viewBox="0 0 24 24">
      <path
        d="M22.56 12.25c0-.78-.07-1.53-.2-2.25H12v4.26h5.92a5.06 5.06 0 01-2.2 3.32v2.77h3.57c2.08-1.92 3.28-4.74 3.28-8.1z"
        fill="#4285F4"
      />
      <path
        d="M12 23c2.97 0 5.46-.98 7.28-2.66l-3.57-2.77c-.98.66-2.23 1.06-3.71 1.06-2.86 0-5.29-1.93-6.16-4.53H2.18v2.84C3.99 20.53 7.7 23 12 23z"
        fill="#34A853"
      />
      <path
        d="M5.84 14.09c-.22-.66-.35-1.36-.35-2.09s.13-1.43.35-2.09V7.07H2.18C1.43 8.55 1 10.22 1 12s.43 3.45 1.18 4.93l2.85-2.22.81-.62z"
        fill="#FBBC05"
      />
      <path
        d="M12 5.38c1.62 0 3.06.56 4.21 1.64l3.15-3.15C17.45 2.09 14.97 1 12 1 7.7 1 3.99 3.47 2.18 7.07l3.66 2.84c.87-2.6 3.3-4.53 6.16-4.53z"
        fill="#EA4335"
      />
    </SvgIcon>
  );
}

function KioubitLoginIcon() {
  const prefersDarkMode: boolean = useMediaQuery(
    "(prefers-color-scheme: dark)",
  );
  const colorScheme = useColorScheme();
  const color: "light" | "dark" =
    colorScheme.mode === "dark"
      ? "dark"
      : colorScheme.mode === "light"
        ? "light"
        : colorScheme.mode === "system"
          ? prefersDarkMode
            ? "dark"
            : "light"
          : "dark";

  if (color === "dark") {
    return (
      <Image
        width={defaultIconLen}
        height={defaultIconLen}
        alt={"Kioubit Auth"}
        src={kioubitLoginSVGDark}
      />
    );
  } else {
    return (
      <Image
        width={defaultIconLen}
        height={defaultIconLen}
        alt={"Kioubit Auth"}
        src={kioubitLoginSVGLight}
      />
    );
  }
}

function HexpLoginIcon() {
  const prefersDarkMode: boolean = useMediaQuery(
    "(prefers-color-scheme: dark)",
  );
  const colorScheme = useColorScheme();
  const color: "light" | "dark" =
    colorScheme.mode === "dark"
      ? "dark"
      : colorScheme.mode === "light"
        ? "light"
        : colorScheme.mode === "system"
          ? prefersDarkMode
            ? "dark"
            : "light"
          : "dark";

  // The HExperiment Network logo is wide rather than square, so height is
  // fixed and width scales to preserve its aspect ratio. The dark variant
  // (hexpauth-dark.png) has a transparent background instead of white.
  return (
    <Image
      alt={"HExpAuth"}
      src={color === "dark" ? hexpAuthDarkPng : hexpAuthJpeg}
      style={{ height: defaultIconLen, width: "auto" }}
    />
  );
}

// wellKnownKindVisitor is the login option kind of the built-in visitor
// (anonymous) login. Unlike the OAuth kinds it is not an IdP name, so its
// button always uses the well-known label below, taking precedence over any
// server-provided label.
const wellKnownKindVisitor = "visitor";

// loginIconGenerators maps a login option name to the generator of its
// button icon.
const loginIconGenerators: Record<string, () => ReactNode> = {
  github: () => <GitHubIcon />,
  google: () => <GoogleIcon />,
  entra: () => <MicrosoftIcon />,
  iedon: () => (
    <Image
      width={defaultIconLen}
      height={defaultIconLen}
      alt={"iEdon Auth"}
      src={iedonLoginSVG}
    />
  ),
  kioubit: () => <KioubitLoginIcon />,
  hexp: () => <HexpLoginIcon />,
  [wellKnownKindVisitor]: () => <PersonOutlineIcon />,
};

// getLoginIcon resolves a login option name to its icon generator, falling
// back to the visitor icon when the name is not recognized.
function getLoginIcon(name: string): () => ReactNode {
  return loginIconGenerators[name] ?? loginIconGenerators[wellKnownKindVisitor];
}

export default function LoginPage() {
  const router = useRouter();
  const { t } = useTranslation();
  // Where to return after a successful login, carried by the
  // redirect_if_succeed query parameter (the login button in the top bar
  // sets it to the page the visitor came from). It is forwarded to the
  // options' start URLs; the start handlers stash it as a cookie the auth
  // callbacks redirect to.
  const redirectIfSucceed = useSearchParams().get("redirect_if_succeed") ?? "";
  // Login options are served by the server from its configuration document
  // (GET /api/login/loginoptions), so deployments can change the IdP list
  // without a frontend rebuild.
  const { data: loginOptions, isPending, isError } = useLoginOptions();

  const handleLogin = (loginURL: string) => {
    // Append redirect_if_succeed to the option's start URL unless it
    // already carries one.
    if (loginURL && redirectIfSucceed) {
      if (!loginURL.includes("redirect_if_succeed=")) {
        loginURL += `${loginURL.includes("?") ? "&" : "?"}redirect_if_succeed=${encodeURIComponent(redirectIfSucceed)}`;
      }
    }
    if (loginURL) {
      // router.push covers both URL shapes: same-origin paths navigate
      // client-side, while absolute external URLs (the env-configured OAuth
      // providers) fall back to a full browser navigation.
      router.push(loginURL);
    } else {
      alert("no supported");
    }
  };

  const activeLoginOptions = (loginOptions ?? []).filter(
    (option) => !!option.loginURL,
  );

  return (
    <Dialog
      open
      fullWidth
      maxWidth="xs"
      onClose={() => {
        // ignore cancle event here
      }}
    >
      <DialogContent sx={{ py: 4 }}>
        <Box
          sx={{
            display: "flex",
            flexDirection: "column",
            gap: 2,
            alignItems: "stretch",
          }}
        >
          {isPending ? (
            <Box
              sx={{
                display: "flex",
                alignItems: "center",
                justifyContent: "center",
              }}
            >
              <CircularProgress size={28} />
            </Box>
          ) : isError ? (
            <Box
              sx={{
                display: "flex",
                alignItems: "center",
                justifyContent: "center",
              }}
            >
              {t("login.loadFailed")}
            </Box>
          ) : activeLoginOptions.length > 0 ? (
            activeLoginOptions.map((option) => (
              <Fragment key={option.name}>
                <Button
                  variant="outlined"
                  size="large"
                  startIcon={getLoginIcon(option.kind)()}
                  onClick={() => handleLogin(option.loginURL)}
                  sx={{ textTransform: "none" }}
                >
                  {option.kind === wellKnownKindVisitor
                    ? t("login.signInAsVisitor")
                    : option.label ||
                      t("login.signInWith", { name: option.displayName })}
                </Button>
              </Fragment>
            ))
          ) : (
            <Box
              sx={{
                display: "flex",
                alignItems: "center",
                justifyContent: "center",
              }}
            >
              {t("login.empty")}
            </Box>
          )}
        </Box>
      </DialogContent>
    </Dialog>
  );
}
