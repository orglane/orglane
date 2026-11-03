import { Monitor, Moon, Sun } from "lucide-react";
import { defineMessage, defineMessages, useIntl } from "react-intl";

import { type ThemeChoice, useTheme } from "@/lib/theme";

import { Button } from "./ui/button";

export const themeMessages = defineMessages({
  theme: { id: "theme.label", defaultMessage: "Theme" },
  system: { id: "theme.system", defaultMessage: "System" },
  light: { id: "theme.light", defaultMessage: "Light" },
  dark: { id: "theme.dark", defaultMessage: "Dark" },
  toggle: defineMessage<{ theme: string }>({
    id: "theme.toggle",
    defaultMessage:
      "Theme: {theme, select, light {light} dark {dark} other {system}}. Change theme",
  }),
});

export const themeIcons: Record<ThemeChoice, typeof Sun> = {
  system: Monitor,
  light: Sun,
  dark: Moon,
};

const order: ThemeChoice[] = ["system", "light", "dark"];

/** Cycles system → light → dark; used on screens without the profile menu. */
export function ThemeToggle() {
  const intl = useIntl();
  const { choice, setChoice } = useTheme();
  const Icon = themeIcons[choice];
  return (
    <Button
      variant="ghost"
      size="icon"
      className="text-muted-foreground"
      aria-label={intl.formatMessage(themeMessages.toggle, { theme: choice })}
      onClick={() => setChoice(order[(order.indexOf(choice) + 1) % order.length] ?? "system")}
    >
      <Icon />
    </Button>
  );
}
