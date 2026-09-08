"use client";

import { useQuery } from "@tanstack/react-query";

// Wire types mirroring the JSON served by the Go backend's pkg/api/dyn:
// GET /api/dyn/posts, GET /api/dyn/posts/{id}, GET /api/dyn/projects,
// GET /api/dyn/authorcontacts, and GET /api/dyn/entertain, sourced from the
// <dynBlogData/> section of the server configuration document and re-read on
// every request server-side.
export type DynPost = {
  id: string;
  href: string;
  title: string;
  description: string;
  // ISO date string; absent when the post was never edited after
  // publication.
  lastModified?: string;
  creation: string;
  tags: string[];
};

export type DynProject = {
  id: string;
  name: string;
  description: string;
  url: string;
  tech: string[];
};

export type DynAuthorContact = {
  id: string;
  kind: string;
  label: string;
  url: string;
};

// One media card of the entertain page's Live, Video, or Music shelf.
// thumbnail is the card's cover image (a data URL, an absolute URL, or a
// site-relative URL; absent renders a placeholder tile); href is where
// clicking the card navigates.
export type DynMedia = {
  id: string;
  name: string;
  displayName: string;
  description: string;
  thumbnail?: string;
  href: string;
};

// The entertain page's three media shelves.
export type DynEntertain = {
  live: DynMedia[];
  videos: DynMedia[];
  music: DynMedia[];
};

// One entry of the top bar's navigation drawer. name is the page's route
// slug ("/" for "home", otherwise "/<name>"); displayName is the caption's
// fallback, overridden by i18nDisplayNames[i18n.language] when the active
// language has an entry; description, when present, is the entry's
// secondary line; iconClassName picks the entry's icon from NavDrawer's
// icon map ("home", "musicNote", …).
export type DynMenuEntry = {
  id: string;
  name: string;
  displayName: string;
  i18nDisplayNames?: Record<string, string>;
  description?: string;
  iconClassName: string;
};

async function fetchJson<T>(path: string): Promise<T> {
  const res = await fetch(path);
  if (!res.ok) {
    throw new Error(`GET ${path} failed: ${res.status}`);
  }
  return res.json();
}

// useDynPosts fetches GET /api/dyn/posts: the blog's post metadata list.
export function useDynPosts() {
  return useQuery({
    queryKey: ["dyn", "posts"],
    queryFn: () => fetchJson<DynPost[]>("/api/dyn/posts"),
  });
}

// useDynPost fetches GET /api/dyn/posts/{id}: a single post's metadata —
// the bandwidth-efficient query for post pages, which need one entry, not
// the whole list. A 404 (no entry for the id) resolves to null rather than
// throwing, so a post without a metadata entry renders its body alone,
// without an error alert; other failures still throw.
export function useDynPost(postId: string) {
  const path = `/api/dyn/posts/${postId}`;
  return useQuery({
    queryKey: ["dyn", "posts", postId],
    queryFn: async (): Promise<DynPost | null> => {
      const res = await fetch(path);
      if (res.status === 404) {
        return null;
      }
      if (!res.ok) {
        throw new Error(`GET ${path} failed: ${res.status}`);
      }
      return res.json();
    },
  });
}

// useDynProjects fetches GET /api/dyn/projects: the site's project list.
export function useDynProjects() {
  return useQuery({
    queryKey: ["dyn", "projects"],
    queryFn: () => fetchJson<DynProject[]>("/api/dyn/projects"),
  });
}

// useDynAuthorContacts fetches GET /api/dyn/authorcontacts: the site
// author's contact entries.
export function useDynAuthorContacts() {
  return useQuery({
    queryKey: ["dyn", "authorcontacts"],
    queryFn: () => fetchJson<DynAuthorContact[]>("/api/dyn/authorcontacts"),
  });
}

// useDynEntertain fetches GET /api/dyn/entertain: the entertain page's Live,
// Video, and Music media shelves in one round trip.
export function useDynEntertain() {
  return useQuery({
    queryKey: ["dyn", "entertain"],
    queryFn: () => fetchJson<DynEntertain>("/api/dyn/entertain"),
  });
}

// useDynMenu fetches GET /api/dyn/menu: the top bar's navigation drawer
// entries, in display order.
export function useDynMenu() {
  return useQuery({
    queryKey: ["dyn", "menu"],
    queryFn: () => fetchJson<DynMenuEntry[]>("/api/dyn/menu"),
  });
}
