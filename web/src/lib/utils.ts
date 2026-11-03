import { type ClassValue, clsx } from "clsx";
import { twMerge } from "tailwind-merge";

/** Joins class names, letting later Tailwind utilities override earlier ones. */
export function cn(...inputs: ClassValue[]): string {
  return twMerge(clsx(inputs));
}

/**
 * Returns `target` when it is a path on this origin (`/inbox?x=1`), otherwise
 * `fallback`. Used for redirect parameters, which must never lead elsewhere.
 */
export function safeRedirect(target: unknown, fallback = "/"): string {
  if (
    typeof target !== "string" ||
    !target.startsWith("/") ||
    target.startsWith("//") ||
    target.includes("\\")
  ) {
    return fallback;
  }
  try {
    const url = new URL(target, "https://orglane.invalid");
    if (url.origin !== "https://orglane.invalid") {
      return fallback;
    }
    return url.pathname + url.search + url.hash;
  } catch {
    return fallback;
  }
}

/** Initials for an avatar: "Alex Morgan" → "AM". */
export function initials(name: string): string {
  const parts = name.trim().split(/\s+/).filter(Boolean);
  const first = parts[0]?.[0] ?? "";
  const last = parts.length > 1 ? (parts[parts.length - 1]?.[0] ?? "") : "";
  return (first + last).toUpperCase() || "?";
}
