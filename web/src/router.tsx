import { type QueryClient, useQuery } from "@tanstack/react-query";
import {
  createRootRouteWithContext,
  createRoute,
  createRouter,
  Outlet,
  redirect,
} from "@tanstack/react-router";
import { AppShell } from "./components/app-shell";
import { type Api, meQuery, setupStatusQuery } from "./lib/api";
import { safeRedirect, welcomeDestination } from "./lib/utils";
import { AcceptInvitationPage } from "./routes/accept-invitation";
import { AppInstancePage, AppsPage, AppsSettingsPage } from "./routes/apps";
import { InstallAppPage } from "./routes/apps-install";
import { EmailSettingsPage } from "./routes/email-settings";
import { HomePage } from "./routes/home";
import { InvitationsPage } from "./routes/invitations";
import { PasskeyOfferPage } from "./routes/passkey-offer";
import { PeoplePage } from "./routes/people";
import { PeopleImportPage } from "./routes/people-import";
import { PersonProfilePage } from "./routes/person-profile";
import { InboxPage } from "./routes/placeholders";
import { ProfileFieldsPage } from "./routes/profile-fields";
import { SecurityPage } from "./routes/security";
import { SetupPage } from "./routes/setup";
import { SignInPage, type SignInSearch } from "./routes/sign-in";
import { ErrorPage, NotFoundPage, PendingPage } from "./routes/status";
import { TeamPage, TeamsPage } from "./routes/teams";
import { WelcomePage } from "./routes/welcome";

/**
 * Renders the signed-in part of the app only while there is a session: when
 * it ends, the screens unmount at once and the SessionGuard takes the
 * browser to sign-in.
 */
function SignedInOutlet() {
  const { api } = authedRoute.useRouteContext();
  const { data: me } = useQuery(meQuery(api));
  return me ? <Outlet /> : null;
}

export interface RouterContext {
  api: Api;
  queryClient: QueryClient;
}

const str = (v: unknown): string | undefined => (typeof v === "string" && v !== "" ? v : undefined);

const rootRoute = createRootRouteWithContext<RouterContext>()({
  component: Outlet,
  notFoundComponent: NotFoundPage,
});

/** First run: create the owner. Only while the instance has none. */
const setupRoute = createRoute({
  getParentRoute: () => rootRoute,
  path: "/setup",
  beforeLoad: async ({ context }) => {
    const status = await context.queryClient.ensureQueryData(setupStatusQuery(context.api));
    if (!status.required) {
      throw redirect({ to: "/sign-in" });
    }
  },
  component: SetupPage,
});

/**
 * Sign-in, for the workspace or, with `authRequest`, for an application
 * signing in through Orglane (OIDC): /oidc/login sends the browser here and
 * the page returns to it once signed in.
 */
const signInRoute = createRoute({
  getParentRoute: () => rootRoute,
  path: "/sign-in",
  validateSearch: (search: Record<string, unknown>): SignInSearch => {
    const out: SignInSearch = {};
    const authRequest = str(search.authRequest);
    const email = str(search.email);
    const target = str(search.redirect);
    if (authRequest) out.authRequest = authRequest;
    if (email) out.email = email;
    if (target) out.redirect = safeRedirect(target);
    if (search.signedOut === true || search.signedOut === "true") out.signedOut = true;
    if (search.sessionEnded === true || search.sessionEnded === "true") out.sessionEnded = true;
    if (search.error === "credentials" || search.error === "limited" || search.error === "failed")
      out.error = search.error;
    return out;
  },
  beforeLoad: async ({ context, search }) => {
    const status = await context.queryClient.ensureQueryData(setupStatusQuery(context.api));
    if (status.required) {
      throw redirect({ to: "/setup" });
    }
    if (search.authRequest) {
      return; // an application asked for a (fresh) sign-in
    }
    const me = await context.queryClient.ensureQueryData(meQuery(context.api));
    if (me) {
      throw redirect({ href: safeRedirect(search.redirect) });
    }
  },
  component: function SignIn() {
    return <SignInPage search={signInRoute.useSearch()} />;
  },
});

const inviteRoute = createRoute({
  getParentRoute: () => rootRoute,
  path: "/invite",
  component: AcceptInvitationPage,
});

/** Everything below needs a signed-in person. */
const authedRoute = createRoute({
  getParentRoute: () => rootRoute,
  id: "authed",
  beforeLoad: async ({ context, location }) => {
    const me = await context.queryClient.ensureQueryData(meQuery(context.api));
    if (!me) {
      const target = location.href === "/" ? undefined : location.href;
      throw redirect({ to: "/sign-in", search: target ? { redirect: target } : {} });
    }
  },
  component: SignedInOutlet,
});

const passkeyOfferRoute = createRoute({
  getParentRoute: () => authedRoute,
  path: "/welcome/passkey",
  validateSearch: (search: Record<string, unknown>): { redirect?: string } => {
    const target = str(search.redirect);
    return target ? { redirect: safeRedirect(target) } : {};
  },
  component: function PasskeyOffer() {
    return <PasskeyOfferPage redirect={passkeyOfferRoute.useSearch().redirect} />;
  },
});

const welcomeRoute = createRoute({
  getParentRoute: () => authedRoute,
  path: "/welcome",
  validateSearch: (search: Record<string, unknown>): { redirect?: string } => {
    const target = welcomeDestination(search.redirect);
    return target === "/" ? {} : { redirect: target };
  },
  component: function Welcome() {
    return <WelcomePage redirect={welcomeRoute.useSearch().redirect} />;
  },
});

