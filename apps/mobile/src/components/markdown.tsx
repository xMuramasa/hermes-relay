// Assistant replies: react-native-marked (pure JS, marked-based) with code blocks in the mono face and a copy button.
import * as Clipboard from "expo-clipboard";
import { Check, Copy } from "lucide-react-native";
import { memo, useMemo, useState, type ReactNode } from "react";
import { Pressable, ScrollView, Text as RNText, View, type TextStyle, type ViewStyle } from "react-native";
import { Renderer, useMarkdown, type MarkedStyles } from "react-native-marked";
import { fonts, usePalette, type Palette } from "@/lib/theme";

function CodeBlock({ text, language, p }: { text: string; language?: string; p: Palette }) {
  const [copied, setCopied] = useState(false);
  async function copy() {
    await Clipboard.setStringAsync(text);
    setCopied(true);
    setTimeout(() => setCopied(false), 1500);
  }
  const Icon = copied ? Check : Copy;
  return (
    <View style={{ borderRadius: 10, borderWidth: 1, borderColor: p.border, backgroundColor: p.muted, marginVertical: 6, overflow: "hidden" }}>
      <View style={{ flexDirection: "row", alignItems: "center", justifyContent: "space-between", paddingHorizontal: 10, paddingTop: 6 }}>
        <RNText style={{ fontFamily: fonts.mono, fontSize: 11, color: p.mutedForeground }}>{language || "code"}</RNText>
        <Pressable onPress={copy} hitSlop={10} accessibilityRole="button" accessibilityLabel="Copy code">
          <Icon size={15} color={copied ? p.success : p.mutedForeground} />
        </Pressable>
      </View>
      <ScrollView horizontal showsHorizontalScrollIndicator={false} contentContainerStyle={{ padding: 10 }}>
        <RNText selectable style={{ fontFamily: fonts.mono, fontSize: 13, lineHeight: 19, color: p.foreground }}>
          {text}
        </RNText>
      </ScrollView>
    </View>
  );
}

class ChatRenderer extends Renderer {
  constructor(private p: Palette) {
    super({ selectable: true });
  }
  code(text: string, language?: string, _container?: ViewStyle, _text?: TextStyle): ReactNode {
    return <CodeBlock key={this.getKey()} text={text} language={language} p={this.p} />;
  }
}

function styles(p: Palette): MarkedStyles {
  const body: TextStyle = { fontFamily: fonts.sans, fontSize: 15, lineHeight: 23, color: p.foreground };
  const heading: TextStyle = { fontFamily: fonts.display, color: p.foreground, marginTop: 8, marginBottom: 4 };
  return {
    text: body,
    paragraph: { marginVertical: 4 },
    strong: { fontFamily: fonts.sansSemibold },
    em: { fontStyle: "italic" },
    link: { color: p.primary, textDecorationLine: "underline" },
    h1: { ...heading, fontSize: 28, lineHeight: 34 },
    h2: { ...heading, fontSize: 24, lineHeight: 30 },
    h3: { ...heading, fontSize: 21, lineHeight: 26 },
    h4: { ...heading, fontSize: 18 },
    codespan: { fontFamily: fonts.mono, fontSize: 13, color: p.primary, backgroundColor: p.muted },
    blockquote: { borderLeftWidth: 2, borderLeftColor: p.primary, paddingLeft: 10, marginVertical: 4 },
    hr: { backgroundColor: p.border, height: 1, marginVertical: 10 },
    li: body,
    table: { borderColor: p.border },
    tableCell: { borderColor: p.border },
  };
}

// Long answers render as many nodes; cut what we draw (the full text stays in the history).
const MAX_RENDERED = 60_000;

export const Markdown = memo(function Markdown({ text }: { text: string }) {
  const p = usePalette();
  const renderer = useMemo(() => new ChatRenderer(p), [p]);
  const st = useMemo(() => styles(p), [p]);
  const shown = text.length > MAX_RENDERED ? `${text.slice(0, MAX_RENDERED)}\n\n*…(cut here; ${text.length - MAX_RENDERED} more characters)*` : text;
  const nodes = useMarkdown(shown, { renderer, styles: st, theme: { colors: { text: p.foreground, link: p.primary, border: p.border, code: p.muted } } });
  return <View>{nodes}</View>;
});
