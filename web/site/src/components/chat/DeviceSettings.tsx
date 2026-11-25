"use client";

import {
  useCallback,
  useEffect,
  useRef,
  useState,
  type ReactNode,
} from "react";
import {
  Alert,
  Box,
  Button,
  IconButton,
  MenuItem,
  Paper,
  Slider,
  Stack,
  TextField,
  Typography,
  type SxProps,
  type Theme,
} from "@mui/material";
import ArrowBackIcon from "@mui/icons-material/ArrowBack";
import MicIcon from "@mui/icons-material/Mic";
import RefreshIcon from "@mui/icons-material/Refresh";
import VideocamIcon from "@mui/icons-material/Videocam";
import VolumeUpIcon from "@mui/icons-material/VolumeUp";
import { useTranslation } from "react-i18next";
import { useAudioGraph } from "@/api/audio/audiograph";
import { useCameraController } from "@/api/media/camera";
import { enumerateDevicesWithMicrophoneLabels } from "@/api/media/devices";
import { useCallVolumes } from "./useCallVolumes";

function errorMessage(error: unknown, fallback: string): string {
  return error instanceof Error && error.message !== ""
    ? error.message
    : fallback;
}

function DeviceSelect({
  label,
  value,
  devices,
  defaultLabel,
  unavailableLabel,
  onChange,
}: {
  label: string;
  value: string;
  devices: MediaDeviceInfo[];
  defaultLabel: string;
  unavailableLabel: string;
  onChange: (deviceId: string) => Promise<void>;
}) {
  const { t } = useTranslation();
  const available = devices.some((device) => device.deviceId === value);
  return (
    <TextField
      select
      fullWidth
      size="small"
      label={label}
      value={value}
      onChange={(event) => void onChange(event.target.value)}
      slotProps={{
        inputLabel: { shrink: true },
        select: { displayEmpty: true },
      }}
    >
      <MenuItem value="">{defaultLabel}</MenuItem>
      {value !== "" && !available && (
        <MenuItem value={value}>{unavailableLabel}</MenuItem>
      )}
      {devices.map((device, index) => (
        <MenuItem key={device.deviceId} value={device.deviceId}>
          {device.label ||
            t("chat.devices.unnamed", {
              kind: label,
              index: index + 1,
            })}
        </MenuItem>
      ))}
    </TextField>
  );
}

function CameraPreview({ stream }: { stream: MediaStream }) {
  const { t } = useTranslation();
  const ref = useRef<HTMLVideoElement>(null);
  useEffect(() => {
    const video = ref.current;
    if (video === null) return;
    video.srcObject = stream;
    void video.play().catch(() => {});
    return () => {
      video.srcObject = null;
    };
  }, [stream]);
  return (
    <Box
      component="video"
      ref={ref}
      autoPlay
      muted
      playsInline
      aria-label={t("chat.devices.cameraPreview")}
      sx={{
        display: "block",
        width: "100%",
        aspectRatio: "16 / 9",
        objectFit: "contain",
        bgcolor: "black",
        transform: "scaleX(-1)",
      }}
    />
  );
}

/** Displays RMS sampled from the actual post-gain AudioContext analyser. */
function LevelMeter({
  analyser,
  active,
  label,
  testId,
}: {
  analyser: AnalyserNode | null;
  active: boolean;
  label: string;
  testId: string;
}) {
  const [sample, setSample] = useState({ rms: 0, level: 0 });
  useEffect(() => {
    if (!active || analyser === null) return;
    const samples = new Uint8Array(analyser.fftSize);
    const read = () => {
      analyser.getByteTimeDomainData(samples);
      let squares = 0;
      for (const value of samples) {
        const normalized = (value - 128) / 128;
        squares += normalized * normalized;
      }
      const rms = Math.sqrt(squares / samples.length);
      setSample({ rms, level: Math.min(1, rms * 3) });
    };
    read();
    const interval = window.setInterval(read, 40);
    return () => window.clearInterval(interval);
  }, [active, analyser]);
  const visible = active ? sample : { rms: 0, level: 0 };
  return (
    <Box sx={{ display: "flex", alignItems: "center", gap: 1 }}>
      <Typography
        variant="caption"
        color="text.secondary"
        sx={{ minWidth: 64 }}
      >
        {label}
      </Typography>
      <Box
        role="meter"
        aria-label={label}
        aria-valuemin={0}
        aria-valuemax={100}
        aria-valuenow={Math.round(visible.level * 100)}
        data-testid={testId}
        data-rms={visible.rms}
        sx={{
          height: 8,
          flexGrow: 1,
          overflow: "hidden",
          borderRadius: 4,
          bgcolor: "action.selected",
        }}
      >
        <Box
          sx={{
            width: `${visible.level * 100}%`,
            height: "100%",
            bgcolor: "primary.main",
            transition: "width 40ms linear",
          }}
        />
      </Box>
    </Box>
  );
}

