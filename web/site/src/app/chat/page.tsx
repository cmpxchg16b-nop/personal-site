import ChatApp from "@/components/chat/ChatApp";

// The chat page. Stage 1 is frontend-only: all data is mock data (see
// src/components/chat/mockData.ts); ChatApp owns the state and layout.
export default function ChatPage() {
  return <ChatApp />;
}
