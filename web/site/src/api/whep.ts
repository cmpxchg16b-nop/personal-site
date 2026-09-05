"use client";

/**
 * A minimal WHEP (WebRTC HTTP Egress Protocol) client — the read side of a
 * live stream.
 *
 * WHEP is the egress counterpart of WHIP: the browser offers recvonly
 * audio/video transceivers, POSTs the SDP offer to the endpoint, and the
 * server answers 201 Created with the SDP answer plus a Location header
 * identifying the session resource. Local ICE candidates trickle to that
 * resource as PATCH requests carrying `application/trickle-ice-sdpfrag`
 * fragments; teardown is a DELETE of the same resource
 * (draft-ietf-wish-whep).
 *
 * The wire details follow MediaMTX's own web reader (this site's stream
 * server): the trickle fragment repeats the offer's ICE credentials and
 * m= lines, the PATCH carries `If-Match: *`, the endpoint is first probed
 * with OPTIONS to harvest any ICE servers advertised in Link headers, and
 * opus is offered with the stereo fmtp parameters so stereo streams stay
 * stereo in the browser.
 *
 * The client is deliberately self-contained: the site's chat subsystem
 * (src/api/ss, src/api/audio) negotiates peer-to-peer calls over the site's
 * own signalling WebSocket, while WHEP only ever talks to the configured
 * stream endpoint — no shared code, no shared ICE-server configuration.
 */

export type WhepClientCallbacks = {
  /**
   * Called once per received track with the MediaStream carrying the
   * server's tracks — the same instance every time, so re-assigning it to
   * a media element is harmless.
   */
  onStream?: (stream: MediaStream) => void;

  /**
   * Called on every RTCPeerConnection state change, including the final
   * "closed" fired by close().
   */
  onStateChange?: (state: RTCPeerConnectionState) => void;
};

/** The offer bits the trickle fragments have to repeat verbatim. */
type OfferData = {
  iceUfrag: string;
  icePwd: string;
  /** The local offer's m= lines, without the "m=" prefix, in order. */
  medias: string[];
};

export class WhepClient {
  private readonly stream = new MediaStream();
  private pc: RTCPeerConnection | null = null;
  private offerData: OfferData | null = null;
  private sessionUrl: string | null = null;
  private queuedCandidates: RTCIceCandidate[] = [];
  private closed = false;
  private readonly url: string;

  /**
   * @param url the WHEP endpoint, absolute
   * ("http://host:8889/mystream/whep") or relative to the document's
   * origin ("/mystream/whep") — the relative form suits deployments that
   * proxy the stream server behind the site's own origin.
   */
  constructor(
    url: string,
    private readonly callbacks: WhepClientCallbacks = {},
  ) {
    // Resolve once, up front: the POST response's Location header (the
    // session resource the trickle PATCHes and the DELETE go to) may
    // itself be relative, and resolving it needs an absolute base. An
    // unresolvable endpoint is kept verbatim: the handshake fetch then
    // rejects with it and the caller surfaces the offline state, instead
    // of the constructor throwing into the render tree.
    try {
      this.url = new URL(url, window.location.href).toString();
    } catch {
      this.url = url;
    }
  }

  /**
   * Runs the handshake: OPTIONS (ICE servers) → recvonly offer → POST →
   * answer. Rejects when the endpoint is unreachable or answers anything
   * but 201 (MediaMTX answers 404 while nobody is publishing the stream);
   * close() the client afterwards either way.
   */
  async start(): Promise<void> {
    const iceServers = await this.fetchIceServers();
    if (this.closed) return;

    const pc = new RTCPeerConnection({ iceServers });
    this.pc = pc;
    // Egress only: recvonly transceivers, no local media is ever attached.
    pc.addTransceiver("video", { direction: "recvonly" });
    pc.addTransceiver("audio", { direction: "recvonly" });
    pc.ontrack = (evt) => {
      this.stream.addTrack(evt.track);
      this.callbacks.onStream?.(this.stream);
    };
    pc.onicecandidate = (evt) => {
      if (evt.candidate !== null) this.sendCandidates([evt.candidate]);
    };
    pc.onconnectionstatechange = () => {
      this.callbacks.onStateChange?.(pc.connectionState);
    };

    const offer = await pc.createOffer();
    offer.sdp = WhepClient.enableStereoOpus(offer.sdp ?? "");
    this.offerData = WhepClient.parseOffer(offer.sdp);
    await pc.setLocalDescription(offer);
    if (this.closed) return;

    const res = await fetch(this.url, {
      method: "POST",
      headers: { "Content-Type": "application/sdp" },
      body: offer.sdp,
    });
    if (!res.ok) {
      throw new Error(await WhepClient.describeError(this.url, res));
    }
    // Location identifies the session resource for the trickle PATCHes and
    // the final DELETE; it may be relative, so resolve it against the
    // endpoint (absolute by construction). If a CORS policy hides the
    // header, fall back to the endpoint itself — MediaMTX pins the session
    // by the fragment's ICE credentials, so its resource URL is the
    // endpoint URL anyway.
    const location = res.headers.get("Location");
    this.sessionUrl =
      location !== null ? new URL(location, this.url).toString() : this.url;
    const answer = await res.text();
    if (this.closed) {
      // Torn down while the POST was in flight: release the session that
      // was just created server-side.
      void this.deleteSession();
      return;
    }
    await pc.setRemoteDescription({ type: "answer", sdp: answer });
    if (this.queuedCandidates.length !== 0) {
      const queued = this.queuedCandidates;
      this.queuedCandidates = [];
      this.sendCandidates(queued);
    }
  }

  /** Closes the peer connection and DELETEs the session resource. */
  close(): void {
    if (this.closed) return;
    this.closed = true;
    if (this.pc !== null) {
      this.pc.close();
      this.pc = null;
    }
    void this.deleteSession();
  }

