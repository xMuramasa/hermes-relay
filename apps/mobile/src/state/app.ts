// App state: stored pairing, the relay socket, and one chat state per open session.
import { randomUUID } from "expo-crypto";
import * as SecureStore from "expo-secure-store";
import { create } from "zustand";
import { createApi, type Api, type Creds } from "@/lib/api";
import { chatReducer, initialChat, type Action, type ChatState } from "@/lib/chat";
import { isId, MAX_TEXT, promptMsg, type ServerMessage } from "@/lib/protocol";
import { RelaySocket, type ConnState } from "@/lib/socket";
import { normalizeUrl } from "@/lib/pairing";

const CREDS_KEY = "trismegistus.pairing";

type AppState = {
  /** undefined while secure-store is read; null when not paired. */
  creds: Creds | null | undefined;
  api: Api | null;
  conn: ConnState;
  /** Set when the relay refuses the token (device revoked). */
  authError: string | null;
  chats: Record<string, ChatState>;

  load(): Promise<void>;
  pair(creds: Creds): Promise<void>;
  unpair(): Promise<void>;
  reconnect(): void;
  openChat(sessionId: string): void;
  closeChat(sessionId: string): void;
  dispatch(sessionId: string, action: Action): void;
  /** Sends a prompt and returns its client_msg_id; null if the socket is down (nothing is queued). */
  send(sessionId: string, text: string): string | null;
};

let socket: RelaySocket | null = null;

function parseCreds(raw: string | null): Creds | null {
  try {
    const v = raw ? JSON.parse(raw) : null;
    const url = typeof v?.url === "string" ? normalizeUrl(v.url) : null;
    if (!url || typeof v.token !== "string" || typeof v.device !== "string") return null;
    return { url, token: v.token, device: v.device };
  } catch {
    return null;
  }
}

export const useApp = create<AppState>((set, get) => {
  const onMessage = (msg: ServerMessage) => {
    if (msg.type === "error" && msg.code === "unauthorized" && !msg.ref) {
      set({ authError: "The relay refused this device's token. It may have been revoked; pair again." });
      return;
    }
    // Every open chat sees it; the reducer keeps what matches its session_id or one of its client_msg_ids.
    for (const id of Object.keys(get().chats)) get().dispatch(id, { type: "server", msg });
  };

  const startSocket = (creds: Creds) => {
    socket?.close();
    socket = new RelaySocket({ url: creds.url, token: creds.token, onMessage, onState: (conn) => set({ conn }) });
    socket.connect();
  };

  return {
    creds: undefined,
    api: null,
    conn: "disconnected",
    authError: null,
    chats: {},

    async load() {
      const creds = parseCreds(await SecureStore.getItemAsync(CREDS_KEY));
      set({ creds, api: creds ? createApi(creds) : null });
      if (creds) startSocket(creds);
    },

    async pair(creds) {
      await SecureStore.setItemAsync(CREDS_KEY, JSON.stringify(creds));
      set({ creds, api: createApi(creds), authError: null, chats: {} });
      startSocket(creds);
    },

    async unpair() {
      socket?.close();
      socket = null;
      await SecureStore.deleteItemAsync(CREDS_KEY);
      set({ creds: null, api: null, conn: "disconnected", authError: null, chats: {} });
    },

    reconnect() {
      const { creds } = get();
      if (creds && !get().authError) socket?.connect();
    },

    openChat(sessionId) {
      if (!isId(sessionId)) return;
      if (!get().chats[sessionId]) set((s) => ({ chats: { ...s.chats, [sessionId]: initialChat(sessionId) } }));
      socket?.subscribe(sessionId);
      get().reconnect(); // opening a chat reconnects a dropped socket (2a has no automatic reconnect)
    },

    closeChat(sessionId) {
      socket?.unsubscribe(sessionId);
      set((s) => {
        const { [sessionId]: _, ...chats } = s.chats;
        return { chats };
      });
    },

    dispatch(sessionId, action) {
      set((s) => {
        const chat = s.chats[sessionId];
        if (!chat) return s;
        const next = chatReducer(chat, action);
        return next === chat ? s : { chats: { ...s.chats, [sessionId]: next } };
      });
    },

    send(sessionId, raw) {
      const text = raw.trim();
      if (!text || text.length > MAX_TEXT || !socket || !isId(sessionId)) return null;
      const clientMsgId = randomUUID();
      if (!socket.send(promptMsg(clientMsgId, sessionId, text))) return null;
      get().dispatch(sessionId, { type: "send", clientMsgId, text });
      return clientMsgId;
    },
  };
});
