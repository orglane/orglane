import type { ReactNode } from "react";

import { workspaceHost } from "@/lib/brand";

import { Logo } from "./logo";
import { ThemeToggle } from "./theme-toggle";

/** The frame of sign-in, setup and the passkey offer: a centered card. */
export function AuthLayout({ children, footer }: { children: ReactNode; footer?: ReactNode }) {
  return (
    <div className="auth-backdrop flex min-h-dvh flex-col">
      <header className="flex items-center justify-between px-4 py-4 sm:px-6">
        <Logo />
        <ThemeToggle />
      </header>
      <main className="flex flex-1 flex-col items-center px-4 pt-4 pb-10 sm:justify-center sm:pt-0 sm:pb-24">
        <div className="w-full max-w-[25rem] animate-fade-in">{children}</div>
        {footer && <div className="mt-6 text-center text-muted-foreground text-sm">{footer}</div>}
      </main>
      <footer className="px-4 pb-5 text-center text-muted-foreground text-xs">
        {workspaceHost()}
      </footer>
    </div>
  );
}

/** The card inside AuthLayout. */
export function AuthCard({
  title,
  description,
  children,
}: {
  title: ReactNode;
  description?: ReactNode;
  children: ReactNode;
}) {
  return (
    <div className="rounded-xl border bg-surface p-6 shadow-lg sm:p-8">
      <div className="mb-6 flex flex-col gap-1.5">
        <h1 className="font-semibold text-2xl text-surface-foreground tracking-tight">{title}</h1>
        {description && (
          <p className="text-muted-foreground text-sm leading-relaxed">{description}</p>
        )}
      </div>
      {children}
    </div>
  );
}
