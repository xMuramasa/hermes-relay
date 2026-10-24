// Chat: REST history, then live events over the socket. One prompt at a time; no streamed text.
import { router, useLocalSearchParams } from "expo-router";
import { ArrowLeft, ArrowUp, Lock } from "lucide-react-native";
import { useCallback, useEffect, useMemo, useRef, useState } from "react";
import { ActivityIndicator, FlatList, KeyboardAvoidingView, Pressable, TextInput, View } from "react-native";
import { useSafeAreaInsets } from "react-native-safe-area-context";
import { ChatItem } from "@/components/chat-items";
import { ConnectionDot } from "@/components/connection-dot";
import { Skeleton } from "@/components/ui/skeleton";
import { Text } from "@/components/ui/text";
import { ApiError } from "@/lib/api";
import { isBusy } from "@/lib/chat";
import { isWritable, sourceLabel } from "@/lib/format";
import { MAX_TEXT } from "@/lib/protocol";
import { fonts, usePalette } from "@/lib/theme";
import { cn } from "@/lib/utils";
import { useApp } from "@/state/app";

export default function ChatScreen() {
  const params = useLocalSearchParams<{ id: string; title?: string; source?: string }>();
  const id = params.id;
  const source = params.source ?? "api_server"; // the relay still refuses writes to other surfaces' sessions
  const readOnly = !isWritable({ source });
  const chat = useApp((s) => s.chats[id]);
  const conn = useApp((s) => s.conn);
  const api = useApp((s) => s.api);
  const [text, setText] = useState("");
  const [historyError, setHistoryError] = useState<string | null>(null);
  const sentId = useRef<{ id: string; text: string } | null>(null);
  const insets = useSafeAreaInsets();
  const p = usePalette();

  const loadHistory = useCallback(async () => {
    if (!api) return;
    try {
      const messages = await api.getMessages(id);
      useApp.getState().dispatch(id, { type: "history", messages });
      setHistoryError(null);
    } catch (e) {
      setHistoryError(e instanceof ApiError ? e.message : "Couldn't load the history.");
    }
  }, [api, id]);

  useEffect(() => {
    useApp.getState().openChat(id);
    void loadHistory();
    return () => useApp.getState().closeChat(id);
  }, [id, loadHistory]);

  // Clear the composer once the relay acks the prompt; on an error the text stays so nothing is lost.
  useEffect(() => {
    const sent = sentId.current;
    if (!sent || !chat) return;
    const item = chat.items.find((i) => i.kind === "user" && i.clientMsgId === sent.id);
    if (item?.kind === "user" && item.status !== "pending") {
      if (item.status === "sent") setText((t) => (t === sent.text ? "" : t));
      sentId.current = null;
    }
  }, [chat]);

  const items = useMemo(() => (chat ? [...chat.items].reverse() : []), [chat]);
  const busy = chat ? isBusy(chat) : true;
  const canSend = conn === "connected" && !busy && text.trim().length > 0 && text.length <= MAX_TEXT;
  const onDismiss = useCallback((key: string) => useApp.getState().dispatch(id, { type: "dismiss", key }), [id]);

  function send() {
    if (!canSend) return;
    const clientMsgId = useApp.getState().send(id, text);
    if (clientMsgId) sentId.current = { id: clientMsgId, text };
  }

  return (
    <View className="flex-1 bg-background" style={{ paddingTop: insets.top }}>
      <View className="flex-row items-center gap-2 border-b border-border px-2 py-2">
        <Pressable onPress={() => router.back()} hitSlop={10} className="p-2" accessibilityRole="button" accessibilityLabel="Back">
          <ArrowLeft size={22} color={p.foreground} />
        </Pressable>
        <View className="flex-1">
          <Text numberOfLines={1} className="font-display text-2xl text-foreground">
            {params.title || "New chat"}
          </Text>
          <Text className="text-xs text-muted-foreground">{sourceLabel(source)}</Text>
        </View>
        <ConnectionDot showLabel />
      </View>

      <KeyboardAvoidingView behavior="padding" className="flex-1">
        {!chat?.historyLoaded && !historyError ? (
          <View className="flex-1 justify-end gap-4 p-4">
            <Skeleton className="h-10 w-3/5 self-end" />
            <Skeleton className="h-24 w-4/5" />
            <Skeleton className="h-6 w-2/5" />
          </View>
        ) : (
          <FlatList
            inverted
            data={items}
            keyExtractor={(i) => i.key}
            renderItem={({ item }) => <ChatItem item={item} onDismiss={onDismiss} />}
            contentContainerClassName="gap-3 px-4 py-4"
            keyboardShouldPersistTaps="handled"
            ListHeaderComponent={chat?.run ? <Working tool={chat.currentTool} waiting={chat.run.status === "waiting_for_approval"} /> : undefined}
            ListFooterComponent={
              historyError ? (
                <Pressable onPress={loadHistory} className="rounded-md border border-destructive/50 bg-destructive/10 p-3">
                  <Text className="text-sm text-destructive">{historyError} Tap to retry.</Text>
                </Pressable>
              ) : undefined
            }
            ListEmptyComponent={
              <View className="items-center gap-2 p-10" style={{ transform: [{ scaleY: -1 }] }}>
                <Text className="font-display-italic text-2xl text-muted-foreground">Speak, and Hermes answers.</Text>
              </View>
            }
          />
        )}

        {readOnly ? (
          <View className="flex-row items-center justify-center gap-2 border-t border-border p-4" style={{ paddingBottom: insets.bottom + 12 }}>
            <Lock size={14} color={p.mutedForeground} />
            <Text className="text-sm text-muted-foreground">{sourceLabel(source)} chat: read-only here</Text>
          </View>
        ) : (
          <View className="flex-row items-end gap-2 border-t border-border bg-background px-3 pt-2" style={{ paddingBottom: insets.bottom + 8 }}>
            <TextInput
              value={text}
              onChangeText={setText}
              multiline
              maxLength={MAX_TEXT}
              placeholder={conn !== "connected" ? "Not connected" : busy ? "Hermes is working…" : "Message Hermes"}
              placeholderTextColor={p.mutedForeground}
              accessibilityLabel="Message"
              className="max-h-40 min-h-11 flex-1 rounded-lg border border-input bg-card px-3 py-2.5 text-base text-foreground"
              style={{ fontFamily: fonts.sans }}
            />
            <Pressable
              onPress={send}
              disabled={!canSend}
              accessibilityRole="button"
              accessibilityLabel="Send"
              accessibilityState={{ disabled: !canSend }}
              className={cn("h-11 w-11 items-center justify-center rounded-full bg-primary", !canSend && "opacity-40")}
            >
              <ArrowUp size={20} color={p.primaryForeground} />
            </Pressable>
          </View>
        )}
      </KeyboardAvoidingView>
    </View>
  );
}

function Working({ tool, waiting }: { tool: string | null; waiting: boolean }) {
  const p = usePalette();
  return (
    <View className="flex-row items-center gap-2 py-1" accessibilityLiveRegion="polite">
      <ActivityIndicator size="small" color={p.primary} />
      <Text className="font-display-italic text-base text-primary">
        {waiting ? "waiting for approval…" : tool ? `working… · ${tool}` : "working…"}
      </Text>
    </View>
  );
}
