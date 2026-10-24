// From react-native-reusables (card), trimmed.
import * as React from "react";
import { View } from "react-native";
import { Text } from "@/components/ui/text";
import { cn } from "@/lib/utils";

function Card({ className, ...props }: React.ComponentProps<typeof View>) {
  return <View className={cn("rounded-lg border border-border bg-card", className)} {...props} />;
}

function CardTitle({ className, ...props }: React.ComponentProps<typeof Text>) {
  return <Text className={cn("font-display text-xl text-card-foreground", className)} {...props} />;
}

function CardDescription({ className, ...props }: React.ComponentProps<typeof Text>) {
  return <Text className={cn("text-sm text-muted-foreground", className)} {...props} />;
}

export { Card, CardDescription, CardTitle };
