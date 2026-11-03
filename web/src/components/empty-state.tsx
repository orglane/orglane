import type { LucideIcon } from "lucide-react";
import type { ReactNode } from "react";

import { cn } from "@/lib/utils";

/** A friendly placeholder for a page or list with nothing to show yet. */
export function EmptyState({
  icon: Icon,
  title,
  description,
  children,
  className,
}: {
  icon: LucideIcon;
  title: ReactNode;
  description?: ReactNode;
  children?: ReactNode;
  className?: string;
}) {
  return (
    <div
      className={cn(
        "flex flex-col items-center rounded-xl border border-dashed px-6 py-14 text-center sm:py-20",
        className,
      )}
    >
      <div className="relative mb-5">
        <div
          className="absolute -inset-3 rounded-[1.25rem] bg-primary-subtle opacity-60"
          aria-hidden="true"
        />
        <div className="relative flex size-12 items-center justify-center rounded-xl border bg-surface text-primary shadow-sm">
          <Icon className="size-6" aria-hidden="true" strokeWidth={1.75} />
        </div>
      </div>
      <h2 className="font-semibold text-base tracking-tight">{title}</h2>
      {description && (
        <p className="mt-1.5 max-w-sm text-muted-foreground text-sm leading-relaxed">
          {description}
        </p>
      )}
      {children && <div className="mt-6 flex flex-wrap justify-center gap-2">{children}</div>}
    </div>
  );
}
