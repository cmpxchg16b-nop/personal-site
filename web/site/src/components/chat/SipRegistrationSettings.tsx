"use client";

import { useState, type FormEvent } from "react";
import {
  Alert,
  Box,
  Button,
  Chip,
  IconButton,
  MenuItem,
  Paper,
  Stack,
  TextField,
  Typography,
  type SxProps,
  type Theme,
} from "@mui/material";
import AddIcon from "@mui/icons-material/Add";
import ArrowBackIcon from "@mui/icons-material/ArrowBack";
import { useTranslation } from "react-i18next";

const TRANSPORTS = ["UDP", "TCP", "TLS", "WebSocket"] as const;
type SipTransport = (typeof TRANSPORTS)[number];

type SipRegistration = {
  id: string;
  server: string;
  username: string;
  transport: SipTransport;
};

// SipRegistrationSettings is a frontend-only scaffold. Configurations live in
// component memory; passwords are neither rendered nor persisted.
export default function SipRegistrationSettings({
  onBack,
  sx,
}: {
  onBack: () => void;
  sx?: SxProps<Theme>;
}) {
  const { t } = useTranslation();
  const [server, setServer] = useState("");
  const [username, setUsername] = useState("");
  const [password, setPassword] = useState("");
  const [transport, setTransport] = useState<SipTransport>("UDP");
  const [registrations, setRegistrations] = useState<SipRegistration[]>([]);

  const addRegistration = (event: FormEvent) => {
    event.preventDefault();
    const trimmedServer = server.trim();
    const trimmedUsername = username.trim();
    if (trimmedServer === "" || trimmedUsername === "") return;
    setRegistrations((current) => [
      ...current,
      {
        id: crypto.randomUUID(),
        server: trimmedServer,
        username: trimmedUsername,
        transport,
      },
    ]);
    setServer("");
    setUsername("");
    setPassword("");
  };

  return (
    <Box
      id="sip-registration-settings"
      sx={[
        {
          flexGrow: 1,
          minWidth: 0,
          minHeight: 0,
          display: "flex",
          flexDirection: "column",
        },
        ...(Array.isArray(sx) ? sx : [sx]),
      ]}
    >
      <Box
        sx={{
          display: "flex",
          alignItems: "center",
          gap: 1,
          px: 2,
          py: 1.25,
          borderBottom: 1,
          borderColor: "divider",
          flexShrink: 0,
        }}
      >
        <IconButton
          onClick={onBack}
          aria-label={t("chat.sip.back")}
          sx={{ display: { xs: "inline-flex", sm: "none" } }}
        >
          <ArrowBackIcon />
        </IconButton>
        <Typography variant="subtitle1" sx={{ fontWeight: 600 }}>
          {t("chat.sip.title")}
        </Typography>
      </Box>
      <Box sx={{ flexGrow: 1, minHeight: 0, overflowY: "auto" }}>
        <Stack
          spacing={2.5}
          sx={{ width: "100%", maxWidth: 760, mx: "auto", p: 3 }}
        >
          <Alert severity="info">{t("chat.sip.placeholder")}</Alert>
          <Paper
            variant="outlined"
            sx={{
              p: 2,
              display: "flex",
              alignItems: "center",
              justifyContent: "space-between",
              gap: 2,
            }}
          >
            <Box>
              <Typography variant="body2" color="text.secondary">
                {t("chat.sip.currentStatus")}
              </Typography>
              <Typography variant="subtitle2" sx={{ mt: 0.5 }}>
                {t("chat.sip.title")}
              </Typography>
            </Box>
            <Chip label={t("chat.sip.unregistered")} variant="outlined" />
          </Paper>

          {registrations.length > 0 && (
            <Box>
              <Typography variant="subtitle2" sx={{ mb: 1 }}>
                {t("chat.sip.configurations")}
              </Typography>
              <Stack spacing={1}>
                {registrations.map((registration) => (
                  <Paper key={registration.id} variant="outlined" sx={{ p: 2 }}>
                    <Box
                      sx={{
                        display: "flex",
                        alignItems: "flex-start",
                        justifyContent: "space-between",
                        gap: 2,
                      }}
                    >
                      <Box sx={{ minWidth: 0 }}>
                        <Typography
                          variant="body2"
                          sx={{ overflowWrap: "anywhere" }}
                        >
                          {registration.username}@{registration.server}
                        </Typography>
                        <Typography variant="caption" color="text.secondary">
                          {registration.transport}
                        </Typography>
                      </Box>
                      <Chip
                        size="small"
                        label={t("chat.sip.unregistered")}
                        variant="outlined"
                      />
                    </Box>
                  </Paper>
                ))}
              </Stack>
            </Box>
          )}

          <Paper variant="outlined" sx={{ p: 2.5 }}>
            <Typography variant="subtitle2" sx={{ mb: 2 }}>
              {t("chat.sip.add")}
            </Typography>
            <Box component="form" onSubmit={addRegistration}>
              <Box
                sx={{
                  display: "grid",
                  gridTemplateColumns: { xs: "1fr", sm: "1fr 1fr" },
                  gap: 2,
                }}
              >
                <TextField
                  required
                  label={t("chat.sip.server")}
                  placeholder="sip.example.com"
                  value={server}
                  onChange={(event) => setServer(event.target.value)}
                  size="small"
                />
                <TextField
                  select
                  label={t("chat.sip.transport")}
                  value={transport}
                  onChange={(event) =>
                    setTransport(event.target.value as SipTransport)
                  }
                  size="small"
                >
                  {TRANSPORTS.map((option) => (
                    <MenuItem key={option} value={option}>
                      {option}
                    </MenuItem>
                  ))}
                </TextField>
                <TextField
                  required
                  label={t("chat.sip.username")}
                  value={username}
                  onChange={(event) => setUsername(event.target.value)}
                  autoComplete="username"
                  size="small"
                />
                <TextField
                  label={t("chat.sip.password")}
                  type="password"
                  value={password}
                  onChange={(event) => setPassword(event.target.value)}
                  autoComplete="new-password"
                  size="small"
                />
              </Box>
              <Button
                type="submit"
                variant="contained"
                startIcon={<AddIcon />}
                disabled={server.trim() === "" || username.trim() === ""}
                sx={{ mt: 2.5 }}
              >
                {t("chat.sip.submit")}
              </Button>
            </Box>
          </Paper>
        </Stack>
      </Box>
    </Box>
  );
}
