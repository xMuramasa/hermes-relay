// From react-native-reusables (skeleton).
import * as React from "react";
import { View } from "react-native";
import Animated, { useAnimatedStyle, useSharedValue, withRepeat, withSequence, withTiming } from "react-native-reanimated";
import { cn } from "@/lib/utils";

function Skeleton({ className, ...props }: Omit<React.ComponentProps<typeof View>, "style">) {
  const opacity = useSharedValue(1);
  React.useEffect(() => {
    opacity.value = withRepeat(withSequence(withTiming(0.45, { duration: 900 }), withTiming(1, { duration: 900 })), -1);
  }, [opacity]);
  const style = useAnimatedStyle(() => ({ opacity: opacity.value }));
  return <Animated.View style={style} className={cn("rounded-md bg-muted", className)} {...props} />;
}

export { Skeleton };
