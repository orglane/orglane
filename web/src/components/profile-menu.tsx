import { Link } from "@tanstack/react-router";
import { ChevronsUpDown, Languages, LogOut, ShieldCheck } from "lucide-react";
import { defineMessage, defineMessages, FormattedMessage, useIntl } from "react-intl";
import { languageName, useLocale } from "@/i18n";
import { fieldMessages } from "@/lib/profile-fields";
import { useMe, useSignOut } from "@/lib/session";
import { type ThemeChoice, useTheme } from "@/lib/theme";
import { cn } from "@/lib/utils";

import { themeIcons, themeMessages } from "./theme-toggle";
import { Avatar } from "./ui/avatar";
import {
  DropdownMenu,
  DropdownMenuContent,
  DropdownMenuGroup,
  DropdownMenuItem,
  DropdownMenuLabel,
  DropdownMenuRadioGroup,
  DropdownMenuRadioItem,
  DropdownMenuSeparator,
  DropdownMenuSub,
  DropdownMenuSubContent,
  DropdownMenuSubTrigger,
  DropdownMenuTrigger,
} from "./ui/dropdown-menu";

const messages = defineMessages({
  open: defineMessage<{ name: string }>({
    id: "profileMenu.open",
    defaultMessage: "Account menu for {name}",
  }),
  security: { id: "profileMenu.security", defaultMessage: "Sign-in & security" },
  email: { id: "profileMenu.email", defaultMessage: "System email" },
  language: { id: "profileMenu.language", defaultMessage: "Language" },
  signOut: { id: "profileMenu.signOut", defaultMessage: "Sign out" },
});

const themes: ThemeChoice[] = ["system", "light", "dark"];

/**
 * The account menu: who is signed in, security settings, theme, language
 * and sign-out. `sidebar` shows name and email next to the avatar.
 */
export function ProfileMenu({ variant }: { variant: "sidebar" | "compact" }) {
  const intl = useIntl();
  const me = useMe();
  const theme = useTheme();
  const locale = useLocale();
  const signOut = useSignOut();
  const { displayName, workEmail, id } = me.person;

  return (
    <DropdownMenu>
      <DropdownMenuTrigger
        aria-label={intl.formatMessage(messages.open, { name: displayName })}
        className={cn(
          "group flex cursor-pointer items-center gap-2.5 rounded-lg text-left outline-none transition-colors focus-visible:ring-2 focus-visible:ring-ring data-[state=open]:bg-accent",
          variant === "sidebar"
            ? "w-full p-2 hover:bg-accent"
            : "rounded-full p-0.5 hover:bg-accent",
        )}
      >
        <Avatar name={displayName} seed={id} />
        {variant === "sidebar" && (
          <>
            <span className="min-w-0 flex-1">
              <span className="block truncate font-medium text-sm">{displayName}</span>
              <span className="block truncate text-muted-foreground text-xs">{workEmail}</span>
            </span>
            <ChevronsUpDown className="size-4 shrink-0 text-muted-foreground" aria-hidden="true" />
          </>
        )}
      </DropdownMenuTrigger>
      <DropdownMenuContent
        align={variant === "sidebar" ? "start" : "end"}
        side={variant === "sidebar" ? "top" : "bottom"}
        className="w-64"
      >
        <DropdownMenuLabel className="flex items-center gap-2.5">
          <Avatar name={displayName} seed={id} />
          <span className="min-w-0">
            <span className="block truncate font-medium">{displayName}</span>
            <span className="block truncate font-normal text-muted-foreground text-xs">
              {workEmail}
            </span>
          </span>
        </DropdownMenuLabel>
        <DropdownMenuSeparator />
        <DropdownMenuGroup>
          <DropdownMenuItem asChild>
            <Link to="/people/$personId" params={{ personId: id }}>
              <FormattedMessage {...fieldMessages.myProfile} />
            </Link>
          </DropdownMenuItem>
          {me.permissions.includes("people.manage") && (
            <DropdownMenuItem asChild>
              <Link to="/settings/profile-fields">
                <FormattedMessage {...fieldMessages.title} />
              </Link>
            </DropdownMenuItem>
          )}
          <DropdownMenuItem asChild>
            <Link to="/settings/security">
              <ShieldCheck />
              <FormattedMessage {...messages.security} />
            </Link>
          </DropdownMenuItem>
          {me.permissions.includes("instance.manage") && (
            <DropdownMenuItem asChild>
              <Link to="/settings/email">
                <FormattedMessage {...messages.email} />
              </Link>
            </DropdownMenuItem>
          )}
          <DropdownMenuSub>
            <DropdownMenuSubTrigger>
              {(() => {
                const Icon = themeIcons[theme.choice];
                return <Icon />;
              })()}
              <FormattedMessage {...themeMessages.theme} />
            </DropdownMenuSubTrigger>
            <DropdownMenuSubContent>
              <DropdownMenuRadioGroup
                value={theme.choice}
                onValueChange={(v) => theme.setChoice(v as ThemeChoice)}
              >
                {themes.map((t) => {
                  const Icon = themeIcons[t];
                  return (
                    <DropdownMenuRadioItem key={t} value={t}>
                      <Icon />
                      <FormattedMessage {...themeMessages[t]} />
                    </DropdownMenuRadioItem>
                  );
                })}
              </DropdownMenuRadioGroup>
            </DropdownMenuSubContent>
          </DropdownMenuSub>
          <DropdownMenuSub>
            <DropdownMenuSubTrigger>
              <Languages />
              <FormattedMessage {...messages.language} />
            </DropdownMenuSubTrigger>
            <DropdownMenuSubContent>
              <DropdownMenuRadioGroup
                value={locale.locale}
                onValueChange={(v) => locale.setLocale(v)}
              >
                {locale.available.map((l) => (
                  <DropdownMenuRadioItem key={l} value={l} lang={l}>
                    {languageName(l)}
                  </DropdownMenuRadioItem>
                ))}
              </DropdownMenuRadioGroup>
            </DropdownMenuSubContent>
          </DropdownMenuSub>
        </DropdownMenuGroup>
        <DropdownMenuSeparator />
        <DropdownMenuItem
          variant="destructive"
          disabled={signOut.isPending}
          onSelect={() => signOut.mutate()}
        >
          <LogOut />
          <FormattedMessage {...messages.signOut} />
        </DropdownMenuItem>
      </DropdownMenuContent>
    </DropdownMenu>
  );
}
