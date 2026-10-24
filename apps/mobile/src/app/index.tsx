// Sessions: every Hermes session (app ones writable, the others read-only), new chat, delete, unpair.
import { router, Stack, useFocusEffect } from "expo-router";
import { Lock, LogOut, Plus } from "lucide-react-native";
import { useCallback, useState } from "react";
import { FlatList, Pressable, RefreshControl, View } from "react-native";
import { useSafeAreaInsets } from "react-native-safe-area-context";
import { ConnectionDot } from "@/components/connection-dot";
import {
  AlertDialog,
  AlertDialogAction,
  AlertDialogCancel,
  AlertDialogContent,
  AlertDialogDescription,
  AlertDialogFooter,
  AlertDialogTitle,
} from "@/components/ui/alert-dialog";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Skeleton } from "@/components/ui/skeleton";
import { Text } from "@/components/ui/text";
import { ApiError, type Session } from "@/lib/api";
import { isWritable, relativeTime, sessionTitle, sourceLabel } from "@/lib/format";
import { usePalette } from "@/lib/theme";
import { useApp } from "@/state/app";

const errorText = (e: unknown) => (e instanceof ApiError ? e.message : "Something went wrong.");

export default function SessionsScreen() {
  const api = useApp((s) => s.api);
  const device = useApp((s) => s.creds?.device);
  const authError = useApp((s) => s.authError);
  const [sessions, setSessions] = useState<Session[] | null>(null);
  const [refreshing, setRefreshing] = useState(false);
  const [creating, setCreating] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const [toDelete, setToDelete] = useState<Session | null>(null);
  const [unpairOpen, setUnpairOpen] = useState(false);
  const p = usePalette();
  const insets = useSafeAreaInsets();

  const load = useCallback(async () => {
    if (!api) return;
    try {
      setSessions(await api.listSessions());
      setError(null);
    } catch (e) {
      setError(errorText(e));
      setSessions((s) => s ?? []);
    }
  }, [api]);

  useFocusEffect(
    useCallback(() => {
      void load();
    }, [load]),
  );

  async function refresh() {
    setRefreshing(true);
    useApp.getState().reconnect();
    await load();
    setRefreshing(false);
  }

  async function newChat() {
    if (!api) return;
    setCreating(true);
    try {
      const s = await api.createSession();
      router.push({ pathname: "/chat/[id]", params: { id: s.id, title: sessionTitle(s), source: s.source } });
    } catch (e) {
      setError(errorText(e));
    } finally {
      setCreating(false);
    }
  }

  async function confirmDelete() {
    const s = toDelete;
    setToDelete(null);
    if (!api || !s) return;
    try {
      await api.deleteSession(s.id);
      setSessions((list) => list?.filter((x) => x.id !== s.id) ?? null);
    } catch (e) {
      setError(errorText(e));
    }
  }

  return (
    <View className="flex-1 bg-background">
      <Stack.Screen
        options={{
          headerRight: () => (
            <View className="flex-row items-center">
              <ConnectionDot />
              <Pressable onPress={() => setUnpairOpen(true)} hitSlop={12} className="p-2" accessibilityRole="button" accessibilityLabel="Unpair this phone">
                <LogOut size={20} color={p.mutedForeground} />
              </Pressable>
            </View>
          ),
        }}
      />

      {(authError || error) && (
        <View className="mx-4 mb-2 rounded-md border border-destructive/50 bg-destructive/10 p-3">
          <Text className="text-sm text-destructive">{authError ?? error}</Text>
        </View>
      )}

      {sessions === null ? (
        <View className="gap-3 p-4">
          {[0, 1, 2, 3, 4].map((i) => (
            <View key={i} className="gap-2 rounded-lg border border-border p-4">
              <Skeleton className="h-5 w-3/5" />
              <Skeleton className="h-3 w-2/5" />
            </View>
          ))}
        </View>
      ) : (
        <FlatList
          data={sessions}
          keyExtractor={(s) => s.id}
          contentContainerClassName="gap-3 p-4"
          contentContainerStyle={{ paddingBottom: 96 + insets.bottom, flexGrow: 1 }}
          refreshControl={<RefreshControl refreshing={refreshing} onRefresh={refresh} colors={[p.primary]} progressBackgroundColor={p.card} />}
          ListEmptyComponent={
            <View className="flex-1 items-center justify-center gap-2 p-10">
              <Text className="font-display text-3xl text-foreground">No conversations yet</Text>
              <Text className="text-center text-muted-foreground">Start one with “New chat”. Telegram and CLI sessions show up here too.</Text>
            </View>
          }
          renderItem={({ item }) => (
            <SessionRow
              session={item}
              onPress={() => router.push({ pathname: "/chat/[id]", params: { id: item.id, title: sessionTitle(item), source: item.source } })}
              onLongPress={isWritable(item) ? () => setToDelete(item) : undefined}
              lockColor={p.mutedForeground}
            />
          )}
        />
      )}

      <View className="absolute inset-x-0 bottom-0 px-4 pt-3" style={{ paddingBottom: insets.bottom + 12 }}>
        <Button size="lg" onPress={newChat} disabled={creating || !api} className="shadow-lg">
          <Plus size={18} color={p.primaryForeground} />
          <Text>{creating ? "Opening…" : "New chat"}</Text>
        </Button>
      </View>

      <AlertDialog open={!!toDelete} onOpenChange={(o) => !o && setToDelete(null)}>
        <AlertDialogContent>
          <AlertDialogTitle>Delete this chat?</AlertDialogTitle>
          <AlertDialogDescription>“{toDelete ? sessionTitle(toDelete) : ""}” and its history are removed from Hermes. This can't be undone.</AlertDialogDescription>
          <AlertDialogFooter>
            <AlertDialogCancel>
              <Text>Cancel</Text>
            </AlertDialogCancel>
            <AlertDialogAction destructive onPress={confirmDelete}>
              <Text>Delete</Text>
            </AlertDialogAction>
          </AlertDialogFooter>
        </AlertDialogContent>
      </AlertDialog>

      <AlertDialog open={unpairOpen} onOpenChange={setUnpairOpen}>
        <AlertDialogContent>
          <AlertDialogTitle>Unpair this phone?</AlertDialogTitle>
          <AlertDialogDescription>
            The token is removed from this phone{device ? ` (“${device}”)` : ""}. To revoke it on the relay too, run `trismegistus-relay devices revoke`.
          </AlertDialogDescription>
          <AlertDialogFooter>
            <AlertDialogCancel>
              <Text>Cancel</Text>
            </AlertDialogCancel>
            <AlertDialogAction destructive onPress={() => void useApp.getState().unpair()}>
              <Text>Unpair</Text>
            </AlertDialogAction>
          </AlertDialogFooter>
        </AlertDialogContent>
      </AlertDialog>
    </View>
  );
}

function SessionRow({ session: s, onPress, onLongPress, lockColor }: { session: Session; onPress: () => void; onLongPress?: () => void; lockColor: string }) {
  const writable = isWritable(s);
  return (
    <Pressable
      onPress={onPress}
      onLongPress={onLongPress}
      accessibilityRole="button"
      accessibilityHint={writable ? "Long-press to delete" : "Read-only here"}
      className="gap-1.5 rounded-lg border border-border bg-card px-4 py-3 active:bg-muted"
    >
      <Text numberOfLines={1} className="font-display text-xl text-card-foreground">
        {sessionTitle(s)}
      </Text>
      <View className="flex-row items-center gap-2">
        <Badge variant={writable ? "default" : "secondary"}>
          {!writable && <Lock size={10} color={lockColor} />}
          <Text>{sourceLabel(s.source)}</Text>
        </Badge>
        <Text className="text-xs text-muted-foreground">
          {[relativeTime(s.lastActive), `${s.messageCount} message${s.messageCount === 1 ? "" : "s"}`].filter(Boolean).join(" · ")}
        </Text>
      </View>
    </Pressable>
  );
}