const shellRoute = createRoute({
  getParentRoute: () => authedRoute,
  id: "shell",
  component: AppShell,
});

const homeRoute = createRoute({ getParentRoute: () => shellRoute, path: "/", component: HomePage });
const inboxRoute = createRoute({
  getParentRoute: () => shellRoute,
  path: "/inbox",
  component: InboxPage,
});
const peopleRoute = createRoute({
  getParentRoute: () => shellRoute,
  path: "/people",
  component: PeoplePage,
});
const appsRoute = createRoute({
  getParentRoute: () => shellRoute,
  path: "/apps",
  component: AppsPage,
});
const teamsRoute = createRoute({
  getParentRoute: () => shellRoute,
  path: "/teams",
  component: TeamsPage,
});
const teamRoute = createRoute({
  getParentRoute: () => shellRoute,
  path: "/teams/$teamId",
  component: function Team() {
    return <TeamPage key={teamRoute.useParams().teamId} teamId={teamRoute.useParams().teamId} />;
  },
});
const securityRoute = createRoute({
  getParentRoute: () => shellRoute,
  path: "/settings/security",
  component: SecurityPage,
});
const peopleImportRoute = createRoute({
  getParentRoute: () => shellRoute,
  path: "/people/import",
  validateSearch: (search: Record<string, unknown>): { runId?: string } => {
    const runId = str(search.runId);
    return runId ? { runId } : {};
  },
  component: function PeopleImport() {
    return <PeopleImportPage runId={peopleImportRoute.useSearch().runId} />;
  },
});
const invitationsRoute = createRoute({
  getParentRoute: () => shellRoute,
  path: "/people/invitations",
  component: InvitationsPage,
});

import { GitHubAccountCallbackPage, GitHubAccountPage } from "./routes/github-account";
import { GitHubConnectCallbackPage, GitHubConnectPage } from "./routes/github-connect";

const githubConnectRoute = createRoute({
  getParentRoute: () => shellRoute,
  path: "/settings/apps/github/connect",
  component: GitHubConnectPage,
});
const githubCallbackRoute = createRoute({
  getParentRoute: () => shellRoute,
  path: "/settings/apps/github/callback",
  component: GitHubConnectCallbackPage,
});
const githubAccountRoute = createRoute({
  getParentRoute: () => shellRoute,
  path: "/me/accounts/github",
  component: GitHubAccountPage,
});
const githubAccountCallbackRoute = createRoute({
  getParentRoute: () => shellRoute,
  path: "/me/accounts/github/callback",
  component: GitHubAccountCallbackPage,
});

const appsSettingsRoute = createRoute({
  getParentRoute: () => shellRoute,
  path: "/settings/apps",
  component: AppsSettingsPage,
});
const appInstanceRoute = createRoute({
  getParentRoute: () => shellRoute,
  path: "/settings/apps/$instanceId",
  component: function AppInstance() {
    const { instanceId } = appInstanceRoute.useParams();
    return <AppInstancePage key={instanceId} instanceId={instanceId} />;
  },
});
const appInstallRoute = createRoute({
  getParentRoute: () => shellRoute,
  path: "/settings/apps/install/$providerId",
  component: function AppInstall() {
    const { providerId } = appInstallRoute.useParams();
    return <InstallAppPage key={providerId} providerId={providerId} />;
  },
});
const emailSettingsRoute = createRoute({
  getParentRoute: () => shellRoute,
  path: "/settings/email",
  component: EmailSettingsPage,
});

const profileFieldsRoute = createRoute({
  getParentRoute: () => shellRoute,
  path: "/settings/profile-fields",
  component: ProfileFieldsPage,
});
const personProfileRoute = createRoute({
  getParentRoute: () => shellRoute,
  path: "/people/$personId",
  component: function PersonProfile() {
    return (
      <PersonProfilePage
        key={personProfileRoute.useParams().personId}
        personId={personProfileRoute.useParams().personId}
      />
    );
  },
});

export const routeTree = rootRoute.addChildren([
  setupRoute,
  signInRoute,
  inviteRoute,
  authedRoute.addChildren([
    passkeyOfferRoute,
    welcomeRoute,
    shellRoute.addChildren([
      homeRoute,
      inboxRoute,
      peopleRoute,
      teamsRoute,
      teamRoute,
      appsRoute,
      securityRoute,
      profileFieldsRoute,
      personProfileRoute,
      invitationsRoute,
      peopleImportRoute,
      emailSettingsRoute,
      appsSettingsRoute,
      githubConnectRoute,
      githubCallbackRoute,
      githubAccountRoute,
      githubAccountCallbackRoute,
      appInstanceRoute,
      appInstallRoute,
    ]),
  ]),
]);

export function createAppRouter(
  context: RouterContext,
  options: { history?: Parameters<typeof createRouter>[0]["history"] } = {},
) {
  return createRouter({
    routeTree,
    context,
    defaultPreload: "intent",
    defaultPreloadStaleTime: 0,
    defaultPendingMs: 400,
    defaultPendingComponent: PendingPage,
    defaultErrorComponent: ({ error }) => <ErrorPage error={error} />,
    scrollRestoration: true,
    ...(options.history ? { history: options.history } : {}),
  });
}

declare module "@tanstack/react-router" {
  interface Register {
    router: ReturnType<typeof createAppRouter>;
  }
}
