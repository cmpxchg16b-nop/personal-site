// DialerHandle is one call's DTMF sender as the dial pad sees it.
export interface DialerHandle {
  /**
   * Queues one digit (0-9, *, #) as an RFC 4733 telephone-event on the
   * call's audio sender. A no-op while the sender cannot insert tones
   * (not connected yet, held): canInsertDTMF is the live gate, checked
   * at press time, not render time.
   */
  insert: (digit: string) => void;
}

/**
 * Returns a dialer when the resolved sender has DTMF support and negotiated
 * telephone-event. The sender is resolved again for every key press so a
 * replaced or cleaned-up sender never receives a stale tone.
 */
export function dialerForSender(
  resolveSender: () => RTCRtpSender | null,
): DialerHandle | null {
  const sender = resolveSender();
  if (sender?.dtmf == null) return null;
  const negotiated = sender
    .getParameters()
    .codecs.some((c) => c.mimeType.toLowerCase() === "audio/telephone-event");
  if (!negotiated) return null;
  return {
    insert: (digit: string) => {
      const live = resolveSender();
      if (live?.dtmf?.canInsertDTMF !== true) return;
      live.dtmf.insertDTMF(digit);
    },
  };
}
