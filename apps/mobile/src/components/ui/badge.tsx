// From react-native-reusables (badge).
import { cva, type VariantProps } from "class-variance-authority";
import * as React from "react";
import { View } from "react-native";
import { TextClassContext } from "@/components/ui/text";
import { cn } from "@/lib/utils";

const badgeVariants = cva("flex-row items-center gap-1 rounded-full border px-2 py-0.5", {
  variants: {
    variant: {
      default: "border-primary/40 bg-primary/15",
      secondary: "border-transparent bg-muted",
      outline: "border-border",
    },
  },
  defaultVariants: { variant: "default" },
});

const badgeTextVariants = cva("font-sans-medium text-xs", {
  variants: {
    variant: { default: "text-primary", secondary: "text-muted-foreground", outline: "text-foreground" },
  },
  defaultVariants: { variant: "default" },
});

function Badge({ className, variant, ...props }: React.ComponentProps<typeof View> & VariantProps<typeof badgeVariants>) {
  return (
    <TextClassContext.Provider value={badgeTextVariants({ variant })}>
      <View className={cn(badgeVariants({ variant }), className)} {...props} />
    </TextClassContext.Provider>
  );
}

export { Badge };
