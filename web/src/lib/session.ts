import { useMutation, useQueryClient, useSuspenseQuery } from "@tanstack/react-query";
import { useNavigate } from "@tanstack/react-router";
import { useRef } from "react";

import { call, hasStatus, type Me, meKey, meQuery, type SignInResult } from "./api";
import { useApi } from "./api-context";
import { browserNavigation } from "./navigation";
import { useEndSession } from "./session-guard";
import { safeRedirect, welcomeDestination } from "./utils";
import { passkeysSupported } from "./webauthn";

/** The OIDC endpoint that finishes an authorization request once signed in. */
export function oidcContinueUrl(authRequest: string): string {
  return `/oidc/login?authRequest=${encodeURIComponent(authRequest)}`;
}

/**
 * The signed-in person; only inside the authenticated part of the app.
 * While a session ends (see SessionGuard), it keeps returning the last known
 * person until the screen goes away.
 */
export function useMe(): Me {
  const api = useApi();
  const { data } = useSuspenseQuery(meQuery(api));
  const last = useRef(data);
  if (data) {
    last.current = data;
  }
  if (!last.current) {
    throw new Error("useMe outside the signed-in app");
  }
  return last.current;
}

export interface AfterSignIn {
  /** An OIDC authorization request to complete (sign-in for another app). */
  authRequest?: string | undefined;
  /** Where to go in the workspace afterwards. */
  redirect?: string | undefined;
}

/**
 * Continues after a successful sign-in or setup: back to the application
 * that asked (OIDC), to the passkey offer, or into the workspace.
 */
export function useAfterSignIn() {
  const api = useApi();
  const queryClient = useQueryClient();
  const navigate = useNavigate();
  return async (result: SignInResult, { authRequest, redirect }: AfterSignIn) => {
    api.setCsrfToken(result.csrfToken);
    queryClient.removeQueries({ predicate: (q) => q.queryKey[0] !== meKey[0] });
    if (authRequest) {
      // The person is on their way to another application: don't stop them
      // with the passkey offer.
      browserNavigation.assign(oidcContinueUrl(authRequest));
      return;
    }
    const me = await queryClient.fetchQuery({ ...meQuery(api), staleTime: 0 });
    const destination = safeRedirect(redirect);
    const returnTo = welcomeDestination(destination);
    const target = me?.welcomePending
      ? returnTo === "/"
        ? "/welcome"
        : `/welcome?redirect=${encodeURIComponent(returnTo)}`
      : destination;
    if (result.passkeyOffer && passkeysSupported()) {
      await navigate({
        to: "/welcome/passkey",
        search: target === "/" ? {} : { redirect: target },
      });
    } else {
      await navigate({ href: target, replace: true });
    }
  };
}

/** Signs out of this browser and returns to the sign-in screen. */
export function useSignOut() {
  const api = useApi();
  const endSession = useEndSession();
  return useMutation({
    mutationFn: async () => {
      try {
        await call(api.client.POST("/api/v1/auth/sign-out"));
      } catch (err) {
        // Already signed out (expired or revoked elsewhere) is fine.
        if (!hasStatus(err, 401)) {
          throw err;
        }
      }
    },
    onSuccess: () => endSession("signedOut"),
  });
}
