// From react-native-reusables (input).
import * as React from "react";
import { TextInput } from "react-native";
import { cn } from "@/lib/utils";

function Input({ className, ...props }: React.ComponentProps<typeof TextInput>) {
  return (
    <TextInput
      className={cn(
        "min-h-11 rounded-md border border-input bg-card px-3 py-2 font-sans text-base text-foreground placeholder:text-muted-foreground",
        props.editable === false && "opacity-50",
        className,
      )}
      {...props}
    />
  );
}

export { Input };
