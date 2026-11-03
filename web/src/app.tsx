import { MutationCache, QueryCache, QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { type RouterHistory, RouterProvider } from "@tanstack/react-router";
import { useState } from "react";

import { I18nProvider } from "./i18n";
import { type Api, createApi, hasStatus } from "./lib/api";
import { ApiProvider } from "./lib/api-context";
import { SessionGuard, SessionGuardProvider } from "./lib/session-guard";
import { ThemeProvider } from "./lib/theme";
import { createAppRouter } from "./router";

export function createQueryClient(onUnauthorized: () => void): QueryClient {
  // A 401 means the session ended (expired, or revoked from another
  // device); the SessionGuard decides what that means for the screen.
  const handle = (err: unknown) => {
    if (hasStatus(err, 401)) {
      onUnauthorized();
    }
  };
  return new QueryClient({
    queryCache: new QueryCache({ onError: handle }),
    mutationCache: new MutationCache({ onError: handle }),
    defaultOptions: {
      queries: {
        retry: (count, err) => !hasStatus(err, 400, 401, 403, 404, 409, 429) && count < 2,
        refetchOnWindowFocus: true,
      },
    },
  });
}

export function App({ api: givenApi, history }: { api?: Api; history?: RouterHistory }) {
  const [state] = useState(() => {
    const api = givenApi ?? createApi();
    let guard: SessionGuard | undefined;
    const queryClient = createQueryClient(() => guard?.unauthorized());
    guard = new SessionGuard(api, queryClient);
    const router = createAppRouter({ api, queryClient }, history ? { history } : {});
    guard.attach(router);
    return { api, queryClient, router, guard };
  });

  return (
    <ApiProvider api={state.api}>
      <QueryClientProvider client={state.queryClient}>
        <SessionGuardProvider guard={state.guard}>
          <ThemeProvider>
            <I18nProvider onLocaleChange={state.api.setLocale}>
              <RouterProvider router={state.router} />
            </I18nProvider>
          </ThemeProvider>
        </SessionGuardProvider>
      </QueryClientProvider>
    </ApiProvider>
  );
}
