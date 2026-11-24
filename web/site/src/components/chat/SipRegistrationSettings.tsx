"use client";

import { useEffect, useMemo, useState, type FormEvent } from "react";
import {
  Alert,
  Box,
  Button,
  Chip,
  CircularProgress,
  Dialog,
  DialogActions,
  DialogContent,
  DialogTitle,
  FormControlLabel,
  IconButton,
  MenuItem,
  Paper,
  Stack,
  Switch,
  TextField,
  Typography,
  type SxProps,
  type Theme,
} from "@mui/material";
import AddIcon from "@mui/icons-material/Add";
import ArrowBackIcon from "@mui/icons-material/ArrowBack";
import DeleteOutlineIcon from "@mui/icons-material/DeleteOutlined";
import EditIcon from "@mui/icons-material/Edit";
import { useTranslation } from "react-i18next";

import {
  useCreateSipCredential,
  useDeleteSipCredential,
  useSipCredentials,
  useUpdateSipCredential,
  type Credential,
  type CredentialInput,
  type SipRegistrationState,
  type SipTransport,
} from "@/hooks/useSipCredentials";

const TRANSPORTS: readonly SipTransport[] = ["udp", "tcp", "tls", "ws"];
const TRANSPORT_LABELS: Record<SipTransport, string> = {
  udp: "UDP",
  tcp: "TCP",
  tls: "TLS",
  ws: "WebSocket",
};
const STATE_COLORS: Record<
  SipRegistrationState,
  "default" | "info" | "success" | "error"
> = {
  disabled: "default",
  registering: "info",
  registered: "success",
  failed: "error",
};

type CredentialForm = {
  server: string;
  username: string;
  password: string;
  transport: SipTransport;
};

type CredentialEditor =
  { mode: "add" } | { mode: "edit"; credential: Credential };

const EMPTY_FORM: CredentialForm = {
  server: "",
  username: "",
  password: "",
  transport: "udp",
};

function errorMessage(error: unknown, fallback: string): string {
  return error instanceof Error && error.message !== ""
    ? error.message
    : fallback;
}

function formatTimestamp(value: string, locale: string): string {
  const date = new Date(value);
  if (Number.isNaN(date.getTime())) return value;
  return new Intl.DateTimeFormat(locale, {
    dateStyle: "medium",
    timeStyle: "medium",
  }).format(date);
}

