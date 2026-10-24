import "../global.css";
// Per-weight imports: the package index would bundle every weight.
import { CormorantGaramond_500Medium_Italic } from "@expo-google-fonts/cormorant-garamond/500Medium_Italic";
import { CormorantGaramond_600SemiBold } from "@expo-google-fonts/cormorant-garamond/600SemiBold";
import { Inter_400Regular } from "@expo-google-fonts/inter/400Regular";
import { Inter_500Medium } from "@expo-google-fonts/inter/500Medium";
import { Inter_600SemiBold } from "@expo-google-fonts/inter/600SemiBold";
import { JetBrainsMono_400Regular } from "@expo-google-fonts/jetbrains-mono/400Regular";
import { PortalHost } from "@rn-primitives/portal";
import { useFonts } from "expo-font";
import { Stack } from "expo-router";
import { StatusBar } from "expo-status-bar";
import * as SystemUI from "expo-system-ui";
import { useEffect } from "react";
import { useColorScheme, View } from "react-native";
import { SafeAreaProvider } from "react-native-safe-area-context";
import { fonts, usePalette } from "@/lib/theme";
import { useApp } from "@/state/app";

export default function RootLayout() {
  const [fontsLoaded] = useFonts({
    CormorantGaramond_500Medium_Italic,
    CormorantGaramond_600SemiBold,
    Inter_400Regular,
    Inter_500Medium,
    Inter_600SemiBold,
    JetBrainsMono_400Regular,
  });
  const creds = useApp((s) => s.creds);
  const p = usePalette();
  const scheme = useColorScheme();

  useEffect(() => {
    void useApp.getState().load();
  }, []);
  useEffect(() => {
    void SystemUI.setBackgroundColorAsync(p.background);
  }, [p.background]);

  if (!fontsLoaded || creds === undefined) return <View style={{ flex: 1, backgroundColor: p.background }} />;

  return (
    <SafeAreaProvider>
      <StatusBar style={scheme === "light" ? "dark" : "light"} />
      <Stack
        screenOptions={{
          headerStyle: { backgroundColor: p.background },
          headerTintColor: p.foreground,
          headerTitleStyle: { fontFamily: fonts.display, fontSize: 24 },
          headerShadowVisible: false,
          contentStyle: { backgroundColor: p.background },
        }}
      >
        <Stack.Protected guard={!creds}>
          <Stack.Screen name="pair" options={{ headerShown: false }} />
        </Stack.Protected>
        <Stack.Protected guard={!!creds}>
          <Stack.Screen name="index" options={{ title: "Trismegistus" }} />
          <Stack.Screen name="chat/[id]" options={{ headerShown: false }} />
        </Stack.Protected>
      </Stack>
      <PortalHost />
    </SafeAreaProvider>
  );
}
