"use client";

// AudioSpectrum draws the frequency spectrum of an AnalyserNode as a
// smooth, filled area chart on a canvas, repainting on the animation
// frame while mounted. A null analyser draws the flat baseline — the
// chart stays rendered (quietly) while its audio path is being set up.

import { useEffect, useRef } from "react";
import { useTheme } from "@mui/material";

// SPECTRUM_POINTS is the number of points the FFT bins are downsampled
// into for the chart; SPECTRUM_BIN_FRACTION is the share of the FFT's
// bins used — voice energy concentrates in the lower bins, so the top
// quarter (near-Nyquist hiss) is left out.
const SPECTRUM_POINTS = 48;
const SPECTRUM_BIN_FRACTION = 0.75;

type AudioSpectrumProps = {
  // The FFT tap to draw (AudioGraph's local/remote analysers), or null
  // for the flat baseline.
  analyser: AnalyserNode | null;
  // Chart height in CSS pixels; the canvas stretches to its box's width.
  height?: number;
  // The curve's color; defaults to the theme's primary.
  color?: string;
};

export function AudioSpectrum({
  analyser,
  height = 48,
  color,
}: AudioSpectrumProps) {
  const theme = useTheme();
  const stroke = color ?? theme.palette.primary.main;
  const canvasRef = useRef<HTMLCanvasElement>(null);

  useEffect(() => {
    const canvas = canvasRef.current;
    const ctx = canvas?.getContext("2d");
    if (canvas == null || ctx == null) return;

    let raf = 0;
    let bins = new Uint8Array(0);
    const points = new Array<number>(SPECTRUM_POINTS).fill(0);

    const draw = () => {
      raf = requestAnimationFrame(draw);
      // Track the box's size at the device's pixel ratio.
      const dpr = window.devicePixelRatio || 1;
      const w = canvas.clientWidth;
      const h = canvas.clientHeight;
      if (w === 0 || h === 0) return;
      if (canvas.width !== w * dpr || canvas.height !== h * dpr) {
        canvas.width = w * dpr;
        canvas.height = h * dpr;
      }
      ctx.setTransform(dpr, 0, 0, dpr, 0, 0);
      ctx.clearRect(0, 0, w, h);

      if (analyser !== null) {
        if (bins.length !== analyser.frequencyBinCount) {
          bins = new Uint8Array(analyser.frequencyBinCount);
        }
        analyser.getByteFrequencyData(bins);
        const usable = Math.max(
          1,
          Math.floor(bins.length * SPECTRUM_BIN_FRACTION),
        );
        const bucket = usable / SPECTRUM_POINTS;
        for (let i = 0; i < SPECTRUM_POINTS; i++) {
          // Average the bucket's bins into one point, 0..1.
          let sum = 0;
          const from = Math.floor(i * bucket);
          const to = Math.max(from + 1, Math.floor((i + 1) * bucket));
          for (let j = from; j < to; j++) sum += bins[j];
          points[i] = sum / ((to - from) * 255);
        }
      } else {
        points.fill(0);
      }

      // The smooth area chart: quadratic curves through the points'
      // midpoints, filled to the bottom.
      const stepX = w / (SPECTRUM_POINTS - 1);
      // Keep a whisper of floor so silence reads as a line, not void.
      const y = (i: number) => h - (points[i] * 0.92 + 0.04) * h;
      ctx.beginPath();
      ctx.moveTo(0, y(0));
      for (let i = 1; i < SPECTRUM_POINTS - 1; i++) {
        ctx.quadraticCurveTo(
          i * stepX,
          y(i),
          (i + 0.5) * stepX,
          (y(i) + y(i + 1)) / 2,
        );
      }
      ctx.lineTo(w, y(SPECTRUM_POINTS - 1));
      ctx.strokeStyle = stroke;
      ctx.lineWidth = 1.5;
      ctx.stroke();
      ctx.lineTo(w, h);
      ctx.lineTo(0, h);
      ctx.closePath();
      ctx.globalAlpha = 0.22;
      ctx.fillStyle = stroke;
      ctx.fill();
      ctx.globalAlpha = 1;
    };
    draw();
    return () => cancelAnimationFrame(raf);
  }, [analyser, stroke]);

  return (
    <canvas
      ref={canvasRef}
      style={{ display: "block", width: "100%", height }}
      aria-hidden
    />
  );
}
