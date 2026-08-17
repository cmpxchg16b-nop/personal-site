"use client";

import { Fragment } from "react";
import { Divider, List } from "@mui/material";
import CommentItem from "./CommentItem";
import type { Comment } from "@/hooks/useComments";

type CommentListProps = {
  comments: Comment[];
};

// CommentList renders a channel's comments oldest-first, with a hairline
// divider between entries.
export default function CommentList({ comments }: CommentListProps) {
  return (
    <List disablePadding>
      {comments.map((comment, i) => (
        <Fragment key={comment.id}>
          {i > 0 && <Divider component="li" />}
          <CommentItem comment={comment} />
        </Fragment>
      ))}
    </List>
  );
}
