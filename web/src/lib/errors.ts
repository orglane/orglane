import { defineMessage, defineMessages, type IntlShape, type MessageDescriptor } from "react-intl";

import { ApiError, NetworkError } from "./api";

const messages = defineMessages({
  network: {
    id: "error.network",
    defaultMessage: "Can't reach the server. Check your connection and try again.",
  },
  rateLimited: defineMessage<{ minutes: number }>({
    id: "error.rateLimited",
    defaultMessage:
      "Too many attempts. Try again {minutes, plural, one {in a minute} other {in # minutes}}.",
  }),
  server: {
    id: "error.server",
    defaultMessage: "Something went wrong on our side. Try again in a moment.",
  },
  forbidden: {
    id: "error.forbidden",
    defaultMessage: "You don't have permission to do that.",
  },
  sessionExpired: {
    id: "error.sessionExpired",
    defaultMessage: "Your session has ended. Sign in again to continue.",
  },
  unknown: {
    id: "error.unknown",
    defaultMessage: "Something went wrong. Try again.",
  },
});

/**
 * Turns a failure into a sentence for people. Server problem details are in
 * English, so the UI words errors itself by status; `byStatus` overrides
 * the wording for a screen (401 on sign-in means a wrong password, not an
 * expired session).
 */
export function errorMessage(
  intl: IntlShape,
  err: unknown,
  byStatus: Partial<Record<number, MessageDescriptor>> = {},
): string {
  if (err instanceof NetworkError) {
    return intl.formatMessage(messages.network);
  }
  if (!(err instanceof ApiError)) {
    return intl.formatMessage(messages.unknown);
  }
  const custom = byStatus[err.status];
  if (custom) {
    return intl.formatMessage(custom);
  }
  switch (err.status) {
    case 401:
      return intl.formatMessage(messages.sessionExpired);
    case 403:
      return intl.formatMessage(messages.forbidden);
    case 429:
      return intl.formatMessage(messages.rateLimited, {
        minutes: Math.max(1, Math.ceil((err.retryAfter ?? 60) / 60)),
      });
    default:
      return intl.formatMessage(err.status >= 500 ? messages.server : messages.unknown);
  }
}