  /**
   * Asks the endpoint (OPTIONS) for the ICE servers it advertises in RFC
   * 8288 Link headers. An endpoint that answers none — or no OPTIONS at
   * all — is fine: a direct connection covers the same-host / LAN case.
   */
  private async fetchIceServers(): Promise<RTCIceServer[]> {
    try {
      const res = await fetch(this.url, { method: "OPTIONS" });
      return WhepClient.parseIceServerLinks(res.headers.get("Link"));
    } catch {
      return [];
    }
  }

  private static parseIceServerLinks(header: string | null): RTCIceServer[] {
    if (header === null) return [];
    const servers: RTCIceServer[] = [];
    for (const link of header.split(", ")) {
      const m =
        /^<(.+?)>; rel="ice-server"(?:; username="(.*?)"; credential="(.*?)"; credential-type="password")?$/i.exec(
          link,
        );
      if (m === null) continue;
      const server: RTCIceServer = { urls: [m[1]] };
      if (m[2] !== undefined) {
        // The username/credential values are JSON-quoted strings.
        server.username = WhepClient.unquote(m[2]);
        server.credential = WhepClient.unquote(m[3]);
      }
      servers.push(server);
    }
    return servers;
  }

  private static unquote(v: string): string {
    return JSON.parse(`"${v}"`) as string;
  }

  /** Queues candidates until the session resource is known, then PATCHes. */
  private sendCandidates(candidates: RTCIceCandidate[]): void {
    if (this.sessionUrl === null || this.offerData === null) {
      this.queuedCandidates.push(...candidates);
      return;
    }
    fetch(this.sessionUrl, {
      method: "PATCH",
      headers: {
        "Content-Type": "application/trickle-ice-sdpfrag",
        "If-Match": "*",
      },
      body: WhepClient.generateSdpFragment(this.offerData, candidates),
    }).catch((err) => {
      // Not fatal: the server also learns this peer's address from the ICE
      // connectivity checks (peer-reflexive candidates), and a truly dead
      // session fails the peer connection on its own.
      console.debug("whep: trickle PATCH failed", err);
    });
  }

  /**
   * Builds the `application/trickle-ice-sdpfrag` body: the offer's ICE
   * credentials at session level, then — per media the candidates belong
   * to — the offer's m= line, the mid, and the `a=candidate:` lines.
   */
  private static generateSdpFragment(
    od: OfferData,
    candidates: RTCIceCandidate[],
  ): string {
    const byMedia = new Map<number, RTCIceCandidate[]>();
    for (const candidate of candidates) {
      const idx = candidate.sdpMLineIndex ?? 0;
      const group = byMedia.get(idx);
      if (group === undefined) byMedia.set(idx, [candidate]);
      else group.push(candidate);
    }
    let frag = `a=ice-ufrag:${od.iceUfrag}\r\na=ice-pwd:${od.icePwd}\r\n`;
    od.medias.forEach((media, mid) => {
      const group = byMedia.get(mid);
      if (group === undefined) return;
      frag += `m=${media}\r\na=mid:${mid}\r\n`;
      for (const candidate of group) frag += `a=${candidate.candidate}\r\n`;
    });
    return frag;
  }

  /** Extracts the ICE credentials and m= lines the fragments must repeat. */
  private static parseOffer(sdp: string): OfferData {
    const ret: OfferData = { iceUfrag: "", icePwd: "", medias: [] };
    for (const line of sdp.split("\r\n")) {
      if (line.startsWith("m=")) {
        ret.medias.push(line.slice("m=".length));
      } else if (ret.iceUfrag === "" && line.startsWith("a=ice-ufrag:")) {
        ret.iceUfrag = line.slice("a=ice-ufrag:".length);
      } else if (ret.icePwd === "" && line.startsWith("a=ice-pwd:")) {
        ret.icePwd = line.slice("a=ice-pwd:".length);
      }
    }
    return ret;
  }

  /**
   * Appends the stereo fmtp parameters to the offer's opus format. Chrome
   * offers opus without them and would play a stereo stream downmixed to
   * mono; MediaMTX's own reader applies the same edit.
   */
  private static enableStereoOpus(sdp: string): string {
    let payloadType: string | null = null;
    for (const line of sdp.split("\r\n")) {
      const m = /^a=rtpmap:(\d+) opus\/48000\/2$/i.exec(line);
      if (m !== null) {
        payloadType = m[1];
        break;
      }
    }
    if (payloadType === null) return sdp;
    const pt = payloadType;
    return sdp
      .split("\r\n")
      .map((line) => {
        if (!line.startsWith(`a=fmtp:${pt} `)) return line;
        if (!line.includes("stereo")) line += ";stereo=1";
        if (!line.includes("sprop-stereo")) line += ";sprop-stereo=1";
        return line;
      })
      .join("\r\n");
  }

  private static async describeError(
    url: string,
    res: Response,
  ): Promise<string> {
    // MediaMTX answers 400 with a JSON body carrying the reason; other
    // failures just get the status code.
    let detail = "";
    if (res.status === 400) {
      try {
        const body = (await res.json()) as { error?: string };
        if (body.error !== undefined) detail = `: ${body.error}`;
      } catch {
        // Not a JSON body — fall back to the bare status.
      }
    }
    return `WHEP POST ${url} failed: ${res.status}${detail}`;
  }

  private async deleteSession(): Promise<void> {
    const url = this.sessionUrl;
    this.sessionUrl = null;
    if (url === null) return;
    try {
      await fetch(url, { method: "DELETE" });
    } catch {
      // Best-effort teardown: the server also ages out sessions whose ICE
      // connection goes away, so a lost DELETE leaks nothing for long.
    }
  }
}
