import type { QueryClient } from "@tanstack/react-query";
import type { AnyRouter } from "@tanstack/react-router";
import { createContext, type ReactNode, useContext } from "react";

import { type Api, type Me, meKey } from "./api";
import { safeRedirect } from "./utils";

/** Why a session ended: the person signed out, or it expired / was revoked. */
export type SessionEndReason = "signedOut" | "expired";

const isMeKey = (key: readonly unknown[]) => key.length === 1 && key[0] === meKey[0];

// Screens where a 401 is an answer (wrong password), not a lost session.
const publicPaths = ["/sign-in", "/setup"];

/**
 * The one place where a signed-in browser becomes signed out. Whatever
 * notices it (the person signing out, a refetch of the current person that
 * finds no session, a 401 from any other request), the session's state is
 * cleared and the browser goes to sign-in: after an expiry with a "your
 * session ended" note and the page to return to.
 */
export class SessionGuard {
  private signedIn = false;
  private ending = false;
  private router: AnyRouter | undefined;

  constructor(
    private readonly api: Api,
    private readonly queryClient: QueryClient,
  ) {}

  /** Connects the router (created after the query client) and starts watching. */
  attach(router: AnyRouter): () => void {
    this.router = router;
    return this.queryClient.getQueryCache().subscribe((event) => {
      if (event.type !== "updated" || !isMeKey(event.query.queryKey)) {
        return;
      }
      const me = event.query.state.data as Me | null | undefined;
      if (me) {
        this.signedIn = true;
      } else if (me === null && this.signedIn) {
        // Not inside the cache's own notification: let the query settle.
        setTimeout(() => void this.end("expired"), 0);
      }
    });
  }

  /** A request was refused as unauthenticated. */
  unauthorized(): void {
    const path = this.router?.state.location.pathname ?? "";
    if (this.signedIn && !publicPaths.includes(path)) {
      void this.end("expired");
    }
  }

  async end(reason: SessionEndReason): Promise<void> {
    if (this.ending || !this.router) {
      return;
    }
    this.ending = true;
    this.signedIn = false;
    try {
      const location = this.router.state.location;
      const here = safeRedirect(
        location.pathname + location.searchStr + (location.hash ? `#${location.hash}` : ""),
      );
      this.api.setCsrfToken(undefined);
      await this.queryClient.cancelQueries({ predicate: (q) => !isMeKey(q.queryKey) });
      this.queryClient.removeQueries({ predicate: (q) => !isMeKey(q.queryKey) });
      this.queryClient.setQueryData(meKey, null);
      const back =
        here !== "/" &&
        !publicPaths.some((p) => here.startsWith(p)) &&
        !here.startsWith("/welcome/");
      await this.router.navigate({
        to: "/sign-in",
        search:
          reason === "signedOut"
            ? { signedOut: true }
            : { sessionEnded: true, ...(back ? { redirect: here } : {}) },
        replace: true,
      });
    } finally {
      this.ending = false;
    }
  }
}

const SessionGuardContext = createContext<SessionGuard | null>(null);

export function SessionGuardProvider({
  guard,
  children,
}: {
  guard: SessionGuard;
  children: ReactNode;
}) {
  return <SessionGuardContext.Provider value={guard}>{children}</SessionGuardContext.Provider>;
}

/** Ends the session in this browser (see SessionGuard). */
export function useEndSession(): (reason: SessionEndReason) => Promise<void> {
  const guard = useContext(SessionGuardContext);
  if (!guard) {
    throw new Error("useEndSession outside SessionGuardProvider");
  }
  return (reason) => guard.end(reason);
}
