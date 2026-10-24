// From react-native-reusables (alert-dialog), without the web branches and animations.
import * as AlertDialogPrimitive from "@rn-primitives/alert-dialog";
import * as React from "react";
import { View } from "react-native";
import { buttonTextVariants, buttonVariants } from "@/components/ui/button";
import { TextClassContext } from "@/components/ui/text";
import { cn } from "@/lib/utils";

const AlertDialog = AlertDialogPrimitive.Root;

function AlertDialogContent({ className, ...props }: AlertDialogPrimitive.ContentProps) {
  return (
    <AlertDialogPrimitive.Portal>
      <AlertDialogPrimitive.Overlay className="absolute inset-0 items-center justify-center bg-black/60 p-6">
        <AlertDialogPrimitive.Content
          className={cn("w-full max-w-md gap-4 rounded-lg border border-border bg-card p-6", className)}
          {...props}
        />
      </AlertDialogPrimitive.Overlay>
    </AlertDialogPrimitive.Portal>
  );
}

function AlertDialogFooter({ className, ...props }: React.ComponentProps<typeof View>) {
  return <View className={cn("flex-row justify-end gap-2", className)} {...props} />;
}

function AlertDialogTitle({ className, ...props }: AlertDialogPrimitive.TitleProps) {
  return <AlertDialogPrimitive.Title className={cn("font-display text-2xl text-foreground", className)} {...props} />;
}

function AlertDialogDescription({ className, ...props }: AlertDialogPrimitive.DescriptionProps) {
  return <AlertDialogPrimitive.Description className={cn("font-sans text-sm text-muted-foreground", className)} {...props} />;
}

function AlertDialogAction({ className, destructive, ...props }: AlertDialogPrimitive.ActionProps & { destructive?: boolean }) {
  const variant = destructive ? "destructive" : "default";
  return (
    <TextClassContext.Provider value={buttonTextVariants({ variant })}>
      <AlertDialogPrimitive.Action className={cn(buttonVariants({ variant }), className)} {...props} />
    </TextClassContext.Provider>
  );
}

function AlertDialogCancel({ className, ...props }: AlertDialogPrimitive.CancelProps) {
  return (
    <TextClassContext.Provider value={buttonTextVariants({ variant: "outline" })}>
      <AlertDialogPrimitive.Cancel className={cn(buttonVariants({ variant: "outline" }), className)} {...props} />
    </TextClassContext.Provider>
  );
}

export {
  AlertDialog,
  AlertDialogAction,
  AlertDialogCancel,
  AlertDialogContent,
  AlertDialogDescription,
  AlertDialogFooter,
  AlertDialogTitle,
};
