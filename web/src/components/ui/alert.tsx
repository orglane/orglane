import { cva, type VariantProps } from "class-variance-authority";
import { CircleAlert, CircleCheck, Info } from "lucide-react";
import type { ComponentProps } from "react";

import { cn } from "@/lib/utils";

const alertVariants = cva(
  "flex items-start gap-2.5 rounded-lg px-3.5 py-3 text-sm animate-fade-in [&>svg]:mt-0.5 [&>svg]:size-4 [&>svg]:shrink-0",
  {
    variants: {
      variant: {
        error: "bg-destructive-subtle text-destructive-subtle-foreground",
        success: "bg-success-subtle text-success-subtle-foreground",
        info: "bg-primary-subtle text-primary-subtle-foreground",
      },
    },
    defaultVariants: { variant: "error" },
  },
);

const icons = { error: CircleAlert, success: CircleCheck, info: Info };

/** An inline message. Errors are announced to screen readers at once. */
export function Alert({
  className,
  variant = "error",
  children,
  ...props
}: ComponentProps<"div"> & VariantProps<typeof alertVariants>) {
  const Icon = icons[variant ?? "error"];
  return (
    <div
      role={variant === "error" ? "alert" : "status"}
      className={cn(alertVariants({ variant }), className)}
      {...props}
    >
      <Icon aria-hidden="true" />
      <div className="min-w-0 flex-1 leading-5">{children}</div>
    </div>
  );
}
