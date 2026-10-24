import { ChevronDown, ChevronRight, GitBranch, X } from "lucide-react-native";
import { memo, useState } from "react";
import { ActivityIndicator, Pressable, View } from "react-native";
import { Markdown } from "@/components/markdown";
import { Text } from "@/components/ui/text";
import type { Item } from "@/lib/chat";
import { formatDuration } from "@/lib/format";
import { usePalette } from "@/lib/theme";
import { cn } from "@/lib/utils";

type Of<K extends Item["kind"]> = Extract<Item, { kind: K }>;

function UserBubble({ item, onDismiss }: { item: Of<"user">; onDismiss: (key: string) => void }) {
  const p = usePalette();
  return (
    <View className="items-end gap-1 pl-12">
      <View className={cn("rounded-lg rounded-br-sm bg-secondary px-4 py-2.5", item.status === "pending" && "opacity-70")}>
        <Text selectable className="text-secondary-foreground">
          {item.text}
        </Text>
      </View>
      {item.status === "pending" && <Text className="text-xs text-muted-foreground">sending…</Text>}
      {item.notice && (
        <View className="flex-row items-start gap-2 rounded-md border border-destructive/50 bg-destructive/10 p-2.5" accessibilityLiveRegion="polite">
          <Text className="flex-1 text-sm text-destructive">{item.notice.message} Your text is still in the box below.</Text>
          <Pressable onPress={() => onDismiss(item.key)} hitSlop={10} accessibilityRole="button" accessibilityLabel="Dismiss">
            <X size={16} color={p.destructive} />
          </Pressable>
        </View>
      )}
    </View>
  );
}

function ToolRow({ item }: { item: Of<"tool"> }) {
  const [open, setOpen] = useState(false);
  const p = usePalette();
  const color = item.error ? p.destructive : p.mutedForeground;
  const Chevron = open ? ChevronDown : ChevronRight;
  const label = [item.tool, item.done ? formatDuration(item.duration) : "running…", item.error ? "error" : ""].filter(Boolean).join(" · ");
  return (
    <View className={cn("rounded-md border px-3 py-2", item.error ? "border-destructive/50" : "border-border")}>
      <Pressable onPress={() => setOpen(!open)} className="flex-row items-center gap-2" accessibilityRole="button" accessibilityState={{ expanded: open }}>
        {item.done ? <Chevron size={14} color={color} /> : <ActivityIndicator size="small" color={p.primary} />}
        <Text className={cn("font-mono text-xs", item.error ? "text-destructive" : "text-muted-foreground")}>{label}</Text>
      </Pressable>
      {open && (
        <View className="gap-2 pt-2">
          {item.args ? <Preview title="input" text={item.args} /> : null}
          {item.result ? <Preview title="output" text={item.result} /> : null}
          {!item.args && !item.result && <Text className="font-mono text-xs text-muted-foreground">no preview</Text>}
        </View>
      )}
    </View>
  );
}

function Preview({ title, text }: { title: string; text: string }) {
  return (
    <View className="gap-1">
      <Text className="font-mono text-[10px] uppercase tracking-widest text-muted-foreground">{title}</Text>
      <Text selectable className="rounded-sm bg-muted p-2 font-mono text-xs text-foreground">
        {text}
      </Text>
    </View>
  );
}

function SubagentCard({ item }: { item: Of<"subagent"> }) {
  const p = usePalette();
  const status = item.done ? [item.status ?? "done", formatDuration(item.duration)].filter(Boolean).join(" · ") : "working…";
  return (
    <View className="gap-1.5 rounded-lg border border-primary/30 bg-card p-3">
      <View className="flex-row items-center gap-2">
        <GitBranch size={14} color={p.primary} />
        <Text className="font-sans-medium text-xs uppercase tracking-widest text-primary">subagent</Text>
        <Text className="ml-auto font-mono text-xs text-muted-foreground">{status}</Text>
      </View>
      {item.goal ? <Text className="text-sm text-card-foreground">{item.goal}</Text> : null}
      {item.summary ? (
        <Text className="text-xs text-muted-foreground" numberOfLines={6}>
          {item.summary}
        </Text>
      ) : null}
    </View>
  );
}

export const ChatItem = memo(function ChatItem({ item, onDismiss }: { item: Item; onDismiss: (key: string) => void }) {
  switch (item.kind) {
    case "user":
      return <UserBubble item={item} onDismiss={onDismiss} />;
    case "assistant":
      return (
        <View className="pr-4">
          <Markdown text={item.text} />
          {item.truncated && <Text className="text-xs text-muted-foreground">Cut for length; reopen the chat for the full text.</Text>}
        </View>
      );
    case "tool":
      return <ToolRow item={item} />;
    case "subagent":
      return <SubagentCard item={item} />;
    case "status":
      return (
        <View className="flex-row items-center gap-3 py-1">
          <View className="h-px flex-1 bg-border" />
          <Text className={cn("font-display-italic text-sm", item.tone === "error" ? "text-destructive" : "text-muted-foreground")}>{item.text}</Text>
          <View className="h-px flex-1 bg-border" />
        </View>
      );
  }
});
