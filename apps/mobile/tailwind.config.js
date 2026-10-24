/** @type {import('tailwindcss').Config} */
const color = (name) => `hsl(var(--${name}) / <alpha-value>)`;

module.exports = {
  content: ["./src/**/*.{ts,tsx}"],
  presets: [require("nativewind/preset")],
  darkMode: "media",
  theme: {
    extend: {
      colors: {
        background: color("background"),
        foreground: color("foreground"),
        card: { DEFAULT: color("card"), foreground: color("card-foreground") },
        muted: { DEFAULT: color("muted"), foreground: color("muted-foreground") },
        primary: { DEFAULT: color("primary"), foreground: color("primary-foreground") },
        secondary: { DEFAULT: color("secondary"), foreground: color("secondary-foreground") },
        accent: { DEFAULT: color("accent"), foreground: color("accent-foreground") },
        destructive: { DEFAULT: color("destructive"), foreground: color("destructive-foreground") },
        success: color("success"),
        border: color("border"),
        input: color("input"),
        ring: color("ring"),
      },
      // React Native picks weights by family name, so each weight is its own family.
      fontFamily: {
        display: ["CormorantGaramond_600SemiBold"],
        "display-italic": ["CormorantGaramond_500Medium_Italic"],
        sans: ["Inter_400Regular"],
        "sans-medium": ["Inter_500Medium"],
        "sans-semibold": ["Inter_600SemiBold"],
        mono: ["JetBrainsMono_400Regular"],
      },
      borderRadius: { lg: "14px", md: "10px", sm: "6px" },
    },
  },
  plugins: [],
};
