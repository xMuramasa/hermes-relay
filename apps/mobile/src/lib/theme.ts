// The theme colors from src/global.css as hex, for props that take a color instead of a className (icons, the
// header, RefreshControl, Markdown). Keep the two in step.
import { useColorScheme } from "react-native";

export const palette = {
  light: {
    background: "#f3ede2",
    foreground: "#171b30",
    card: "#faf6f0",
    muted: "#e4dccd",
    mutedForeground: "#675c51",
    primary: "#885f20",
    primaryForeground: "#faf6f0",
    destructive: "#ae2929",
    success: "#2a6f4d",
    border: "#d0c5b3",
  },
  dark: {
    background: "#0d0e1c",
    foreground: "#e7e1d5",
    card: "#151728",
    muted: "#212336",
    mutedForeground: "#9295aa",
    primary: "#cea450",
    primaryForeground: "#0e1020",
    destructive: "#d65151",
    success: "#54b685",
    border: "#2a2d41",
  },
};
export type Palette = (typeof palette)["dark"];

export function usePalette(): Palette {
  return useColorScheme() === "light" ? palette.light : palette.dark;
}

export const fonts = {
  display: "CormorantGaramond_600SemiBold",
  sans: "Inter_400Regular",
  sansMedium: "Inter_500Medium",
  sansSemibold: "Inter_600SemiBold",
  mono: "JetBrainsMono_400Regular",
};
