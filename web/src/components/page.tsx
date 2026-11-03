import type { ReactNode } from "react";

import { useDocumentTitle } from "@/lib/document-title";

/** The standard page frame inside the app shell: title, description, content. */
export function Page({
  title,
  documentTitle,
  description,
  actions,
  children,
}: {
  title: ReactNode;
  /** The tab title; defaults to `title` when it is a string. */
  documentTitle?: string;
  description?: ReactNode;
  actions?: ReactNode;
  children: ReactNode;
}) {
  useDocumentTitle(documentTitle ?? (typeof title === "string" ? title : undefined));
  return (
    <div className="mx-auto w-full max-w-5xl animate-fade-in px-4 py-6 sm:px-8 sm:py-10">
      <header className="mb-6 flex flex-wrap items-end justify-between gap-4 sm:mb-8">
        <div className="min-w-0">
          <h1 className="font-semibold text-2xl tracking-tight sm:text-[1.75rem] sm:leading-9">
            {title}
          </h1>
          {description && (
            <p className="mt-1 text-muted-foreground text-sm sm:text-[15px]">{description}</p>
          )}
        </div>
        {actions && <div className="flex shrink-0 gap-2">{actions}</div>}
      </header>
      {children}
    </div>
  );
}
