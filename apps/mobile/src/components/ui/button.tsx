// From react-native-reusables (button), trimmed to the variants the app uses.
import { cva, type VariantProps } from "class-variance-authority";
import * as React from "react";
import { Pressable } from "react-native";
import { TextClassContext } from "@/components/ui/text";
import { cn } from "@/lib/utils";

const buttonVariants = cva("flex-row items-center justify-center gap-2 rounded-md active:opacity-80", {
  variants: {
    variant: {
      default: "bg-primary",
      destructive: "bg-destructive",
      outline: "border border-border bg-background",
      secondary: "bg-secondary",
      ghost: "",
    },
    size: {
      default: "h-11 px-5",
      sm: "h-9 px-3",
      lg: "h-12 px-8",
      icon: "h-10 w-10",
    },
  },
  defaultVariants: { variant: "default", size: "default" },
});

const buttonTextVariants = cva("font-sans-semibold text-sm", {
  variants: {
    variant: {
      default: "text-primary-foreground",
      destructive: "text-destructive-foreground",
      outline: "text-foreground",
      secondary: "text-secondary-foreground",
      ghost: "text-foreground",
    },
    size: { default: "", sm: "", lg: "text-base", icon: "" },
  },
  defaultVariants: { variant: "default", size: "default" },
});

type ButtonProps = React.ComponentProps<typeof Pressable> & VariantProps<typeof buttonVariants>;

function Button({ className, variant, size, disabled, ...props }: ButtonProps) {
  return (
    <TextClassContext.Provider value={buttonTextVariants({ variant, size })}>
      <Pressable
        accessibilityRole="button"
        accessibilityState={{ disabled: !!disabled }}
        disabled={disabled}
        className={cn(buttonVariants({ variant, size }), disabled && "opacity-50", className)}
        {...props}
      />
    </TextClassContext.Provider>
  );
}

export { Button, buttonTextVariants, buttonVariants };
export type { ButtonProps };