export default function SipRegistrationSettings({
  onBack,
  sx,
}: {
  onBack: () => void;
  sx?: SxProps<Theme>;
}) {
  const { t, i18n } = useTranslation();
  const credentialsQuery = useSipCredentials();
  const createMutation = useCreateSipCredential();
  const editorUpdateMutation = useUpdateSipCredential();
  const toggleMutation = useUpdateSipCredential();
  const deleteMutation = useDeleteSipCredential();

  const [editor, setEditor] = useState<CredentialEditor | null>(null);
  const [form, setForm] = useState<CredentialForm>(EMPTY_FORM);
  const [deleting, setDeleting] = useState<Credential | null>(null);

  const credentials = useMemo(
    () => credentialsQuery.data ?? [],
    [credentialsQuery.data],
  );
  useEffect(() => {
    for (const credential of credentials) {
      const error = credential.registration.last_error;
      if (error) {
        console.error("SIP registration failed", {
          credentialId: credential.id,
          sipUri: credential.sip_uri,
          attempts: credential.registration.attempts,
          error,
        });
      }
    }
  }, [credentials]);

  const isSaving = createMutation.isPending || editorUpdateMutation.isPending;
  const isCredentialBusy =
    editorUpdateMutation.isPending ||
    toggleMutation.isPending ||
    deleteMutation.isPending;
  const saveError =
    editor?.mode === "add" ? createMutation.error : editorUpdateMutation.error;

  const openAddDialog = () => {
    if (!createMutation.isPending) createMutation.reset();
    if (!editorUpdateMutation.isPending) editorUpdateMutation.reset();
    if (!toggleMutation.isPending) toggleMutation.reset();
    setForm(EMPTY_FORM);
    setEditor({ mode: "add" });
  };

  const openEditDialog = (credential: Credential) => {
    if (!createMutation.isPending) createMutation.reset();
    if (!editorUpdateMutation.isPending) editorUpdateMutation.reset();
    if (!toggleMutation.isPending) toggleMutation.reset();
    setForm({
      server: credential.server,
      username: credential.username,
      password: "",
      transport: credential.transport,
    });
    setEditor({ mode: "edit", credential });
  };

  const closeEditor = () => {
    if (!isSaving) setEditor(null);
  };

  const submitEditor = async (event: FormEvent) => {
    event.preventDefault();
    if (editor === null || isSaving) return;

    const input: CredentialInput = {
      server: form.server.trim(),
      username: form.username.trim(),
      password: form.password,
      transport: form.transport,
      // Ordinary edits preserve the server's current enabled value atomically;
      // only the explicit Switch and the add-default choose a new desired state.
      ...(editor.mode === "add" ? { enabled: true } : {}),
    };
    if (input.server === "" || input.username === "") return;

    try {
      if (editor.mode === "add") {
        await createMutation.mutateAsync(input);
      } else {
        await editorUpdateMutation.mutateAsync({
          id: editor.credential.id,
          input,
        });
      }
      setEditor(null);
    } catch {
      // The active mutation renders the server error inside this dialog.
    }
  };

  const toggleCredential = (credential: Credential, enabled: boolean) => {
    if (isCredentialBusy) return;
    toggleMutation.reset();
    toggleMutation.mutate({
      id: credential.id,
      input: {
        server: credential.server,
        username: credential.username,
        password: "",
        transport: credential.transport,
        enabled,
      },
    });
  };

  const openDeleteDialog = (credential: Credential) => {
    deleteMutation.reset();
    setDeleting(credential);
  };

  const confirmDelete = async () => {
    if (deleting === null || deleteMutation.isPending) return;
    try {
      await deleteMutation.mutateAsync(deleting.id);
      setDeleting(null);
    } catch {
      // Keep the confirmation open so its error remains visible.
    }
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

      <Box
        sx={{
          flexGrow: 1,
          minHeight: 0,
          overflowY: "auto",
          display: "flex",
          flexDirection: "column",
        }}
      >
        <Stack
          spacing={2}
          sx={{
            width: "100%",
            maxWidth: 820,
            mx: "auto",
            p: { xs: 2, sm: 3 },
            flexGrow: 1,
          }}
        >
          {credentials.length > 0 && (
            <Button
              variant="text"
              size="small"
              startIcon={<AddIcon />}
              onClick={openAddDialog}
              disabled={isSaving || isCredentialBusy}
              sx={{ alignSelf: "flex-start", flexShrink: 0 }}
            >
              {t("chat.sip.add")}
            </Button>
          )}
          {credentialsQuery.isError && (
            <Alert
              severity="error"
              action={
                <Button
                  color="inherit"
                  size="small"
                  onClick={() => void credentialsQuery.refetch()}
                >
                  {t("chat.sip.retry")}
                </Button>
              }
            >
              {errorMessage(credentialsQuery.error, t("chat.sip.loadFailed"))}
            </Alert>
          )}

          {toggleMutation.isError && (
            <Alert severity="error" onClose={() => toggleMutation.reset()}>
              {errorMessage(toggleMutation.error, t("chat.sip.mutationFailed"))}
            </Alert>
          )}

          {credentialsQuery.isPending ? (
            <Box sx={{ display: "flex", justifyContent: "center", py: 5 }}>
              <CircularProgress size={32} aria-label={t("chat.sip.loading")} />
            </Box>
          ) : credentialsQuery.data !== undefined &&
            credentials.length === 0 ? (
            <Box
              sx={{
                flexGrow: 1,
                display: "flex",
                alignItems: "center",
                justifyContent: "center",
              }}
            >
              <Stack direction="row" spacing={1} sx={{ alignItems: "center" }}>
                <Typography variant="body2">{t("chat.sip.empty")}</Typography>
                <Button
                  variant="text"
                  size="small"
                  startIcon={<AddIcon />}
                  onClick={openAddDialog}
                  disabled={isSaving || isCredentialBusy}
                >
                  {t("chat.sip.add")}
                </Button>
              </Stack>
            </Box>
          ) : (
            <Stack spacing={1.25}>
              {credentials.map((credential) => (
                <Paper key={credential.id} variant="outlined" sx={{ p: 2 }}>
                  <Stack spacing={1.5}>
                    <Box
                      sx={{
                        display: "flex",
                        alignItems: "flex-start",
                        justifyContent: "space-between",
                        gap: 1.5,
                      }}
                    >
                      <Typography
                        variant="body2"
                        sx={{
                          minWidth: 0,
                          fontWeight: 600,
                          overflowWrap: "anywhere",
                        }}
                      >
                        {credential.sip_uri}
                        <Box
                          component="span"
                          sx={{
                            ml: 1,
                            color: "text.secondary",
                            fontWeight: 400,
                          }}
                        >
                          · {TRANSPORT_LABELS[credential.transport]}
                        </Box>
                      </Typography>
                      <Chip
                        size="small"
                        color={STATE_COLORS[credential.registration.state]}
                        label={t(
                          `chat.sip.states.${credential.registration.state}`,
                        )}
                        variant="outlined"
                        sx={{ flexShrink: 0 }}
                      />
                    </Box>

                    {credential.registration.retry_at !== undefined && (
                      <Typography variant="caption" color="text.secondary">
                        {t("chat.sip.retryAt", {
                          time: formatTimestamp(
                            credential.registration.retry_at,
                            i18n.language,
                          ),
                        })}
                      </Typography>
                    )}

                    <Box
                      sx={{
                        display: "flex",
                        alignItems: "center",
                        flexWrap: "wrap",
                        gap: 0.5,
                      }}
                    >
                      <FormControlLabel
                        sx={{ mr: "auto" }}
                        control={
                          <Switch
                            size="small"
                            checked={credential.enabled}
                            disabled={isCredentialBusy}
                            onChange={(_, enabled) =>
                              toggleCredential(credential, enabled)
                            }
                            slotProps={{
                              input: {
                                "aria-label": t("chat.sip.enableCredential", {
                                  uri: credential.sip_uri,
                                }),
                              },
                            }}
                          />
                        }
                        label={t("chat.sip.enabled")}
                      />
                      <IconButton
                        size="small"
                        aria-label={t("chat.sip.editCredential", {
                          uri: credential.sip_uri,
                        })}
                        disabled={isCredentialBusy}
                        onClick={() => openEditDialog(credential)}
                      >
                        <EditIcon fontSize="small" />
                      </IconButton>
                      <IconButton
                        size="small"
                        color="error"
                        aria-label={t("chat.sip.deleteCredential", {
                          uri: credential.sip_uri,
                        })}
                        disabled={isCredentialBusy}
                        onClick={() => openDeleteDialog(credential)}
                      >
                        <DeleteOutlineIcon fontSize="small" />
                      </IconButton>
                    </Box>
                  </Stack>
                </Paper>
              ))}
            </Stack>
          )}
        </Stack>
      </Box>

      <Dialog
        open={editor !== null}
        onClose={closeEditor}
        fullWidth
        maxWidth="sm"
      >
        {editor !== null && (
          <Box component="form" onSubmit={submitEditor}>
            <DialogTitle>
              {editor.mode === "add"
                ? t("chat.sip.addTitle")
                : t("chat.sip.edit")}
            </DialogTitle>
            <DialogContent>
              <Stack spacing={2} sx={{ pt: 1 }}>
                {saveError !== null && saveError !== undefined && (
                  <Alert severity="error">
                    {errorMessage(saveError, t("chat.sip.mutationFailed"))}
                  </Alert>
                )}
                <Box
                  sx={{
                    display: "grid",
                    gridTemplateColumns: { xs: "1fr", sm: "1fr 1fr" },
                    gap: 2,
                  }}
                >
                  <TextField
                    required
                    autoFocus
                    label={t("chat.sip.server")}
                    placeholder="192.168.1.10:5060"
                    value={form.server}
                    onChange={(event) =>
                      setForm((current) => ({
                        ...current,
                        server: event.target.value,
                      }))
                    }
                    autoComplete="url"
                    size="small"
                    disabled={isSaving}
                  />
                  <TextField
                    select
                    label={t("chat.sip.transport")}
                    value={form.transport}
                    onChange={(event) =>
                      setForm((current) => ({
                        ...current,
                        transport: event.target.value as SipTransport,
                      }))
                    }
                    size="small"
                    disabled={isSaving}
                  >
                    {TRANSPORTS.map((option) => (
                      <MenuItem key={option} value={option}>
                        {TRANSPORT_LABELS[option]}
                      </MenuItem>
                    ))}
                  </TextField>
                  <TextField
                    required
                    label={t("chat.sip.username")}
                    value={form.username}
                    onChange={(event) =>
                      setForm((current) => ({
                        ...current,
                        username: event.target.value,
                      }))
                    }
                    autoComplete="username"
                    size="small"
                    disabled={isSaving}
                  />
                  <TextField
                    label={t("chat.sip.password")}
                    type="password"
                    value={form.password}
                    onChange={(event) =>
                      setForm((current) => ({
                        ...current,
                        password: event.target.value,
                      }))
                    }
                    autoComplete="new-password"
                    helperText={
                      editor.mode === "edit"
                        ? t("chat.sip.passwordHint")
                        : undefined
                    }
                    size="small"
                    disabled={isSaving}
                  />
                </Box>
              </Stack>
            </DialogContent>
            <DialogActions sx={{ px: 3, pb: 2 }}>
              <Button type="button" onClick={closeEditor} disabled={isSaving}>
                {t("chat.sip.cancel")}
              </Button>
              <Button
                type="submit"
                variant="contained"
                disabled={
                  isSaving ||
                  form.server.trim() === "" ||
                  form.username.trim() === ""
                }
                startIcon={
                  isSaving ? (
                    <CircularProgress size={18} color="inherit" />
                  ) : undefined
                }
              >
                {isSaving
                  ? editor.mode === "add"
                    ? t("chat.sip.adding")
                    : t("chat.sip.saving")
                  : editor.mode === "add"
                    ? t("chat.sip.confirm")
                    : t("chat.sip.save")}
              </Button>
            </DialogActions>
          </Box>
        )}
      </Dialog>

      <Dialog
        open={deleting !== null}
        onClose={() => {
          if (!deleteMutation.isPending) setDeleting(null);
        }}
        fullWidth
        maxWidth="xs"
      >
        <DialogTitle>{t("chat.sip.deleteTitle")}</DialogTitle>
        <DialogContent>
          <Stack spacing={2}>
            {deleteMutation.isError && (
              <Alert severity="error">
                {errorMessage(
                  deleteMutation.error,
                  t("chat.sip.mutationFailed"),
                )}
              </Alert>
            )}
            <Typography variant="body2">
              {t("chat.sip.deleteMessage", { uri: deleting?.sip_uri ?? "" })}
            </Typography>
          </Stack>
        </DialogContent>
        <DialogActions sx={{ px: 3, pb: 2 }}>
          <Button
            onClick={() => setDeleting(null)}
            disabled={deleteMutation.isPending}
          >
            {t("chat.sip.cancel")}
          </Button>
          <Button
            color="error"
            variant="contained"
            onClick={() => void confirmDelete()}
            disabled={deleteMutation.isPending || deleting === null}
            startIcon={
              deleteMutation.isPending ? (
                <CircularProgress size={18} color="inherit" />
              ) : (
                <DeleteOutlineIcon />
              )
            }
          >
            {deleteMutation.isPending
              ? t("chat.sip.deleting")
              : t("chat.sip.delete")}
          </Button>
        </DialogActions>
      </Dialog>
    </Box>
  );
}
