"use client";

import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";

// Wire types mirroring the JSON served by the Go backend's pkg/api/comments:
// GET and PUT /api/comments/channel/{channelId}. Times are seconds since the
// Unix epoch (UTC); last_comment_id is the id of the channel's last comment
// when this one was made (empty for the channel's first comment).
export type Comment = {
  id: string;
  channel_id: string;
  user_id: string;
  serial_number: number;
  last_comment_id: string;
  content: string;
  mime_type: string;
  creation_time: number;
  last_modified: number;
};

// ApiError is a non-2xx response from the comments API. It carries the HTTP
// status so callers can special-case it — notably 409, which means the
// channel moved past the last_comment_id the comment was submitted with.
export class ApiError extends Error {
  constructor(
    public readonly status: number,
    message: string,
  ) {
    super(message);
    this.name = "ApiError";
  }
}

// commentsKey is the react-query cache key of a channel's comment list.
function commentsKey(channelId: string) {
  return ["comments", "channel", channelId] as const;
}

// channelPath builds the API path of a channel, URL-encoding the id: a
// channel id can be a pathname ("/posts/…") — CommentZone defaults to
// exactly that — which must not leak extra segments into the request path.
function channelPath(channelId: string): string {
  return `/api/comments/channel/${encodeURIComponent(channelId)}`;
}

// useComments fetches GET /api/comments/channel/{channelId}: the channel's
// comments, oldest first. enabled gates the fetch (default true): pass false
// when no channel id is available.
export function useComments(channelId: string, enabled = true) {
  return useQuery({
    queryKey: commentsKey(channelId),
    queryFn: async (): Promise<Comment[]> => {
      const path = channelPath(channelId);
      const res = await fetch(path);
      if (!res.ok) {
        throw new ApiError(res.status, `GET ${path} failed: ${res.status}`);
      }
      const body = (await res.json()) as { comments: Comment[] };
      return body.comments;
    },
    enabled,
  });
}

// PostCommentInput is one comment submission. lastCommentId is the id of the
// channel's last comment as the submitter knew it ("" when the channel
// looked empty); the server rejects the append with 409 when the channel has
// moved past it.
export type PostCommentInput = {
  userId: string;
  content: string;
  lastCommentId: string;
};

// usePostComment appends a comment to a channel (PUT
// /api/comments/channel/{channelId}). There is no authentication yet: userId
// is trusted as given. A successful append invalidates the channel's comment
// list; a 409 conflict invalidates it too, so the retry the server expects
// starts from the fresh list.
export function usePostComment(channelId: string) {
  const queryClient = useQueryClient();
  return useMutation({
    mutationFn: async (input: PostCommentInput): Promise<Comment> => {
      const path = channelPath(channelId);
      const res = await fetch(path, {
        method: "PUT",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify({
          user_id: input.userId,
          content: input.content,
          last_comment_id: input.lastCommentId,
        }),
      });
      if (!res.ok) {
        throw new ApiError(res.status, `PUT ${path} failed: ${res.status}`);
      }
      return res.json();
    },
    onSuccess: () =>
      queryClient.invalidateQueries({ queryKey: commentsKey(channelId) }),
    onError: (error) => {
      if (error instanceof ApiError && error.status === 409) {
        queryClient.invalidateQueries({ queryKey: commentsKey(channelId) });
      }
    },
  });
}
