/**
 * Browsers hide device labels until the corresponding media permission has
 * been granted. Briefly capture audio when necessary so microphone names can
 * be shown before the user starts a test or call.
 */
export async function enumerateDevicesWithMicrophoneLabels(
  mediaDevices: MediaDevices,
): Promise<MediaDeviceInfo[]> {
  const devices = await mediaDevices.enumerateDevices();
  if (
    !devices.some(
      (device) => device.kind === "audioinput" && device.label === "",
    )
  ) {
    return devices;
  }

  let stream: MediaStream;
  try {
    stream = await mediaDevices.getUserMedia({ audio: true });
  } catch {
    return devices;
  }

  try {
    return await mediaDevices.enumerateDevices();
  } finally {
    for (const track of stream.getTracks()) track.stop();
  }
}
