// "As above, so below": two interlaced triangles in a double ring, drawn in the accent color.
import Svg, { Circle, Path } from "react-native-svg";
import { usePalette } from "@/lib/theme";

export function Sigil({ size = 120 }: { size?: number }) {
  const c = usePalette().primary;
  return (
    <Svg width={size} height={size} viewBox="0 0 100 100" accessibilityElementsHidden importantForAccessibility="no">
      <Circle cx={50} cy={50} r={47} stroke={c} strokeWidth={1.2} fill="none" />
      <Circle cx={50} cy={50} r={42} stroke={c} strokeWidth={0.6} fill="none" opacity={0.6} />
      <Path d="M50 14 L81 68 L19 68 Z" stroke={c} strokeWidth={1.4} fill="none" strokeLinejoin="round" />
      <Path d="M50 86 L19 32 L81 32 Z" stroke={c} strokeWidth={1.4} fill="none" strokeLinejoin="round" opacity={0.75} />
      <Circle cx={50} cy={50} r={6} stroke={c} strokeWidth={1.2} fill="none" />
      <Circle cx={50} cy={50} r={1.6} fill={c} />
    </Svg>
  );
}