function VolumeSlider({
  icon,
  label,
  value,
  onChange,
}: {
  icon: ReactNode;
  label: string;
  value: number;
  onChange: (volume: number) => void;
}) {
  return (
    <Box>
      <Box sx={{ display: "flex", alignItems: "center", gap: 0.75, mb: 0.5 }}>
        {icon}
        <Typography variant="body2">{label}</Typography>
        <Typography
          variant="caption"
          color="text.secondary"
          sx={{ ml: "auto" }}
        >
          {Math.round(value * 100)}%
        </Typography>
      </Box>
      <Slider
        min={0}
        max={1}
        step={0.01}
        value={value}
        onChange={(_, next) => onChange(next as number)}
        aria-label={label}
      />
    </Box>
  );
}

export default function DeviceSettings({
  onBack,
  sx,
}: {
  onBack: () => void;
  sx?: SxProps<Theme>;
}) {
  const { t } = useTranslation();
  const audio = useAudioGraph();
  const camera = useCameraController();
  const { localVolume, remoteVolume, setLocalVolume, setRemoteVolume } =
    useCallVolumes(audio);
  const [devices, setDevices] = useState<MediaDeviceInfo[]>([]);
  const [microphoneId, setMicrophoneId] = useState(
    () => audio?.getMicrophoneDeviceId() ?? "",
  );
  const [cameraId, setCameraId] = useState(
    () => camera?.selectedDeviceId ?? "",
  );
  const [cameraPreview, setCameraPreview] = useState(false);
  const [cameraStream, setCameraStream] = useState<MediaStream | null>(null);
  const [microphonePreview, setMicrophonePreview] = useState(false);
  const [microphoneAnalyser, setMicrophoneAnalyser] =
    useState<AnalyserNode | null>(null);
  const [speakerTesting, setSpeakerTesting] = useState(false);
  const [cameraPending, setCameraPending] = useState(false);
  const [microphonePending, setMicrophonePending] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const mounted = useRef(true);
  const cameraStarting = useRef(false);
  const microphoneStarting = useRef(false);
  const cameraHeld = useRef(false);
  const microphoneHeld = useRef(false);
  const deviceEnumeration = useRef<Promise<MediaDeviceInfo[]> | null>(null);

  const refreshDevices = useCallback(async () => {
    try {
      if (navigator.mediaDevices?.enumerateDevices === undefined) {
        throw new Error(t("chat.devices.unsupported"));
      }
      let enumeration = deviceEnumeration.current;
      if (enumeration === null) {
        enumeration = enumerateDevicesWithMicrophoneLabels(
          navigator.mediaDevices,
        );
        deviceEnumeration.current = enumeration;
      }
      try {
        setDevices(await enumeration);
      } finally {
        if (deviceEnumeration.current === enumeration) {
          deviceEnumeration.current = null;
        }
      }
    } catch (reason) {
      setError(errorMessage(reason, t("chat.devices.enumerateFailed")));
    }
  }, [t]);

  useEffect(() => {
    const changed = () => void refreshDevices();
    const initial = window.setTimeout(changed, 0);
    navigator.mediaDevices?.addEventListener("devicechange", changed);
    return () => {
      window.clearTimeout(initial);
      navigator.mediaDevices?.removeEventListener("devicechange", changed);
    };
  }, [refreshDevices]);

  useEffect(() => {
    if (camera === null) return;
    return camera.subscribe(setCameraStream);
  }, [camera]);

  const stopCamera = useCallback(() => {
    if (cameraHeld.current) {
      cameraHeld.current = false;
      camera?.release();
    }
    setCameraPreview(false);
  }, [camera]);

  const stopMicrophone = useCallback(() => {
    if (microphoneHeld.current) {
      microphoneHeld.current = false;
      audio?.releaseLocalInput();
    }
    setMicrophonePreview(false);
    setMicrophoneAnalyser(null);
  }, [audio]);

  useEffect(() => {
    mounted.current = true;
    return () => {
      mounted.current = false;
      if (cameraHeld.current) camera?.release();
      if (microphoneHeld.current) audio?.releaseLocalInput();
      audio?.stopSpeakerTest();
    };
  }, [audio, camera]);

  const startCamera = useCallback(async () => {
    if (camera === null || cameraHeld.current || cameraStarting.current) {
      return;
    }
    cameraStarting.current = true;
    setCameraPending(true);
    setError(null);
    try {
      await camera.acquire();
      if (!mounted.current) {
        camera.release();
        return;
      }
      cameraHeld.current = true;
      setCameraPreview(true);
      await refreshDevices();
    } catch (reason) {
      if (mounted.current) {
        setError(errorMessage(reason, t("chat.devices.cameraFailed")));
      }
    } finally {
      cameraStarting.current = false;
      if (mounted.current) setCameraPending(false);
    }
  }, [camera, refreshDevices, t]);

  useEffect(() => {
    const start = window.setTimeout(() => void startCamera(), 0);
    return () => window.clearTimeout(start);
  }, [startCamera]);

  const startMicrophone = async () => {
    if (
      audio === null ||
      microphoneHeld.current ||
      microphoneStarting.current
    ) {
      return;
    }
    microphoneStarting.current = true;
    setMicrophonePending(true);
    setError(null);
    try {
      audio.resume();
      await audio.acquireLocalInput();
      if (!mounted.current) {
        audio.releaseLocalInput();
        return;
      }
      microphoneHeld.current = true;
      setMicrophoneAnalyser(audio.localAnalyser);
      setMicrophonePreview(true);
      await refreshDevices();
    } catch (reason) {
      if (mounted.current) {
        setError(errorMessage(reason, t("chat.devices.microphoneFailed")));
      }
    } finally {
      microphoneStarting.current = false;
      if (mounted.current) setMicrophonePending(false);
    }
  };

  const selectCamera = async (deviceId: string) => {
    setCameraId(deviceId);
    setError(null);
    try {
      await camera?.setDevice(deviceId);
    } catch (reason) {
      setError(errorMessage(reason, t("chat.devices.cameraFailed")));
    }
  };

  const selectMicrophone = async (deviceId: string) => {
    setMicrophoneId(deviceId);
    setError(null);
    try {
      await audio?.setMicrophoneDevice(deviceId);
    } catch (reason) {
      setError(errorMessage(reason, t("chat.devices.microphoneFailed")));
    }
  };

  const toggleSpeakerTest = () => {
    if (speakerTesting) {
      audio?.stopSpeakerTest();
      setSpeakerTesting(false);
      return;
    }
    audio?.startSpeakerTest();
    setSpeakerTesting(true);
  };

  const microphones = devices.filter((device) => device.kind === "audioinput");
  const cameras = devices.filter((device) => device.kind === "videoinput");

  return (
    <Box
      component="section"
      id="device-settings"
      aria-label={t("chat.devices.title")}
      sx={[
        {
          flexGrow: 1,
          minWidth: 0,
          minHeight: 0,
          display: "flex",
          flexDirection: "column",
          bgcolor: "background.default",
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
          onClick={() => {
            stopCamera();
            stopMicrophone();
            audio?.stopSpeakerTest();
            setSpeakerTesting(false);
            onBack();
          }}
          aria-label={t("chat.devices.back")}
          sx={{ display: { xs: "inline-flex", sm: "none" } }}
        >
          <ArrowBackIcon />
        </IconButton>
        <Typography variant="subtitle1" sx={{ fontWeight: 600 }}>
          {t("chat.devices.title")}
        </Typography>
        <IconButton
          onClick={() => void refreshDevices()}
          aria-label={t("chat.devices.refresh")}
          sx={{ ml: "auto" }}
        >
          <RefreshIcon />
        </IconButton>
      </Box>
      <Box sx={{ flexGrow: 1, minHeight: 0, overflowY: "auto" }}>
        <Stack
          spacing={2}
          sx={{ maxWidth: 760, mx: "auto", p: { xs: 2, sm: 3 } }}
        >
          <Typography variant="body2" color="text.secondary">
            {t("chat.devices.description")}
          </Typography>
          {error !== null && (
            <Alert severity="error" onClose={() => setError(null)}>
              {error}
            </Alert>
          )}

          <Paper variant="outlined" sx={{ p: 2 }}>
            <Stack spacing={1.5}>
              <Box sx={{ display: "flex", alignItems: "center", gap: 1 }}>
                <VideocamIcon color="action" />
                <Typography variant="subtitle2">
                  {t("chat.devices.camera")}
                </Typography>
              </Box>
              <DeviceSelect
                label={t("chat.devices.camera")}
                value={cameraId}
                devices={cameras}
                defaultLabel={t("chat.devices.systemDefault")}
                unavailableLabel={t("chat.devices.unavailable")}
                onChange={selectCamera}
              />
              {cameraPreview && cameraStream !== null ? (
                <CameraPreview stream={cameraStream} />
              ) : cameraPending ? (
                <Box
                  sx={{
                    aspectRatio: "16 / 9",
                    display: "flex",
                    alignItems: "center",
                    justifyContent: "center",
                    bgcolor: "black",
                    color: "common.white",
                  }}
                >
                  <Typography variant="body2" role="status">
                    {t("chat.devices.opening")}
                  </Typography>
                </Box>
              ) : null}
            </Stack>
          </Paper>

          <Paper variant="outlined" sx={{ p: 2 }}>
            <Stack spacing={1.5}>
              <Box sx={{ display: "flex", alignItems: "center", gap: 1 }}>
                <MicIcon color="action" />
                <Typography variant="subtitle2">
                  {t("chat.devices.microphone")}
                </Typography>
              </Box>
              <DeviceSelect
                label={t("chat.devices.microphone")}
                value={microphoneId}
                devices={microphones}
                defaultLabel={t("chat.devices.systemDefault")}
                unavailableLabel={t("chat.devices.unavailable")}
                onChange={selectMicrophone}
              />
              <LevelMeter
                analyser={microphoneAnalyser}
                active={microphonePreview}
                label={t("chat.devices.inputLevel")}
                testId="microphone-level"
              />
              <Stack
                direction="row"
                spacing={1.5}
                sx={{ alignItems: "center" }}
              >
                <Button
                  variant="outlined"
                  size="small"
                  onClick={() =>
                    microphonePreview
                      ? stopMicrophone()
                      : void startMicrophone()
                  }
                  disabled={microphonePending || audio === null}
                >
                  {microphonePending
                    ? t("chat.devices.opening")
                    : microphonePreview
                      ? t("chat.devices.stop")
                      : t("chat.devices.test")}
                </Button>
                {microphonePreview && (
                  <Typography
                    variant="caption"
                    color="text.secondary"
                    role="status"
                  >
                    {t("chat.devices.testing")}
                  </Typography>
                )}
              </Stack>
              <Typography variant="caption" color="text.secondary">
                {t("chat.devices.microphoneHint")}
              </Typography>
              <VolumeSlider
                icon={<MicIcon fontSize="small" color="action" />}
                label={t("chat.devices.microphoneVolume")}
                value={localVolume}
                onChange={setLocalVolume}
              />
            </Stack>
          </Paper>

          <Paper variant="outlined" sx={{ p: 2 }}>
            <Stack spacing={1.5}>
              <Box sx={{ display: "flex", alignItems: "center", gap: 1 }}>
                <VolumeUpIcon color="action" />
                <Typography variant="subtitle2">
                  {t("chat.devices.speaker")}
                </Typography>
              </Box>
              <VolumeSlider
                icon={<VolumeUpIcon fontSize="small" color="action" />}
                label={t("chat.devices.speakerVolume")}
                value={remoteVolume}
                onChange={setRemoteVolume}
              />
              <LevelMeter
                analyser={audio?.speakerAnalyser ?? null}
                active={speakerTesting}
                label={t("chat.devices.outputLevel")}
                testId="speaker-level"
              />
              <Stack
                direction="row"
                spacing={1.5}
                sx={{ alignItems: "center" }}
              >
                <Button
                  variant="outlined"
                  size="small"
                  onClick={toggleSpeakerTest}
                  disabled={audio === null}
                >
                  {speakerTesting
                    ? t("chat.devices.stop")
                    : t("chat.devices.test")}
                </Button>
                {speakerTesting && (
                  <Typography
                    variant="caption"
                    color="text.secondary"
                    role="status"
                  >
                    {t("chat.devices.testing")}
                  </Typography>
                )}
              </Stack>
            </Stack>
          </Paper>
          <Typography variant="caption" color="text.secondary">
            {t("chat.devices.volumeHint")}
          </Typography>
        </Stack>
      </Box>
    </Box>
  );
}
