import { Pressable, View } from "react-native";
import { Text } from "@/components/ui/text";
import { cn } from "@/lib/utils";
import { useApp } from "@/state/app";

const LABEL = { connected: "Connected", connecting: "Connecting…", disconnected: "Disconnected, tap to reconnect" };

/** The socket's state; tap to reconnect when it's down. */
export function ConnectionDot({ showLabel = false }: { showLabel?: boolean }) {
  const conn = useApp((s) => s.conn);
  return (
    <Pressable
      onPress={() => useApp.getState().reconnect()}
      disabled={conn !== "disconnected"}
      hitSlop={12}
      accessibilityRole="button"
      accessibilityLabel={LABEL[conn]}
      className="flex-row items-center gap-2 px-2"
    >
      <View
        className={cn(
          "h-2.5 w-2.5 rounded-full",
          conn === "connected" && "bg-success",
          conn === "connecting" && "bg-primary",
          conn === "disconnected" && "bg-destructive",
        )}
      />
      {showLabel && conn !== "connected" && <Text className="text-xs text-muted-foreground">{LABEL[conn]}</Text>}
    </Pressable>
  );
}
