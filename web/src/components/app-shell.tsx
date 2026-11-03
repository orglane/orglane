import { Link, Outlet } from "@tanstack/react-router";
import { House, Inbox, LayoutGrid, type LucideIcon, Users } from "lucide-react";
import { defineMessages, FormattedMessage, type MessageDescriptor, useIntl } from "react-intl";

import { workspaceHost } from "@/lib/brand";
import { cn } from "@/lib/utils";

import { LogoMark } from "./logo";
import { ProfileMenu } from "./profile-menu";

export const navMessages = defineMessages({
  home: { id: "nav.home", defaultMessage: "Home" },
  inbox: { id: "nav.inbox", defaultMessage: "Inbox" },
  people: { id: "nav.people", defaultMessage: "People" },
  apps: { id: "nav.apps", defaultMessage: "Apps" },
  main: { id: "nav.main", defaultMessage: "Main" },
  skip: { id: "nav.skipToContent", defaultMessage: "Skip to content" },
});

interface NavItem {
  to: "/" | "/inbox" | "/people" | "/apps";
  label: MessageDescriptor;
  icon: LucideIcon;
}

const items: NavItem[] = [
  { to: "/", label: navMessages.home, icon: House },
  { to: "/inbox", label: navMessages.inbox, icon: Inbox },
  { to: "/people", label: navMessages.people, icon: Users },
  { to: "/apps", label: navMessages.apps, icon: LayoutGrid },
];

function Workspace() {
  return (
    <span className="flex min-w-0 items-center gap-2.5">
      <LogoMark className="size-7" />
      <span className="truncate font-semibold text-sm">{workspaceHost()}</span>
    </span>
  );
}

/**
 * The signed-in frame: a sidebar on wide screens; a top bar and a bottom tab
 * bar on phones, like the mobile app.
 */
export function AppShell() {
  const intl = useIntl();
  return (
    <div className="min-h-dvh bg-canvas text-canvas-foreground md:grid md:grid-cols-[15rem_minmax(0,1fr)]">
      <a
        href="#main"
        className="sr-only z-50 rounded-md bg-primary px-3 py-2 font-medium text-primary-foreground text-sm focus:not-sr-only focus:fixed focus:top-3 focus:left-3"
      >
        <FormattedMessage {...navMessages.skip} />
      </a>

      <aside className="sticky top-0 hidden h-dvh flex-col gap-6 px-3 py-4 md:flex">
        <div className="px-2 pt-1">
          <Workspace />
        </div>
        <nav aria-label={intl.formatMessage(navMessages.main)} className="flex flex-col gap-0.5">
          {items.map(({ to, label, icon: Icon }) => (
            <Link
              key={to}
              to={to}
              activeOptions={{ exact: to === "/" }}
              className={cn(
                "flex items-center gap-3 rounded-lg px-2.5 py-2 font-medium text-[14px] text-muted-foreground outline-none transition-colors",
                "hover:bg-accent hover:text-accent-foreground focus-visible:ring-2 focus-visible:ring-ring",
                "data-[status=active]:bg-primary-subtle data-[status=active]:text-primary-subtle-foreground",
              )}
            >
              <Icon className="size-[18px]" aria-hidden="true" strokeWidth={1.9} />
              <FormattedMessage {...label} />
            </Link>
          ))}
        </nav>
        <div className="mt-auto">
          <ProfileMenu variant="sidebar" />
        </div>
      </aside>

      <header className="sticky top-0 z-30 flex h-14 items-center justify-between gap-3 border-b bg-canvas/90 px-4 backdrop-blur-md md:hidden">
        <Workspace />
        <ProfileMenu variant="compact" />
      </header>

      <main
        id="main"
        tabIndex={-1}
        className="min-h-[calc(100dvh-3.5rem)] bg-background pb-[calc(4.5rem+env(safe-area-inset-bottom))] outline-none md:my-2 md:mr-2 md:min-h-[calc(100dvh-1rem)] md:rounded-xl md:border md:pb-0 md:shadow-xs"
      >
        <Outlet />
      </main>

      <nav
        aria-label={intl.formatMessage(navMessages.main)}
        className="fixed inset-x-0 bottom-0 z-30 grid grid-cols-4 border-t bg-canvas/95 pb-[env(safe-area-inset-bottom)] backdrop-blur-md md:hidden"
      >
        {items.map(({ to, label, icon: Icon }) => (
          <Link
            key={to}
            to={to}
            activeOptions={{ exact: to === "/" }}
            className="group flex flex-col items-center gap-1 pt-2 pb-2.5 font-medium text-[11px] text-muted-foreground outline-none transition-colors focus-visible:bg-accent data-[status=active]:text-primary"
          >
            <span className="flex h-7 w-14 items-center justify-center rounded-full transition-colors group-data-[status=active]:bg-primary-subtle">
              <Icon className="size-5" aria-hidden="true" strokeWidth={1.9} />
            </span>
            <FormattedMessage {...label} />
          </Link>
        ))}
      </nav>
    </div>
  );
}
